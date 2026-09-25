// SPDX-License-Identifier: Apache-2.0

// Package auth 实现 monitor 的节点凭据与管理端会话（根 README §4、§7）。
//
// 节点凭据：服务端只保存 token 的 sha256 摘要（JSON 凭据文件），认证为
// Bearer token → sha256 → 常量时间比较；凭据文件按 mtime 变化自动重载，
// 支持轮换/吊销不重启。重载失败保留旧数据，不影响在线节点。
//
// 管理端：密码 sha256 与配置摘要常量时间比较；会话令牌为
// "exp_unix.hex(hmac_sha256(key, "admin:"+exp))"，key 为启动时随机
// 32 字节（重启即失效），有效期 12 小时；Cookie fm_admin 带 HttpOnly、
// SameSite=Strict，TLS 连接上加 Secure。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// AdminCookieName 为管理端会话 Cookie 名。
	AdminCookieName = "fm_admin"
	// AdminSessionTTL 为管理会话有效期（12 小时）。
	AdminSessionTTL = 12 * time.Hour
)

// nodeCredential 为凭据文件中的单条节点凭据。服务端只保存 token 的
// sha256 摘要；created_at 为创建时间（Unix 秒），旧版文件缺失时为 0。
type nodeCredential struct {
	ID          string `json:"id"`
	TokenSHA256 string `json:"token_sha256"`
	Comment     string `json:"comment,omitempty"`
	CreatedAt   int64  `json:"created_at,omitempty"`
}

// nodeCredentialsFile 为节点凭据文件的 JSON 结构。
type nodeCredentialsFile struct {
	Nodes []nodeCredential `json:"nodes"`
}

// maxCredentialIDLen 为凭据 ID 长度上限（字节）。
const maxCredentialIDLen = 64

// 凭据管理错误。ErrCredentialConflict 映射 409，ErrInvalidCredentialID 映射 400。
var (
	ErrCredentialConflict  = errors.New("auth: 节点 id 已存在")
	ErrInvalidCredentialID = errors.New("auth: 节点 id 非法（空、超长或含空白）")
)

// ValidateCredentialID 校验凭据 ID：非空、不超过 64 字节、不含空白。
func ValidateCredentialID(id string) error {
	if id == "" || len(id) > maxCredentialIDLen ||
		strings.IndexFunc(id, unicode.IsSpace) >= 0 {
		return ErrInvalidCredentialID
	}
	return nil
}

// CredentialInfo 为凭据的管理视图（不含摘要）。
type CredentialInfo struct {
	ID        string
	Comment   string
	CreatedAt int64
}

// NodeAuthenticator 校验节点 Bearer token。并发安全。
type NodeAuthenticator struct {
	path string

	mu      sync.RWMutex
	modTime time.Time
	entries []nodeCredential  // 凭据文件全量条目（管理写回时保留 comment/created_at）
	digests map[string][]byte // nodeID → token sha256 摘要（32 字节）
}

// NewNodeAuthenticator 加载凭据文件；文件缺失或格式非法返回 error
// （启动期失败由 service 层上抛，阻止监控启动）。
func NewNodeAuthenticator(path string) (*NodeAuthenticator, error) {
	a := &NodeAuthenticator{path: path}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *NodeAuthenticator) load() error {
	st, err := os.Stat(a.path)
	if err != nil {
		return fmt.Errorf("auth: 凭据文件不可读：%w", err)
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return fmt.Errorf("auth: 凭据文件读取失败：%w", err)
	}
	var f nodeCredentialsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("auth: 凭据文件 JSON 非法：%w", err)
	}
	digests := make(map[string][]byte, len(f.Nodes))
	for i, n := range f.Nodes {
		if n.ID == "" {
			return fmt.Errorf("auth: nodes[%d].id 为空", i)
		}
		digest, err := hex.DecodeString(n.TokenSHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("auth: 节点 %s 的 token_sha256 不是 64 位十六进制", n.ID)
		}
		if _, dup := digests[n.ID]; dup {
			return fmt.Errorf("auth: 节点 id 重复：%s", n.ID)
		}
		digests[n.ID] = digest
	}

	a.mu.Lock()
	a.entries = f.Nodes
	a.digests = digests
	a.modTime = st.ModTime()
	a.mu.Unlock()
	return nil
}

