package userstats

import (
	"bytes"
	"encoding/json"

	E "github.com/sagernet/sing/common/exceptions"
)

var errUnknownLineage = E.New("全量表包含当前配置中不存在的计费身份")

// quotaRequest 是 PUT /v2/quota 的请求体。
//
// 全量覆盖语义：不提供增量的 block / unblock——增量事件丢一条即永久错位，
// 全量覆盖天然幂等，也天然处理进程重启后的状态清零（README §4.9）。
type quotaRequest struct {
	SchemaVersion int          `json:"schema_version"`
	NodeID        string       `json:"node_id"`
	RuntimeID     string       `json:"runtime_id"`
	Epoch         uint64       `json:"epoch"`
	Entries       []QuotaEntry `json:"entries"`
}

type quotaResponse struct {
	SchemaVersion int    `json:"schema_version"`
	Epoch         uint64 `json:"epoch"`
	Applied       int    `json:"applied"`
}

type quotaErrorBody struct {
	SchemaVersion int               `json:"schema_version"`
	Error         quotaErrorPayload `json:"error"`
}

type quotaErrorPayload struct {
	Code    int      `json:"code"`
	Unknown []string `json:"unknown,omitempty"`
}

func (s *Service) handleQuotaRoute(req *request) (int, []byte) {
	if req.path != "/v2/quota" {
		return statusNotFound, mustJSON(newErrorBody(statusNotFound))
	}
	if req.method != "PUT" {
		return statusMethodNotAllowed, mustJSON(newErrorBody(statusMethodNotAllowed))
	}

	decoder := json.NewDecoder(bytes.NewReader(req.body))
	// 对未知字段失败关闭，与配置面同一纪律：控制面能改变转发行为，宽容解析等于静默降级。
	decoder.DisallowUnknownFields()
	var payload quotaRequest
	if err := decoder.Decode(&payload); err != nil {
		return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
	}
	if decoder.More() {
		return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
	}
	if payload.SchemaVersion != SchemaVersion {
		return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
	}
	// node_id 与 runtime_id 必须逐字节相同，否则 409 并整份丢弃。
	// 这条同时是「重启即解封」的自动纠正机制：进程重启后 collector 拿旧 runtime_id 推送会
	// 立刻拿到 409，据此知道必须按新 runtime 重推。
	if payload.NodeID != s.registry.NodeID() || payload.RuntimeID != s.registry.RuntimeID() {
		s.registry.quota.rejectedByRuntime.Add(1)
		return statusConflict, mustJSON(newErrorBody(statusConflict))
	}
	if payload.Epoch == 0 {
		return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
	}
	// epoch 单调递增，≤ 已接受值即整份丢弃，用于抵抗乱序重投。
	if s.registry.quota.accepted.Load() && payload.Epoch <= s.registry.quota.epoch.Load() {
		s.registry.quota.rejectedByEpoch.Add(1)
		return statusConflict, mustJSON(newErrorBody(statusConflict))
	}
	for _, entry := range payload.Entries {
		if err := validateToken("entries[].inbound_tag", entry.InboundTag); err != nil {
			return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
		}
		if err := validateToken("entries[].name", entry.Name); err != nil {
			return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
		}
		if entry.RemainingBytes < 0 {
			return statusBadRequest, mustJSON(newErrorBody(statusBadRequest))
		}
	}

	applied, unknown, err := s.registry.ApplyQuotaTable(payload.Epoch, payload.Entries)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("拒绝配额全量表：", err, " 未知身份=", unknown)
		}
		return statusBadRequest, mustJSON(quotaErrorBody{
			SchemaVersion: SchemaVersion,
			Error:         quotaErrorPayload{Code: statusBadRequest, Unknown: unknown},
		})
	}
	return statusOK, mustJSON(quotaResponse{
		SchemaVersion: SchemaVersion,
		Epoch:         payload.Epoch,
		Applied:       applied,
	})
}
