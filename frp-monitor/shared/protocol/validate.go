// SPDX-License-Identifier: Apache-2.0

package protocol

import "fmt"

// Validate 校验 hello 参数：版本为正、必填字段非空、长度受限。
func (p *HelloParams) Validate() error {
	if p.SchemaVersion < 1 {
		return fmt.Errorf("protocol: schema_version 越界：%d", p.SchemaVersion)
	}
	if p.AgentVersion == "" || p.FRPVersion == "" {
		return fmt.Errorf("protocol: agent_version/frp_version 不能为空")
	}
	if err := checkLen("agent_version", p.AgentVersion, MaxVersionLen); err != nil {
		return err
	}
	if err := checkLen("frp_version", p.FRPVersion, MaxVersionLen); err != nil {
		return err
	}
	if err := checkSession(p.SessionID); err != nil {
		return err
	}
	if len(p.Capabilities) > MaxCapabilities {
		return fmt.Errorf("protocol: capabilities 数量 %d 超过上限 %d",
			len(p.Capabilities), MaxCapabilities)
	}
	for _, c := range p.Capabilities {
		if err := checkLen("capability", c, MaxCapabilityLen); err != nil {
			return err
		}
	}
	if p.ReportInterval < 1 || p.ReportInterval > 3600 {
		return fmt.Errorf("protocol: report_interval 越界 [1,3600]：%d", p.ReportInterval)
	}
	if p.SentAt <= 0 {
		return fmt.Errorf("protocol: sent_at 必须为正 Unix 秒：%d", p.SentAt)
	}
	return nil
}

// Validate 校验 report 参数：会话与序号有效，三类载荷至少一个非空，
// 嵌套载荷逐一校验。
func (p *ReportParams) Validate() error {
	if err := checkSession(p.SessionID); err != nil {
		return err
	}
	if p.Sequence < 1 {
		return fmt.Errorf("protocol: sequence 必须从 1 递增：%d", p.Sequence)
	}
	if p.SentAt <= 0 {
		return fmt.Errorf("protocol: sent_at 必须为正 Unix 秒：%d", p.SentAt)
	}
	if p.Facts == nil && p.Metrics == nil && p.Extensions == nil {
		return fmt.Errorf("protocol: report 载荷为空")
	}
	if p.Facts != nil {
		if err := p.Facts.Validate(); err != nil {
			return err
		}
	}
	if p.Metrics != nil {
		if err := p.Metrics.Validate(); err != nil {
			return err
		}
	}
	if p.Extensions != nil && p.Extensions.FRP != nil {
		return p.Extensions.FRP.Validate()
	}
	return nil
}

// Validate 校验 FRP 扩展区。
func (e *FRPExtension) Validate() error {
	if err := checkLen("frp.client_id", e.ClientID, MaxProxyNameLen); err != nil {
		return err
	}
	if err := checkLen("frp.frp_version", e.FRPVersion, MaxVersionLen); err != nil {
		return err
	}
	if len(e.Proxies) > MaxProxiesPerAgent {
		return fmt.Errorf("protocol: proxies 数量 %d 超过上限 %d",
			len(e.Proxies), MaxProxiesPerAgent)
	}
	for i, px := range e.Proxies {
		if px.Name == "" {
			return fmt.Errorf("protocol: proxies[%d].name 为空", i)
		}
		if err := checkLen("proxy.name", px.Name, MaxProxyNameLen); err != nil {
			return err
		}
		if err := checkLen("proxy.type", px.Type, MaxProxyTypeLen); err != nil {
			return err
		}
		if err := checkLen("proxy.local_addr", px.LocalAddr, MaxProxyAddrLen); err != nil {
			return err
		}
		if err := checkLen("proxy.status", px.Status, MaxProxyStatusLen); err != nil {
			return err
		}
	}
	return nil
}

// Validate 校验下发的探测任务列表：数量受限、间隔在范围内、ID 唯一。
func (p *PingTasksParams) Validate() error {
	if p.Version < 1 {
		return fmt.Errorf("protocol: tasks version 必须为正：%d", p.Version)
	}
	if len(p.Tasks) > MaxPingTasks {
		return fmt.Errorf("protocol: 探测任务 %d 个超过上限 %d", len(p.Tasks), MaxPingTasks)
	}
	seen := make(map[string]struct{}, len(p.Tasks))
	for i, t := range p.Tasks {
		if t.ID == "" {
			return fmt.Errorf("protocol: tasks[%d].id 为空", i)
		}
		if err := checkLen("task.id", t.ID, MaxPingTaskIDLen); err != nil {
			return err
		}
		if _, dup := seen[t.ID]; dup {
			return fmt.Errorf("protocol: 探测任务 ID 重复：%s", t.ID)
		}
		seen[t.ID] = struct{}{}
		if t.Target == "" {
			return fmt.Errorf("protocol: tasks[%d].target 为空", i)
		}
		if err := checkLen("task.target", t.Target, MaxPingTargetLen); err != nil {
			return err
		}
		if t.Interval < MinPingInterval || t.Interval > MaxPingInterval {
			return fmt.Errorf("protocol: 任务 %s 间隔 %d 越界 [%d,%d]",
				t.ID, t.Interval, MinPingInterval, MaxPingInterval)
		}
	}
	return nil
}

// Validate 校验探测结果上报。
func (p *PingResultParams) Validate() error {
	if err := checkSession(p.SessionID); err != nil {
		return err
	}
	if p.Sequence < 1 {
		return fmt.Errorf("protocol: sequence 必须从 1 递增：%d", p.Sequence)
	}
	if len(p.Results) > MaxPingResultsPerMessage {
		return fmt.Errorf("protocol: results 数量 %d 超过上限 %d",
			len(p.Results), MaxPingResultsPerMessage)
	}
	for i, r := range p.Results {
		if r.TaskID == "" {
			return fmt.Errorf("protocol: results[%d].task_id 为空", i)
		}
		if err := checkLen("result.task_id", r.TaskID, MaxPingTaskIDLen); err != nil {
			return err
		}
		if r.LatencyMS < LatencyFailed {
			return fmt.Errorf("protocol: results[%d].latency_ms 越界：%d", i, r.LatencyMS)
		}
	}
	return nil
}

func checkSession(id string) error {
	if id == "" {
		return fmt.Errorf("protocol: session_id 为空")
	}
	return checkLen("session_id", id, MaxSessionIDLen)
}

func checkLen(field, v string, limit int) error {
	if len(v) > limit {
		return fmt.Errorf("protocol: %s 长度 %d 超过上限 %d", field, len(v), limit)
	}
	return nil
}
