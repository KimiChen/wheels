// SPDX-License-Identifier: Apache-2.0

// Package transport 实现 agent 到 monitor 的独立 WSS 通道（根 README §5）。
//
// 通道不经 FRP 隧道，不修改 FRP wire protocol；认证在 HTTP 握手期完成。
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// MaxFrameSize 为帧大小上限（字节），与服务端一致。
const MaxFrameSize = 256 * 1024

// 默认读写超时。
const (
	DialTimeout  = 10 * time.Second
	WriteTimeout = 10 * time.Second
)

// AuthError 表示握手期认证失败（HTTP 401/403）。
// 调用方必须据此切换到长退避，不得高频重试。
type AuthError struct {
	StatusCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("monitor 认证失败（HTTP %d）", e.StatusCode)
}

// IsAuth 判断错误是否为认证失败。
func IsAuth(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// Conn 为一条已认证的监控 WSS 连接。写操作串行化；读操作单读 pump 使用。
type Conn struct {
	ws *websocket.Conn

	writeMu sync.Mutex
}

// Dial 建立连接并完成 HTTP 握手认证。rawURL 支持 wss://；
// ws:// 仅允许回环地址（本地开发），其余一律拒绝。
func Dial(ctx context.Context, rawURL string, token string) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("非法监控地址：%w", err)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		host := u.Hostname()
		if ip := net.ParseIP(host); !(ip != nil && ip.IsLoopback()) &&
			!strings.EqualFold(host, "localhost") {
			return nil, fmt.Errorf("ws:// 仅允许回环地址，远程节点必须使用 wss://：%s", rawURL)
		}
	default:
		return nil, fmt.Errorf("监控地址 scheme 必须为 wss 或 ws：%s", rawURL)
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: DialTimeout,
		// 远程 agent 必须校验证书：不设置 InsecureSkipVerify。
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	header := http.Header{"Authorization": []string{"Bearer " + token}}

	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	ws, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden) {
			return nil, &AuthError{StatusCode: resp.StatusCode}
		}
		return nil, fmt.Errorf("连接 monitor 失败：%w", err)
	}
	ws.SetReadLimit(MaxFrameSize)
	return &Conn{ws: ws}, nil
}

// Send 序列化并发送一个 JSON-RPC 消息，带写超时与串行化。
func (c *Conn) Send(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(WriteTimeout))
	return c.ws.WriteJSON(v)
}

// ReadMessage 读取下一个文本帧。读方必须是单一 goroutine。
func (c *Conn) ReadMessage() ([]byte, error) {
	_, data, err := c.ws.ReadMessage()
	return data, err
}

// SetReadDeadline 设置读超时（读 pump 保活使用）。
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// Close 关闭底层连接。
func (c *Conn) Close() error {
	return c.ws.Close()
}
