// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"testing"

	"github.com/fatedier/frp/extension/frpmonitor/agent/probe"
	"github.com/fatedier/frp/extension/frpmonitor/shared/protocol"
)

func TestApplyPingTasks(t *testing.T) {
	id := uint64(7)
	valid := json.RawMessage(`{"version":1,"tasks":[{"id":"a","target":"example.com:443","interval":30}]}`)

	t.Run("成功回 null", func(t *testing.T) {
		resp := applyPingTasks(probe.NewManager(), &id, valid)
		if resp == nil || resp.Error != nil {
			t.Fatalf("合法任务须成功，得到 %+v", resp)
		}
		if resp.ID != id || string(resp.Result) != "null" {
			t.Fatalf("响应须回显 id 且 result 为 null，得到 %+v", resp)
		}
	})

	t.Run("版本回退拒绝", func(t *testing.T) {
		m := probe.NewManager()
		if resp := applyPingTasks(m, &id, valid); resp.Error != nil {
			t.Fatal(resp.Error)
		}
		resp := applyPingTasks(m, &id, valid) // 同版本重发
		if resp == nil || resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
			t.Fatalf("版本不回退须回 InvalidParams，得到 %+v", resp)
		}
	})

	t.Run("非法 JSON", func(t *testing.T) {
		resp := applyPingTasks(probe.NewManager(), &id, json.RawMessage(`{`))
		if resp == nil || resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
			t.Fatalf("非法 JSON 须回 InvalidParams，得到 %+v", resp)
		}
	})

	t.Run("非法任务", func(t *testing.T) {
		dup := json.RawMessage(`{"version":1,"tasks":[` +
			`{"id":"a","target":"h:80","interval":5},` +
			`{"id":"a","target":"h:81","interval":5}]}`)
		resp := applyPingTasks(probe.NewManager(), &id, dup)
		if resp == nil || resp.Error == nil || resp.Error.Code != protocol.CodeInvalidParams {
			t.Fatalf("重复任务 ID 须回 InvalidParams，得到 %+v", resp)
		}
	})

	t.Run("通知无响应", func(t *testing.T) {
		m := probe.NewManager()
		if resp := applyPingTasks(m, nil, valid); resp != nil {
			t.Fatalf("无 ID 的下行通知不回复，得到 %+v", resp)
		}
		// 通知也已应用：同版本重发（带 ID）须因版本未递增被拒绝。
		if resp := applyPingTasks(m, &id, valid); resp.Error == nil {
			t.Fatal("通知形式的任务列表未生效")
		}
	})
}
