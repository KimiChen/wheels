# 上游补丁

`series` 按顺序将补丁应用到 `upstream.lock` 固定的 FRP 提交。每次准备都从原始
源码重新组装，不能在已打补丁的源码树上重复应用；升级上游时重新生成并验证全部补丁。

| 补丁 | 职责 |
|---|---|
| `0001-monitor-lifecycle.patch` | 配置与版本入口、client/server 监控生命周期、客户端只读 Proxy 快照、内存 metrics 注册保护及回归测试 |
| `0002-storage-dependencies.patch` | SQLite、内嵌 VictoriaMetrics 及完整 FRP 模块图所需的最终 `go.mod` / `go.sum`，要求 Go 1.26.6+ |
| `0003-persistence-shutdown.patch` | 监控启用时处理 TERM/INT，经原生退出路径取消并关闭监控存储；关闭监控时保留原生信号行为 |
| `0004-frp-reconciliation.patch` | 从 Registry、Proxy Manager 和 Stats 提供有界只读快照，锁忙时返回 unavailable；保留身份与流量口径，不带认证秘密 |
| `0005-frp-observation.patch` | 独立私有详情适配器、file/include/Store 来源、实际服务端入口、Proxy/Visitor/控制错误与恢复事件；不修改原生转发协议 |
| `0006-config-management.patch` | 显式 Store 托管、原生写入口屏障、装载 Store 前恢复、整集内存替换，以及实际 Proxy/Visitor 运行验证；未托管模式保留原生行为 |

补丁只包含上游既有文件的必要改动与相关测试，不提交完整上游树，不混入无关格式化。
独立扩展通过 Overlay 映射到 `extension/frpmonitor/`。FRP wire protocol、原生命令、
认证与 Dashboard 保持；可信归属由管理员绑定、节点报告和服务端观察共同核对。

`scripts/frp.py prepare` 逐项执行 `git apply --check` 后应用；依赖版本和校验值只维护
一份最终补丁。行为测试及真实二进制验证见 [tests/README.md](../tests/README.md)。