// maybeReload 在凭据文件 mtime 变化时重载（轮换/吊销不重启）。
// 重载失败保留旧数据，认证继续可用。
func (a *NodeAuthenticator) maybeReload() {
	st, err := os.Stat(a.path)
	if err != nil {
		return
	}
	a.mu.RLock()
	current := a.modTime
	a.mu.RUnlock()
	if st.ModTime().Equal(current) {
		return
	}
	_ = a.load()
}

// Authenticate 校验 Bearer token：sha256 后与已存摘要做常量时间比较，
// 命中返回节点 ID。节点归属只由凭据决定，不信任正文自填 ID。
func (a *NodeAuthenticator) Authenticate(token string) (nodeID string, ok bool) {
	a.maybeReload()
	sum := sha256.Sum256([]byte(token))
	a.mu.RLock()
	defer a.mu.RUnlock()
	for id, digest := range a.digests {
		if subtle.ConstantTimeCompare(sum[:], digest) == 1 {
			return id, true
		}
	}
	return "", false
}

// List 返回全部凭据的管理视图（按 id 排序，不含摘要）。
func (a *NodeAuthenticator) List() []CredentialInfo {
	a.maybeReload()
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]CredentialInfo, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, CredentialInfo{ID: e.ID, Comment: e.Comment, CreatedAt: e.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Add 生成 32 字节随机 token（hex），把摘要追加进凭据文件并立即生效。
// 明文 token 只经返回值返回一次；id 冲突返回 ErrCredentialConflict，
// id 非法返回 ErrInvalidCredentialID。
func (a *NodeAuthenticator) Add(id, comment string) (token string, err error) {
	if err := ValidateCredentialID(id); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: token 生成失败：%w", err)
	}
	token = hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	a.mu.Lock()
	defer a.mu.Unlock()
	if _, dup := a.digests[id]; dup {
		return "", ErrCredentialConflict
	}
	entries := make([]nodeCredential, len(a.entries), len(a.entries)+1)
	copy(entries, a.entries)
	entries = append(entries, nodeCredential{
		ID: id, TokenSHA256: hex.EncodeToString(sum[:]),
		Comment: comment, CreatedAt: time.Now().Unix(),
	})
	if err := a.saveLocked(entries); err != nil {
		return "", err
	}
	return token, nil
}

