// SPDX-License-Identifier: Apache-2.0

// Package service 是 monitor 监控服务的对外唯一入口（根 README §1、§6）。
//
// Start 组合凭据认证、内存状态、WSS 接收与 API/SSE，绑定监听后即刻返回，
// 服务在 goroutine 中运行；ctx 取消时 5 秒内优雅关闭。绑定或凭据加载失败
// 返回 error；运行期错误只记日志并降级，绝不拖停调用方（frps）。
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	frpversion "github.com/fatedier/frp/pkg/util/version"

	"github.com/fatedier/frp/extension/frpmonitor/monitor/api"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/auth"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/ingest"
	"github.com/fatedier/frp/extension/frpmonitor/monitor/store"
	"github.com/fatedier/frp/extension/frpmonitor/web"

	// 引用即触发版本后缀注入（frpversion.Full() 附带 frp-monitor 版本）。
	_ "github.com/fatedier/frp/extension/frpmonitor/shared/version"
)

// defaultAddr 为监控 Listener 默认地址（回环，由反向代理发布 HTTPS）。
const defaultAddr = "127.0.0.1:7400"

// registryPollInterval 为 FRP 注册表快照的轮询周期。
const registryPollInterval = 2 * time.Second

// shutdownTimeout 为 ctx 取消后的优雅关闭时限。
const shutdownTimeout = 5 * time.Second

// Config 为监控服务配置。
type Config struct {
	Enable            bool
	Addr              string // 默认 "127.0.0.1:7400"
	CredentialsFile   string // 节点凭据文件（JSON），必填
	AdminPasswordHash string // 管理密码 sha256 十六进制小写；空 = 管理端禁用
}

// FRPClientInfo 为 frps 注册表条目的只读快照。
type FRPClientInfo struct {
	User, RawClientID, RunID, Version string
	Online                            bool
	FirstConnectedAt, LastConnectedAt int64 // Unix 秒
}

// FRPRegistrySource 为 frps Registry 的只读适配器（由上层胶水实现）。
type FRPRegistrySource interface {
	ListClients() []FRPClientInfo
}

// Start 启动监控服务：绑定监听并即刻返回；服务在 goroutine 中运行，
// ctx 取消时 5 秒内优雅关闭。绑定/凭据加载失败返回 error；运行期错误
// 只记日志并降级，绝不拖停调用方（frps）。
func Start(ctx context.Context, cfg Config, src FRPRegistrySource) error {
	if !cfg.Enable {
		return nil
	}
	addr := cfg.Addr
	if addr == "" {
		addr = defaultAddr
	}
	if cfg.CredentialsFile == "" {
		return errors.New("frpmonitor monitor: CredentialsFile 必填")
	}
	nodeAuth, err := auth.NewNodeAuthenticator(cfg.CredentialsFile)
	if err != nil {
		return fmt.Errorf("frpmonitor monitor: 节点凭据加载失败：%w", err)
	}
	adminAuth, err := auth.NewAdmin(cfg.AdminPasswordHash)
	if err != nil {
		return fmt.Errorf("frpmonitor monitor: 管理端配置非法：%w", err)
	}

	st := store.New()
	ingestHandler := ingest.NewHandler(nodeAuth, st, serverVersion())
	apiHandler := api.NewHandler(st, adminAuth, web.Static())

	mux := http.NewServeMux()
	mux.Handle("/agent/v1/ws", ingestHandler)
	mux.Handle("/", apiHandler)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("frpmonitor monitor: 监听 %s 失败：%w", addr, err)
	}

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("frpmonitor monitor: HTTP 服务异常退出：%v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("frpmonitor monitor: 优雅关闭失败：%v", err)
		}
	}()
	if src != nil {
		go pollRegistry(ctx, src, st)
	}
	return nil
}

// serverVersion 填入 hello 应答：FRP 基线版本 + 扩展版本
// （shared/version 的 init 已把后缀注入 frpversion.Full()）。
func serverVersion() string {
	return frpversion.Full()
}

// pollRegistry 周期拉取 FRP 注册表快照喂入 store。src  panic 等运行期
// 错误只记日志，保证 frps 转发不受影响。
func pollRegistry(ctx context.Context, src FRPRegistrySource, st *store.Store) {
	ticker := time.NewTicker(registryPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("frpmonitor monitor: FRP 注册表快照 panic（已降级）：%v", r)
					}
				}()
				infos := src.ListClients()
				clients := make([]store.FRPClient, 0, len(infos))
				for _, in := range infos {
					clients = append(clients, store.FRPClient{
						User:             in.User,
						ClientID:         in.RawClientID,
						RunID:            in.RunID,
						Version:          in.Version,
						Online:           in.Online,
						FirstConnectedAt: in.FirstConnectedAt,
						LastConnectedAt:  in.LastConnectedAt,
					})
				}
				st.UpdateFRPClients(clients)
			}()
		}
	}
}
