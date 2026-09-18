package userstats

import (
	"bytes"
	"encoding/json"

	E "github.com/sagernet/sing/common/exceptions"
)

// SchemaVersion 是本项目快照与全部错误响应共用的唯一版本常量（README §4.5）。
//
// /healthz、错误 body 与快照必须共用同一个常量，不得出现 1/2 混用；
// 路径版本号与它同步推进，本项目不提供 /v1/snapshot。
const SchemaVersion = 3

// Health 是闭集：恰好四个 bool 键，出现额外键即整份拒绝；新增键必须同时提升 SchemaVersion
// （audit_dropped 就是这么从 2 推到 3 的，路径同步改为 /v3）。
//
// 四位分两类，语义不同，不要混用：
//   - 前三位是**这份快照不可入账**：计数器截断、sequence 饱和、身份被截断，任一为真都意味着
//     数字本身不可信，采集端必须拒绝入账（unhealthy() 与 /healthz 只看这三位）。
//   - audit_dropped 是**审计有损**：计费计数器仍然准确，可以照常入账，但 §4.8 的 JSONL
//     旁路丢过记录（开文件失败、轮转重命名失败、重开失败、写失败复位，或纪律 10 触底
//     而无已轮转文件可删）。它不进 unhealthy()，否则一次审计丢弃会把整条计费链路停掉。
//
// 队列满导致的丢弃不在这一位里：那种丢弃在文件内就地写成 ev=gap 行，是自描述的，
// 不需要带外信号；这一位专门对付**连 gap 行都写不出去**的那些路径。
//
// identity_limit_reached 只由两种越限触发，都不是普通的身份轮换：
//   - 活跃血统数超过 max_identities；
//   - 含墓碑在内的总血统数超过由 max_identities 派生的总量上限。
//
// 2026-09 之前它数的是含墓碑的总数并以 max_identities 为界，于是每一轮身份轮换都让
// 这个数单调增长，最终把一个完全健康的节点变成永久 503。墓碑不会让任何数字失真——
// Reconcile 不截断任何东西——所以拿它去触发「数字不可信」是按构造过度触发。
type Health struct {
	CounterOverflow      bool `json:"counter_overflow"`
	SequenceOverflow     bool `json:"sequence_overflow"`
	IdentityLimitReached bool `json:"identity_limit_reached"`
	AuditDropped         bool `json:"audit_dropped"`
}

// SnapshotUser 是一个计费身份的四向累计值。
type SnapshotUser struct {
	Name             string `json:"name"`
	Generation       uint64 `json:"generation"`
	Active           bool   `json:"active"`
	TCPUplinkBytes   uint64 `json:"tcp_uplink_bytes"`
	TCPDownlinkBytes uint64 `json:"tcp_downlink_bytes"`
	UDPUplinkBytes   uint64 `json:"udp_uplink_bytes"`
	UDPDownlinkBytes uint64 `json:"udp_downlink_bytes"`
}

// SnapshotInbound 的 listen / listen_port 是**配置值而非实际绑定结果**。
//
// 上游不给回读实际绑定的路径，因此 Validate 要求显式配置非 0 的 listen_port，
// 使配置值与实际绑定恒等（README §4.5、§4.6 第 2 条）。
type SnapshotInbound struct {
	Tag         string         `json:"tag"`
	Type        string         `json:"type"`
	Listen      string         `json:"listen"`
	ListenPort  uint16         `json:"listen_port"`
	Generation  uint64         `json:"generation"`
	Active      bool           `json:"active"`
	TCPSessions int64          `json:"tcp_sessions"`
	UDPSessions int64          `json:"udp_sessions"`
	Users       []SnapshotUser `json:"users"`
}

// Snapshot 是 GET /v3/snapshot 的响应体。
type Snapshot struct {
	SchemaVersion   int               `json:"schema_version"`
	NodeID          string            `json:"node_id"`
	RuntimeID       string            `json:"runtime_id"`
	StartedAtUnixMs uint64            `json:"started_at_unix_ms"`
	Sequence        uint64            `json:"sequence"`
	Health          Health            `json:"health"`
	Inbounds        []SnapshotInbound `json:"inbounds"`
}

// Snapshot 生成一份非破坏性累计快照，并推进 sequence。
//
// 排序一律按已校验的 ASCII 原始字节升序，不使用任何 locale 或大小写折叠。
func (r *Registry) Snapshot() (*Snapshot, error) {
	if r.validationOnly {
		return nil, E.New("校验用 registry 不提供快照")
	}
	r.snapshotMu.Lock()
	defer r.snapshotMu.Unlock()

	snapshot := &Snapshot{
		SchemaVersion:   SchemaVersion,
		NodeID:          r.nodeID,
		RuntimeID:       r.runtimeID,
		StartedAtUnixMs: r.startedAtUnixMs,
		Sequence:        r.nextSequence(),
		Health: Health{
			CounterOverflow:      r.counterOverflow.Load(),
			SequenceOverflow:     r.sequenceOverflow.Load(),
			IdentityLimitReached: r.identityLimitReached.Load(),
			AuditDropped:         r.auditDropped(),
		},
		Inbounds: []SnapshotInbound{},
	}
	for _, record := range r.sortedInbounds() {
		inboundType, listen, listenPort := record.spec()
		item := SnapshotInbound{
			Tag:         record.tag,
			Type:        inboundType,
			Listen:      listen,
			ListenPort:  listenPort,
			Generation:  record.generation,
			Active:      record.active.Load(),
			TCPSessions: record.tcpSessions.Load(),
			UDPSessions: record.udpSessions.Load(),
			Users:       []SnapshotUser{},
		}
		for _, user := range record.sortedUsers() {
			item.Users = append(item.Users, SnapshotUser{
				Name:             user.name,
				Generation:       user.generation,
				Active:           user.active.Load(),
				TCPUplinkBytes:   user.tcpUplink.load(),
				TCPDownlinkBytes: user.tcpDownlink.load(),
				UDPUplinkBytes:   user.udpUplink.load(),
				UDPDownlinkBytes: user.udpDownlink.load(),
			})
		}
		snapshot.Inbounds = append(snapshot.Inbounds, item)
	}
	return snapshot, nil
}

// marshalJSONLine 序列化为以 LF 结尾的紧凑 JSON。
func marshalJSONLine(value any) ([]byte, error) {
	buffer := new(bytes.Buffer)
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// errorBody 是固定的错误响应对象，与快照共用 schema 版本常量。
type errorBody struct {
	SchemaVersion int          `json:"schema_version"`
	Error         errorPayload `json:"error"`
}

type errorPayload struct {
	Code int `json:"code"`
}

func newErrorBody(code int) errorBody {
	return errorBody{SchemaVersion: SchemaVersion, Error: errorPayload{Code: code}}
}

// healthBody 是 /healthz 的响应体。
type healthBody struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
}