// Delete 吊销一个节点凭据并立即生效；id 不存在返回 found=false。
func (a *NodeAuthenticator) Delete(id string) (found bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := -1
	for i, e := range a.entries {
		if e.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	entries := make([]nodeCredential, 0, len(a.entries)-1)
	entries = append(entries, a.entries[:idx]...)
	entries = append(entries, a.entries[idx+1:]...)
	if err := a.saveLocked(entries); err != nil {
		return false, err
	}
	return true, nil
}

// saveLocked 原子写凭据文件（同目录临时文件 + rename，0600）并更新内存
// 条目、摘要与 modTime（避免随后 mtime 重载入旧文件）。调用方须持有 a.mu。
// 写盘失败时内存状态不变（继续用旧凭据）。
func (a *NodeAuthenticator) saveLocked(entries []nodeCredential) error {
	data, err := json.MarshalIndent(nodeCredentialsFile{Nodes: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: 凭据序列化失败：%w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.path), ".nodes-*.tmp")
	if err != nil {
		return fmt.Errorf("auth: 凭据临时文件创建失败：%w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("auth: 凭据临时文件写入失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("auth: 凭据临时文件关闭失败：%w", err)
	}
	if err := os.Rename(tmpPath, a.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("auth: 凭据文件替换失败：%w", err)
	}
	// CreateTemp 即 0600；显式 chmod 兜底umask 之外的场景。
	if err := os.Chmod(a.path, 0o600); err != nil {
		return fmt.Errorf("auth: 凭据文件权限设置失败：%w", err)
	}
	st, err := os.Stat(a.path)
	if err != nil {
		return fmt.Errorf("auth: 凭据文件状态读取失败：%w", err)
	}
	digests := make(map[string][]byte, len(entries))
	for _, e := range entries {
		digest, err := hex.DecodeString(e.TokenSHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("auth: 节点 %s 的 token_sha256 不是 64 位十六进制", e.ID)
		}
		digests[e.ID] = digest
	}
	a.entries = entries
	a.digests = digests
	a.modTime = st.ModTime()
	return nil
}

// Admin 为管理端认证。passwordHash 为空表示管理端禁用（任何登录都失败，
// 会话校验同样失败）。
type Admin struct {
	passwordHash []byte // nil = 管理端禁用
	key          []byte // 启动时随机 32 字节，重启即失效
}

// NewAdmin 创建管理端认证。passwordHashHex 为管理密码 sha256 的
// 64 位十六进制小写；空串表示禁用管理端。
func NewAdmin(passwordHashHex string) (*Admin, error) {
	a := &Admin{key: make([]byte, 32)}
	if _, err := rand.Read(a.key); err != nil {
		return nil, fmt.Errorf("auth: 会话密钥生成失败：%w", err)
	}
	if passwordHashHex == "" {
		return a, nil
	}
	hash, err := hex.DecodeString(passwordHashHex)
	if err != nil || len(hash) != sha256.Size {
		return nil, fmt.Errorf("auth: 管理密码摘要必须是 64 位十六进制")
	}
	a.passwordHash = hash
	return a, nil
}

// Enabled 报告管理端是否启用。
func (a *Admin) Enabled() bool {
	return a.passwordHash != nil
}

// Authenticate 校验管理密码（常量时间比较）。
func (a *Admin) Authenticate(password string) bool {
	if a.passwordHash == nil {
		return false
	}
	sum := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(sum[:], a.passwordHash) == 1
}

// IssueToken 签发管理会话令牌，返回令牌串与过期时间。
func (a *Admin) IssueToken(now time.Time) (token string, expires time.Time) {
	expires = now.Add(AdminSessionTTL)
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte("admin:" + exp))
	return exp + "." + hex.EncodeToString(mac.Sum(nil)), expires
}

// ValidateToken 校验会话令牌：格式、过期时间与 HMAC 签名（常量时间比较）。
func (a *Admin) ValidateToken(token string, now time.Time) bool {
	exp, sig, found := strings.Cut(token, ".")
	if !found || exp == "" || sig == "" {
		return false
	}
	expUnix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return false
	}
	if now.Unix() >= expUnix {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte("admin:" + exp))
	return subtle.ConstantTimeCompare(got, mac.Sum(nil)) == 1
}

// SetCookie 下发管理会话 Cookie：HttpOnly、SameSite=Strict、Path=/，
// TLS 连接上加 Secure。
func (a *Admin) SetCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie 清除管理会话 Cookie（登录态属性须与 SetCookie 一致）。
func (a *Admin) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     AdminCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

// SessionValid 校验请求携带的管理会话 Cookie。
func (a *Admin) SessionValid(r *http.Request, now time.Time) bool {
	if a.passwordHash == nil {
		return false
	}
	c, err := r.Cookie(AdminCookieName)
	if err != nil {
		return false
	}
	return a.ValidateToken(c.Value, now)
}

// OriginAllowed 校验管理写操作的 Origin：Origin 头存在时其 host 必须
// 等于请求 Host（CSRF 防护）；无 Origin 头（非浏览器客户端）放行。
func OriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}
