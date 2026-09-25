// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tokenDigest(t *testing.T, token string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeCredentials(t *testing.T, path string, entries ...string) {
	t.Helper()
	body := `{"nodes":[`
	for i, e := range entries {
		if i > 0 {
			body += ","
		}
		body += e
	}
	body += `]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNodeAuthenticateHitMiss(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	writeCredentials(t, path,
		fmt.Sprintf(`{"id":"node-a","token_sha256":%q,"comment":"a"}`, tokenDigest(t, "token-a")),
		fmt.Sprintf(`{"id":"node-b","token_sha256":%q}`, tokenDigest(t, "token-b")),
	)

	a, err := NewNodeAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := a.Authenticate("token-a"); !ok || id != "node-a" {
		t.Fatalf("token-a 应命中 node-a，得到 %q/%v", id, ok)
	}
	if id, ok := a.Authenticate("token-b"); !ok || id != "node-b" {
		t.Fatalf("token-b 应命中 node-b，得到 %q/%v", id, ok)
	}
	if _, ok := a.Authenticate("token-x"); ok {
		t.Fatal("未知 token 不应命中")
	}
	if _, ok := a.Authenticate(""); ok {
		t.Fatal("空 token 不应命中")
	}
}

func TestNodeCredentialsReloadOnMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	writeCredentials(t, path,
		fmt.Sprintf(`{"id":"node-a","token_sha256":%q}`, tokenDigest(t, "token-old")),
	)
	a, err := NewNodeAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate("token-new"); ok {
		t.Fatal("轮换前 token-new 不应命中")
	}

	// 轮换凭据：改写文件并显式推进 mtime（某些文件系统 mtime 粒度较粗）
	writeCredentials(t, path,
		fmt.Sprintf(`{"id":"node-a","token_sha256":%q}`, tokenDigest(t, "token-new")),
	)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if id, ok := a.Authenticate("token-new"); !ok || id != "node-a" {
		t.Fatalf("mtime 变化后应自动重载并命中新 token，得到 %q/%v", id, ok)
	}
	if _, ok := a.Authenticate("token-old"); ok {
		t.Fatal("吊销后旧 token 不应命中")
	}
}

func TestNodeCredentialsReloadFailureKeepsOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	writeCredentials(t, path,
		fmt.Sprintf(`{"id":"node-a","token_sha256":%q}`, tokenDigest(t, "token-a")),
	)
	a, err := NewNodeAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	// 写成非法 JSON 并推进 mtime：重载失败应保留旧数据
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Authenticate("token-a"); !ok {
		t.Fatal("重载失败应保留旧凭据")
	}
}

func TestNewNodeAuthenticatorErrors(t *testing.T) {
	if _, err := NewNodeAuthenticator(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("文件缺失应返回 error")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	writeCredentials(t, bad, `{"id":"x","token_sha256":"zz"}`)
	if _, err := NewNodeAuthenticator(bad); err == nil {
		t.Fatal("非法摘要应返回 error")
	}
}

func TestAdminPassword(t *testing.T) {
	sum := sha256.Sum256([]byte("s3cret"))
	a, err := NewAdmin(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Enabled() {
		t.Fatal("配置摘要后管理端应启用")
	}
	if !a.Authenticate("s3cret") {
		t.Fatal("正确密码应通过")
	}
	if a.Authenticate("wrong") {
		t.Fatal("错误密码不应通过")
	}

	disabled, err := NewAdmin("")
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled() {
		t.Fatal("空摘要应禁用管理端")
	}
	if disabled.Authenticate("s3cret") {
		t.Fatal("禁用状态下任何密码都应失败")
	}

	if _, err := NewAdmin("abcd"); err == nil {
		t.Fatal("非法摘要格式应返回 error")
	}
}

func TestAdminSessionToken(t *testing.T) {
	sum := sha256.Sum256([]byte("s3cret"))
	a, err := NewAdmin(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790380800, 0)
	token, exp := a.IssueToken(now)
	if exp.Unix() != now.Add(AdminSessionTTL).Unix() {
		t.Fatalf("过期时间应为 now+12h，得到 %v", exp)
	}
	if !a.ValidateToken(token, now) {
		t.Fatal("新令牌应有效")
	}
	if !a.ValidateToken(token, now.Add(AdminSessionTTL-time.Second)) {
		t.Fatal("过期前 1 秒应有效")
	}
	if a.ValidateToken(token, now.Add(AdminSessionTTL)) {
		t.Fatal("到达过期时刻应失效")
	}
	if a.ValidateToken(token, now.Add(AdminSessionTTL+time.Hour)) {
		t.Fatal("过期令牌应失效")
	}

	// 篡改签名
	tampered := token[:len(token)-1] + "0"
	if token == tampered {
		tampered = token[:len(token)-1] + "1"
	}
	if a.ValidateToken(tampered, now) {
		t.Fatal("篡改签名应失效")
	}
	// 篡改过期时间
	if a.ValidateToken("9999999999."+token[len(token)-64:], now) {
		t.Fatal("篡改过期时间应失效")
	}
	// 格式非法
	if a.ValidateToken("garbage", now) {
		t.Fatal("非法格式应失效")
	}

	// 重启（更换 key）后旧令牌失效
	a2, err := NewAdmin(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if a2.ValidateToken(token, now) {
		t.Fatal("重启后旧令牌应失效")
	}
}

func TestAdminCookie(t *testing.T) {
	a, err := NewAdmin("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790380800, 0)
	token, exp := a.IssueToken(now)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	a.SetCookie(rec, r, token, exp)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应下发 1 个 Cookie，得到 %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != AdminCookieName || c.Value != token {
		t.Fatalf("Cookie 名/值不符：%+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("Cookie 属性不符：%+v", c)
	}
	if c.Secure {
		t.Fatal("非 TLS 请求不应加 Secure")
	}

	// TLS 请求加 Secure
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	a.SetCookie(rec, r, token, exp)
	if !rec.Result().Cookies()[0].Secure {
		t.Fatal("TLS 请求应加 Secure")
	}
}

func TestOriginAllowed(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://example.com/api/admin/v1/login", nil)
	if !OriginAllowed(r) {
		t.Fatal("无 Origin 应放行")
	}
	r.Header.Set("Origin", "http://example.com")
	if !OriginAllowed(r) {
		t.Fatal("同源 Origin 应放行")
	}
	r.Header.Set("Origin", "http://evil.com")
	if OriginAllowed(r) {
		t.Fatal("跨源 Origin 应拒绝")
	}
	r.Header.Set("Origin", ":::not-a-url")
	if OriginAllowed(r) {
		t.Fatal("非法 Origin 应拒绝")
	}
}
