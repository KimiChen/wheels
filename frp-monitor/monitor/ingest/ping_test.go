// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

// frameReader 在 goroutine 中持续读帧并投递到 channel（读超时断言用，
// 避免 gorilla 读超时损坏连接状态）。
type frameReader struct {
	ch chan map[string]json.RawMessage
}

func startReader(c *websocket.Conn) *frameReader {
	r := &frameReader{ch: make(chan map[string]json.RawMessage, 64)}
	go func() {
		defer close(r.ch)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			r.ch <- m
		}
	}()
	return r
}

// next 读取下一帧；timeout 内无帧返回 ok=false（不断言失败，供「不应收到」用）。
func (r *frameReader) next(timeout time.Duration) (map[string]json.RawMessage, bool) {
	select {
	case m, ok := <-r.ch:
		if !ok {
			return nil, false
		}
		return m, true
	case <-time.After(timeout):
		return nil, false
	}
}

// mustNext 读取下一帧，超时则失败。
func (r *frameReader) mustNext(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	m, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("5 秒内未收到预期帧")
	}
	return m
}

// expectRPCError 断言下一帧为指定错误码的应答。
func (r *frameReader) expectRPCError(t *testing.T, code int) {
	t.Helper()
	m := r.mustNext(t)
	var resp protocol.Response
	raw, ok := m["result"]
	hasResult := ok && string(raw) != "null" && len(raw) > 0
	b, _ := json.Marshal(m)
	if hasResult {
		t.Fatalf("应为错误应答：%s", b)
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("错误码应为 %d：%s", code, b)
	}
}

// expectRPCOK 断言下一帧为成功应答。
func (r *frameReader) expectRPCOK(t *testing.T) {
	t.Helper()
	m := r.mustNext(t)
	if raw, ok := m["error"]; ok && string(raw) != "null" && len(raw) > 0 {
		t.Fatalf("应为成功应答：%s", raw)
	}
}

// helloOK 完成 hello 握手。
func helloOK(t *testing.T, c *websocket.Conn, r *frameReader, sessionID string, caps []string) {
	t.Helper()
	h := helloParams(sessionID, protocol.SchemaVersion)
	h.Capabilities = caps
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 1, protocol.MethodHello, h)); err != nil {
		t.Fatal(err)
	}
	r.expectRPCOK(t)
}

// readPingTasks 断言下一帧为 ping.tasks 请求，返回请求 ID、版本与任务。
func readPingTasks(t *testing.T, r *frameReader) (reqID uint64, version uint64, tasks []protocol.PingTask) {
	t.Helper()
	frame := r.mustNext(t)
	var method string
	if err := json.Unmarshal(frame["method"], &method); err != nil || method != protocol.MethodPingTasks {
		t.Fatalf("应为 ping.tasks 请求：%v", frame)
	}
	if err := json.Unmarshal(frame["id"], &reqID); err != nil {
		t.Fatal(err)
	}
	var p protocol.PingTasksParams
	if err := json.Unmarshal(frame["params"], &p); err != nil {
		t.Fatal(err)
	}
	return reqID, p.Version, p.Tasks
}

// ackRequest 回一条成功应答。
func ackRequest(t *testing.T, c *websocket.Conn, id uint64) {
	t.Helper()
	resp := &protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Result: json.RawMessage(`{}`)}
	if err := c.WriteJSON(resp); err != nil {
		t.Fatal(err)
	}
}

