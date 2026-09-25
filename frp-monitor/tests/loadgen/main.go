// SPDX-License-Identifier: Apache-2.0

// loadgen 为 frp-monitor 容量测试工具：模拟 N 个节点对 monitor 建立监控会话，
// 按周期上报合成指标，统计连接与上报成功率。仅用于容量测试档位验证，
// 不是性能承诺。随 extension/frpmonitor/tests/loadgen 映射进上游树编译。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fatedier/frp/extension/frpmonitor/shared/metrics"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
	"github.com/fatedier/frp/extension/frpmonitor/shared/version"
)

func main() {
	var (
		serverURL   = flag.String("server", "", "监控地址，如 ws://127.0.0.1:7400/agent/v1/ws")
		nodes       = flag.Int("nodes", 100, "模拟节点数")
		tokenPrefix = flag.String("token-prefix", "loadtest-token-", "节点 token 前缀（token=前缀+序号）")
		interval    = flag.Duration("interval", time.Second, "上报间隔")
		hold        = flag.Duration("hold", 15*time.Second, "全连接后保持时长")
	)
	flag.Parse()
	if *serverURL == "" {
		fmt.Fprintln(os.Stderr, "缺少 -server")
		os.Exit(2)
	}

	var connected, reports, reportErrs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	for i := 0; i < *nodes; i++ {
		go runNode(ctx, *serverURL, fmt.Sprintf("%s%d", *tokenPrefix, i),
			fmt.Sprintf("loadgen-%d", i), *interval, &connected, &reports, &reportErrs)
	}
	// 交错建立连接，避免瞬时齐射
	time.Sleep(time.Duration(*nodes) * 5 * time.Millisecond)

	fmt.Printf("loadgen：%d 节点，%v 上报间隔，保持 %v\n", *nodes, *interval, *hold)
	time.Sleep(*hold)
	// 在保持窗口结束时读数；cancel 后节点陆续断开，事后读数必然归零。
	finalConnected := connected.Load()
	cancel()
	time.Sleep(time.Second)

	fmt.Printf("结果：已连接 %d/%d，上报成功 %d，失败 %d，总耗时 %v\n",
		finalConnected, *nodes, reports.Load(), reportErrs.Load(), time.Since(start).Round(time.Millisecond))
	if finalConnected < int64(*nodes) {
		fmt.Fprintln(os.Stderr, "存在未连接节点")
		os.Exit(1)
	}
	if total := reports.Load() + reportErrs.Load(); total > 0 && reportErrs.Load()*100 > total {
		fmt.Fprintln(os.Stderr, "上报失败率超过 1%")
		os.Exit(1)
	}
}

// runNode 模拟单个节点的 连接→hello→周期report 生命周期；连接断开即退出
// （容量测试不模拟重连风暴，重连语义由端到端冒烟覆盖）。
func runNode(ctx context.Context, serverURL, token, sessionID string, interval time.Duration,
	connected, reports, reportErrs *atomic.Int64,
) {
	dialer := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	ws, _, err := dialer.DialContext(ctx, serverURL, header)
	if err != nil {
		reportErrs.Add(1)
		return
	}
	defer ws.Close()

	hello, _ := protocol.NewRequest(nil, protocol.MethodHello, &protocol.HelloParams{
		SchemaVersion:  protocol.SchemaVersion,
		AgentVersion:   version.Version + "-loadgen",
		FRPVersion:     "loadgen",
		Capabilities:   []string{"metrics"},
		SessionID:      sessionID,
		ReportInterval: int(interval / time.Second),
		SentAt:         time.Now().Unix(),
	})
	if err := ws.WriteJSON(hello); err != nil {
		reportErrs.Add(1)
		return
	}
	connected.Add(1)
	defer connected.Add(-1)

	var seq uint64
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		seq++
		m := &metrics.Metrics{
			CollectedAt: time.Now().Unix(),
			CPU:         10, Load: [3]float64{0.1, 0.2, 0.3},
			MemUsed: 1 << 30, MemTotal: 4 << 30,
			SwapTotal: 1 << 30, DiskUsed: 10 << 30, DiskTotal: 100 << 30,
			NetRX: 1024, NetTX: 2048,
			NetRXTotal: 1 << 40, NetTXTotal: 1 << 39,
			BootID: "loadgen-boot", Iface: "eth0",
			Uptime: 3600, TCP: 59, UDP: 14, Procs: 100,
		}
		req, _ := protocol.NewRequest(nil, protocol.MethodReport, &protocol.ReportParams{
			SessionID: sessionID,
			Sequence:  seq,
			SentAt:    time.Now().Unix(),
			Metrics:   m,
		})
		if err := ws.WriteJSON(req); err != nil {
			reportErrs.Add(1)
			return
		}
		reports.Add(1)
	}
}
