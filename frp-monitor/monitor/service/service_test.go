// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCreds(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.json")
	token := "svc-token"
	sum := sha256.Sum256([]byte(token))
	body := fmt.Sprintf(`{"nodes":[{"id":"node-1","token_sha256":%q}]}`,
		hex.EncodeToString(sum[:]))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, token
}

func TestStartDisabled(t *testing.T) {
	if err := Start(context.Background(), Config{Enable: false}, nil); err != nil {
		t.Fatalf("禁用时 Start 应返回 nil：%v", err)
	}
}

func TestStartMissingCredentials(t *testing.T) {
	err := Start(context.Background(), Config{Enable: true, Addr: "127.0.0.1:0"}, nil)
	if err == nil {
		t.Fatal("缺少凭据文件应返回 error")
	}
	err = Start(context.Background(), Config{
		Enable: true, Addr: "127.0.0.1:0",
		CredentialsFile: filepath.Join(t.TempDir(), "missing.json"),
	}, nil)
	if err == nil {
		t.Fatal("凭据文件不可读应返回 error")
	}
}

func TestStartBindFailure(t *testing.T) {
	creds, _ := writeCreds(t)
	// 先占用一个端口，再让 monitor 绑定同一地址
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Start(ctx, Config{Enable: true, Addr: "127.0.0.1:0", CredentialsFile: creds}, nil); err != nil {
		t.Fatal(err)
	}
	// 非法地址
	if err := Start(ctx, Config{Enable: true, Addr: "127.0.0.1:-1", CredentialsFile: creds}, nil); err == nil {
		t.Fatal("绑定失败应返回 error")
	}
}

type fakeRegistry struct{ clients []FRPClientInfo }

func (f *fakeRegistry) ListClients() []FRPClientInfo { return f.clients }

func TestStartServeAndShutdown(t *testing.T) {
	creds, _ := writeCreds(t)
	ctx, cancel := context.WithCancel(context.Background())

	// 用占位端口启动后再探测实际端口不可行（Config.Addr 固定），
	// 改为绑定 127.0.0.1:0 前让系统分配：直接 Start 后无法拿到端口，
	// 因此选高端口重试。
	var port int
	for port = 17410; port < 17430; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if err := Start(ctx, Config{
			Enable: true, Addr: addr, CredentialsFile: creds,
		}, &fakeRegistry{clients: []FRPClientInfo{
			{User: "u1", RawClientID: "c1", Online: true},
		}}); err == nil {
			break
		}
		if port == 17429 {
			t.Fatal("无可用端口")
		}
	}

	// API 可用
	url := fmt.Sprintf("http://127.0.0.1:%d/api/public/v1/overview", port)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("overview 应 200，得到 %d", resp.StatusCode)
			}
			if got := string(body); got == "" {
				t.Fatal("overview 响应为空")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("服务未在 3 秒内就绪：%v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 静态占位可用
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(body) == 0 {
		t.Fatalf("占位首页应 200，得到 %d", resp.StatusCode)
	}

	// 优雅关闭：取消后 5 秒内端口释放（再次绑定同地址成功）
	cancel()
	deadline = time.Now().Add(6 * time.Second)
	for {
		ctx2, cancel2 := context.WithCancel(context.Background())
		err := Start(ctx2, Config{Enable: true, Addr: fmt.Sprintf("127.0.0.1:%d", port), CredentialsFile: creds}, nil)
		if err == nil {
			cancel2()
			return
		}
		cancel2()
		if time.Now().After(deadline) {
			t.Fatalf("取消后 6 秒内未释放端口：%v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
