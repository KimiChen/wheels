// SPDX-License-Identifier: Apache-2.0

package probe_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/agent/probe"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// runOnce 以最小合法间隔跑真实探测，等待首个结果。
func runOnce(t *testing.T, target string) protocol.PingResult {
	t.Helper()
	m := probe.NewManager()
	err := m.Apply(protocol.PingTasksParams{Version: 1, Tasks: []protocol.PingTask{
		{ID: "loop", Target: target, Interval: protocol.MinPingInterval},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan protocol.PingResult, 4)
	go m.Run(ctx, func(r protocol.PingResult) { results <- r })
	select {
	case r := <-results:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("超时未收到探测结果")
		return protocol.PingResult{}
	}
}

func TestLoopbackProbeSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	r := runOnce(t, ln.Addr().String())
	if r.TaskID != "loop" {
		t.Fatalf("任务 ID 不符：%+v", r)
	}
	if r.LatencyMS < 0 {
		t.Fatalf("回环握手须成功（latency>=0），得到 %d", r.LatencyMS)
	}
}

func TestLoopbackProbeRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 关闭后连接被拒绝
	r := runOnce(t, addr)
	if r.LatencyMS != protocol.LatencyFailed {
		t.Fatalf("拒绝须上报 -1，得到 %d", r.LatencyMS)
	}
}
