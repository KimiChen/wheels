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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AdminCookieName 为管理端会话 Cookie 名。
	AdminCookieName = "fm_admin"
	// AdminSessionTTL 为管理会话有效期（12 小时）。
	AdminSessionTTL = 12 * time.Hour
)

// nodeCredentialsFile 为节点凭据文件的 JSON 结构。
type nodeCredentialsFile struct {
	Nodes []struct {
		ID          string `json:"id"`
		TokenSHA256 string `json:"token_sha256"`
		Comment     string `json:"comment"`
	} `json:"nodes"`
}

// NodeAuthenticator 校验节点 Bearer token。并发安全。
type NodeAuthenticator struct {
	path string

	mu      sync.RWMutex
	modTime time.Time
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
