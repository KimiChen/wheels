// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/web"
)

// 页面 <head> 的防主题闪烁内联脚本以 sha256 白名单进入 CSP；
// 本测试从嵌入资源重算 hash，改动页面脚本后 CSP 不过期。
func TestCSPInlineScriptHashes(t *testing.T) {
	inlineRe := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	scriptSrc := securityHeaders["Content-Security-Policy"]

	matched := 0
	err := fs.WalkDir(web.Static(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		raw, err := fs.ReadFile(web.Static(), path)
		if err != nil {
			return err
		}
		for _, m := range inlineRe.FindAllSubmatch(raw, -1) {
			sum := sha256.Sum256(m[1])
			hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			if !strings.Contains(scriptSrc, hash) {
				t.Errorf("%s 的内联脚本不在 CSP 白名单：%s", path, hash)
			}
			matched++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历嵌入资源失败：%v", err)
	}
	if matched == 0 {
		t.Fatal("未找到任何内联脚本，CSP 白名单失去意义")
	}
}

// 安全响应头实际出现在页面响应上。
func TestSecurityHeadersServed(t *testing.T) {
	h := NewHandler(nil, nil, nil, web.Static())
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("页面响应缺少 CSP 头")
	}
	if rec.Code != 200 {
		t.Errorf("GET / 状态码 = %d，应为 200", rec.Code)
	}
}

// /admin/ 路由形态重定向到 admin.html。
func TestAdminRedirect(t *testing.T) {
	h := NewHandler(nil, nil, nil, web.Static())
	for _, path := range []string{"/admin", "/admin/"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 302 || rec.Header().Get("Location") != "/admin.html" {
			t.Errorf("%s 应 302 到 /admin.html，实际 %d %s", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}
