// SPDX-License-Identifier: Apache-2.0

// Package protocol 定义 agent 与 monitor 之间的 WSS 协议契约（根 README §5）。
//
// 传输为独立 HTTPS/WSS Listener 路径 /agent/v1/ws，握手期用 Authorization 认证；
// 帧内为 JSON-RPC 2.0。保留 monitor-probe 的 hello、report、ping.tasks、
// ping.result 方法概念，params 为自有版本，不承诺与原版二进制互通。
package protocol

import "encoding/json"

// SchemaVersion 为当前协议 schema 版本，hello 协商使用；
// 不支持的对端版本必须明确报错，不得静默降级。
const SchemaVersion = 1

// JSONRPCVersion 为帧格式的固定标识。
const JSONRPCVersion = "2.0"

// 方法名。ping.tasks 由 monitor 下发，其余由 agent 发起。
const (
	MethodHello      = "hello"
	MethodReport     = "report"
	MethodPingTasks  = "ping.tasks"
	MethodPingResult = "ping.result"
)

// JSON-RPC 2.0 标准错误码与自定义错误码（-32000 到 -32099 为服务端自定义段）。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602

	// CodeUnsupportedSchema 表示 hello 协商的 schema/capabilities 不被支持。
	CodeUnsupportedSchema = -32001
	// CodeStaleSession 表示报告来自旧会话或乱序序号，接收方拒绝覆盖新会话。
	CodeStaleSession = -32002
)

// Request 为 JSON-RPC 2.0 请求/通知。ID 为 nil 时是通知；
// 本契约约定 ID 使用非负整数。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *uint64         `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response 为 JSON-RPC 2.0 响应，Result 与 Error 有且仅有一个。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error 为 JSON-RPC 2.0 错误对象。
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// NewRequest 构造请求并编码 params。id 为 nil 时产出通知。
func NewRequest(id *uint64, method string, params any) (*Request, error) {
	req := &Request{JSONRPC: JSONRPCVersion, ID: id, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		req.Params = raw
	}
	return req, nil
}