// TestPingDispatchAckResultLoop 验证完整闭环：hello（含 ping 能力）→
// 下发任务 → ack → ping.result → 滑窗统计。
func TestPingDispatchAckResultLoop(t *testing.T) {
	srv, st, token := setup(t)

	// 离线期间先配置任务（上线即补发）
	if _, err := st.SetProbeTasks("node-1", []protocol.PingTask{{ID: "t1", Target: "example.com:443", Interval: 5}}); err != nil {
		t.Fatal(err)
	}

	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := startReader(c)
	helloOK(t, c, r, "sess-1", []string{"ping"})

	// hello 后应立即收到当前任务列表
	reqID, version, tasks := readPingTasks(t, r)
	if version != 1 || len(tasks) != 1 || tasks[0].ID != "t1" || tasks[0].Target != "example.com:443" || tasks[0].Interval != 5 {
		t.Fatalf("下发内容不符：v=%d tasks=%+v", version, tasks)
	}

	// ack
	ackRequest(t, c, reqID)
	deadline := time.Now().Add(3 * time.Second)
	for {
		sent, acked := st.ProbeDispatch("node-1")
		if sent == 1 && acked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ack 未被记录：sent=%d acked=%d", sent, acked)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 上报探测结果：一次成功 23ms，一次失败
	mkResult := func(seq uint64, latency int64) *protocol.PingResultParams {
		return &protocol.PingResultParams{
			SessionID: "sess-1", Sequence: seq, SentAt: time.Now().Unix(),
			Results: []protocol.PingResult{{TaskID: "t1", LatencyMS: latency}},
		}
	}
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 2, protocol.MethodPingResult, mkResult(1, 23))); err != nil {
		t.Fatal(err)
	}
	r.expectRPCOK(t)
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 3, protocol.MethodPingResult, mkResult(2, protocol.LatencyFailed))); err != nil {
		t.Fatal(err)
	}
	r.expectRPCOK(t)

	stats := st.ProbeStats("node-1", time.Now())
	if len(stats) != 1 || stats[0].Samples != 2 {
		t.Fatalf("滑窗统计不符：%+v", stats)
	}
	if stats[0].LastLatency == nil || *stats[0].LastLatency != 23 {
		t.Fatalf("最近延迟应为 23：%+v", stats[0].LastLatency)
	}
	if stats[0].FailRate == nil || *stats[0].FailRate != 0.5 {
		t.Fatalf("失败率应为 0.5：%+v", stats[0].FailRate)
	}

	// 乱序 ping.result → CodeStaleSession（不断开，且不计入统计）
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 4, protocol.MethodPingResult, mkResult(2, 30))); err != nil {
		t.Fatal(err)
	}
	r.expectRPCError(t, protocol.CodeStaleSession)
	if got := st.ProbeStats("node-1", time.Now())[0].Samples; got != 2 {
		t.Fatalf("乱序结果不得计入：%d", got)
	}

	// 旧会话 ping.result → CodeStaleSession（非本连接会话，不断开）
	stale := &protocol.PingResultParams{
		SessionID: "sess-old", Sequence: 9, SentAt: time.Now().Unix(),
		Results: []protocol.PingResult{{TaskID: "t1", LatencyMS: 1}},
	}
	if err := c.WriteMessage(websocket.TextMessage, rpcRequest(t, 5, protocol.MethodPingResult, stale)); err != nil {
		t.Fatal(err)
	}
	r.expectRPCError(t, protocol.CodeStaleSession)
}

// TestPingRedispatchOnChangeAndReconnect 验证在线变更全量重发与离线期间
// 变更的上线补发。
func TestPingRedispatchOnChangeAndReconnect(t *testing.T) {
	srv, st, token := setup(t)

	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	r := startReader(c)
	helloOK(t, c, r, "sess-1", []string{"ping"})

	// 无任务配置：不应收到 ping.tasks
	if _, ok := r.next(500 * time.Millisecond); ok {
		t.Fatal("无任务配置不应下发")
	}

	// 在线变更 → 版本 +1 全量重发
	if _, err := st.SetProbeTasks("node-1", []protocol.PingTask{{ID: "t1", Target: "a:1", Interval: 5}}); err != nil {
		t.Fatal(err)
	}
	reqID, version, tasks := readPingTasks(t, r)
	if version != 1 || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("在线变更应下发版本 1：v=%d tasks=%+v", version, tasks)
	}
	ackRequest(t, c, reqID)
	c.Close()

	// 等待服务端摘除连接
	deadline := time.Now().Add(3 * time.Second)
	for {
		if n, _ := st.Get("node-1"); !n.Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("连接关闭后节点应离线")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 离线期间变更两次
	if _, err := st.SetProbeTasks("node-1", []protocol.PingTask{{ID: "t2", Target: "b:2", Interval: 10}}); err != nil {
		t.Fatal(err)
	}
	if v, err := st.SetProbeTasks("node-1", []protocol.PingTask{
		{ID: "t2", Target: "b:2", Interval: 10}, {ID: "t3", Target: "c:3", Interval: 30},
	}); err != nil || v != 3 {
		t.Fatalf("离线变更版本应为 3：v=%d err=%v", v, err)
	}

	// 重新上线：立即补发最新版本（版本 3，任务两项）
	c2, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	r2 := startReader(c2)
	helloOK(t, c2, r2, "sess-2", []string{"ping"})
	_, version, tasks = readPingTasks(t, r2)
	if version != 3 || len(tasks) != 2 {
		t.Fatalf("上线应补发版本 3 全量任务：v=%d tasks=%+v", version, tasks)
	}
}

// TestPingNoCapabilityNoDispatch 验证未声明 ping 能力的连接不受下发。
func TestPingNoCapabilityNoDispatch(t *testing.T) {
	srv, st, token := setup(t)
	if _, err := st.SetProbeTasks("node-1", []protocol.PingTask{{ID: "t1", Target: "a:1", Interval: 5}}); err != nil {
		t.Fatal(err)
	}

	c, _, err := dial(t, srv, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := startReader(c)
	helloOK(t, c, r, "sess-1", nil)

	// hello 后不下发，在线变更也不下发
	if _, ok := r.next(500 * time.Millisecond); ok {
		t.Fatal("无 ping 能力不应下发")
	}
	if _, err := st.SetProbeTasks("node-1", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.next(500 * time.Millisecond); ok {
		t.Fatal("无 ping 能力不应因变更下发")
	}
}
