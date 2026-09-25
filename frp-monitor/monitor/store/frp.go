// SPDX-License-Identifier: Apache-2.0

// FRP 隧道对账（根 README §4、§7）：proxy stat 按 (user, clientID) 组合
// 绑定键匹配节点 FRP 绑定；未匹配（无声明或冲突）的隧道 NodeID 为空，
// 仍参与展示。对账只读，冲突不自动改绑。
package store

import (
	"sort"
	"time"
)

// FRPProxy 为 frps 内存统计中单个 Proxy 的只读快照（service 层周期喂入）。
// 流量为服务端视角：TodayTrafficIn=frps 从隧道收到字节，
// TodayTrafficOut=frps 向隧道发出字节（今日）。
type FRPProxy struct {
	Name, Type, User, ClientID string
	Online                     bool
	CurConns                   int64
	TodayTrafficIn             int64
	TodayTrafficOut            int64
}

// Tunnel 为一条隧道的对账视图。NodeID 为空表示未匹配到节点
// （无节点声明该绑定键，或绑定键被多节点声明冲突）。
type Tunnel struct {
	NodeID string
	Proxy  FRPProxy
	// LocalAddr 为匹配节点上报的同 name proxy 的本地目标；未匹配节点或
	// 节点未上报同名 proxy 时为 nil。只允许出现在管理视图。
	LocalAddr *string
}

// bindKey 为 FRP 关联键 user+clientID（不同 user 可有同名 clientID，
// 不能只按 clientID 建表）。
func bindKey(user, clientID string) string { return user + "\x00" + clientID }

// ReconcileTunnels 把 proxy stats 对账到节点：唯一声明 (user, clientID)
// 绑定键的节点获得对应隧道；冲突或无人声明的隧道 NodeID 为空。
// 输出按 (NodeID, Name, Type) 排序，保证稳定。纯函数，不修改任何状态。
func ReconcileTunnels(all []NodeState, proxies []FRPProxy) []Tunnel {
	claims := make(map[string][]string) // bindKey → 声明节点 ID
	byNode := make(map[string]NodeState, len(all))
	for _, n := range all {
		byNode[n.NodeID] = n
		if n.FRP == nil || n.FRP.ClientID == "" {
			continue
		}
		k := bindKey(n.FRP.User, n.FRP.ClientID)
		claims[k] = append(claims[k], n.NodeID)
	}
	out := make([]Tunnel, 0, len(proxies))
	for _, p := range proxies {
		t := Tunnel{Proxy: p}
		if ids := claims[bindKey(p.User, p.ClientID)]; len(ids) == 1 {
			t.NodeID = ids[0]
			if n, ok := byNode[ids[0]]; ok && n.FRP != nil {
				for _, lp := range n.FRP.Proxies {
					if lp.Name == p.Name {
						addr := lp.LocalAddr
						t.LocalAddr = &addr
						break
					}
				}
			}
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		if out[i].Proxy.Name != out[j].Proxy.Name {
			return out[i].Proxy.Name < out[j].Proxy.Name
		}
		return out[i].Proxy.Type < out[j].Proxy.Type
	})
	return out
}

// canonicalProxies 返回按 (user, clientID, name, type) 排序的副本
// （上游按 type 分组且组内遍历 map，顺序不稳定），供变化检测比较。
func canonicalProxies(proxies []FRPProxy) []FRPProxy {
	out := make([]FRPProxy, len(proxies))
	copy(out, proxies)
	sort.Slice(out, func(i, j int) bool {
		if out[i].User != out[j].User {
			return out[i].User < out[j].User
		}
		if out[i].ClientID != out[j].ClientID {
			return out[i].ClientID < out[j].ClientID
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func proxiesEqual(a, b []FRPProxy) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// proxyFlipKey 为隧道事件翻转检测键。
func proxyFlipKey(p FRPProxy) string {
	return p.User + "\x00" + p.ClientID + "\x00" + p.Name
}

// clientFlipKey 为客户端事件翻转检测键。上游注册表以 "{user}.{clientID}"
// 为键，断连时稳定 clientID 的条目被标记离线保留，未设置 clientID 的
// 临时条目被移除；后者以 runID 区分不同临时连接。
func clientFlipKey(c FRPClient) string {
	if c.ClientID == "" {
		return c.User + "\x00\x00" + c.RunID
	}
	return bindKey(c.User, c.ClientID)
}

// detectProxyFlips 对比上一份 proxy 快照，记录 Online 翻转事件
// （首次出现只建基线，不记事件；消失的键遗忘）。调用方须持有 s.mu。
func (s *Store) detectProxyFlips(proxies []FRPProxy, now time.Time) {
	if s.prevProxyOnline == nil {
		s.prevProxyOnline = make(map[string]bool)
	}
	seen := make(map[string]struct{}, len(proxies))
	for _, p := range proxies {
		key := proxyFlipKey(p)
		seen[key] = struct{}{}
		prev, ok := s.prevProxyOnline[key]
		s.prevProxyOnline[key] = p.Online
		if !ok || prev == p.Online {
			continue
		}
		kind := EventTunnelOffline
		if p.Online {
			kind = EventTunnelOnline
		}
		s.recordEvent(FRPEvent{
			TS:     now.Unix(),
			NodeID: s.nodeForKey(p.User, p.ClientID),
			Kind:   kind,
			Name:   p.Name,
			Detail: "user=" + p.User + " client_id=" + p.ClientID + " type=" + p.Type,
		})
	}
	for k := range s.prevProxyOnline {
		if _, ok := seen[k]; !ok {
			delete(s.prevProxyOnline, k)
		}
	}
}

// detectClientFlips 对比上一份注册表快照，记录 Online 翻转事件
// （首次出现只建基线，不记事件；消失的键遗忘）。调用方须持有 s.mu。
func (s *Store) detectClientFlips(clients []FRPClient, now time.Time) {
	if s.prevClientOnline == nil {
		s.prevClientOnline = make(map[string]bool)
	}
	seen := make(map[string]struct{}, len(clients))
	for _, c := range clients {
		key := clientFlipKey(c)
		seen[key] = struct{}{}
		prev, ok := s.prevClientOnline[key]
		s.prevClientOnline[key] = c.Online
		if !ok || prev == c.Online {
			continue
		}
		kind := EventClientOffline
		if c.Online {
			kind = EventClientOnline
		}
		name := c.ClientID
		if name == "" {
			name = c.RunID
		}
		s.recordEvent(FRPEvent{
			TS:     now.Unix(),
			NodeID: s.nodeForKey(c.User, c.ClientID),
			Kind:   kind,
			Name:   name,
			Detail: "user=" + c.User + " client_id=" + c.ClientID + " run_id=" + c.RunID,
		})
	}
	for k := range s.prevClientOnline {
		if _, ok := seen[k]; !ok {
			delete(s.prevClientOnline, k)
		}
	}
}

// nodeForKey 返回唯一声明 (user, clientID) 绑定键的节点 ID；
// 无声明、冲突或 clientID 为空时返回空串。调用方须持有 s.mu。
func (s *Store) nodeForKey(user, clientID string) string {
	if clientID == "" {
		return ""
	}
	found := ""
	for _, n := range s.nodes {
		if n.FRP == nil || n.FRP.ClientID != clientID || n.FRP.User != user {
			continue
		}
		if found != "" {
			return "" // 多节点声明同一绑定键：冲突，不归因
		}
		found = n.NodeID
	}
	return found
}
