# sing-box 1.14.2 候选验证

2026-10-03：本分支是未完成的升级候选，**禁止发布和部署**。保持 README §4.2 的成功转发
计费合同，保留失败用例；不得通过改断言、排除 Linux 测试或仅跑正常流量来放行。

## 固定输入与已完成迁移

- 官方 `v1.14.2` tag：`af6e64c3b69e6132ebaee0e1a3d24e93903f6709`；本次对规范 GitHub 地址重新 `ls-remote` 核对。
- 准备树 SHA-256：`8119c3fde55e4716f2afc0192a897a464eaa1960365a799ed0611d8d9dc7be9b`。
- 官方精确依赖：sing `v0.9.6-0.20260922013354-87c33f17688f`；sing-tun `v0.9.6-0.20260924001923-ddaa4ca25e3b`；sing-mux `v0.3.8`；sing-vmess `v0.2.8`；bbolt `v0.0.0-20260915102804-500ee1e84832`。
- 9 份复制 CLI 已复核；两份 `run` / `check` 必须同步 `service.ExtendContext`，避免实例写入共用 registry。新增 `TestBoxContextIsolation` 验证两次创建和 check 后进程上下文未污染。
- 全树 `AppendTracker` 调用仍只有 `box.go:250` 与 `box.go:443`；修正 verify 中原先计算全树数量却未断言的问题。
- `NetworkManager.started` 的旧竞争已移除，旧 3 条 race 抑制清空；macOS 上完整无抑制 race 通过。
- `CountFunc` 仍为 `func(int64)`，unwrap 仍收集 reader / writer 两组回调；**回调执行时机发生不兼容变化**。

## Linux 阻塞证据

`internal/userstats/splice_failure_linux_test.go` 使用真实 Unix stream socketpair、131072 字节输入、
受限目的 socket 缓冲和 100 ms 写入 deadline，进入真实 Linux splice；在独立网络命名空间执行。
源读入量由读取剩余源数据反推，目标接收量从目标 socket 实际读出，再与本项目 registry 对账。
同一 Go 1.26.8、同一 Linux 内核、同一测试源码对比：

| 情况 | sing-box 1.14.0 / sing beta.4 | sing-box 1.14.2 官方组合 |
| --- | --- | --- |
| 目标已关闭，目标收到 0 B | 计费 0 B，通过 | 源消费 131072 B，计费 131072 B，失败 |
| 部分写入，目标收到 64896 B | copy 与计费均为 0，暴露原有漏计边界 | copy 正确返回 64896，但源消费与计费均 131072，失败 |

部分写入量依赖内核缓冲，不把 64896 硬编码为期待值；用实际收到量作为 oracle。
旧版本也有部分写入漏计，不能因此声称旧版该边界正确；但新版引入了完全未转发时也收费的回归。

原因是 [sing 的 splice 修复](https://github.com/SagerNet/sing/commit/0f86e07920f7e846c410785355fb843eac20657b)：
read counter 改为源读入 pipe 后立即触发，write counter 才按目标实际成功写出的字节执行。
本项目 tracker 只包装 inbound conn，uplink 正是 reader counter；目标 writer 由上游创建，
该回调没有获得目标写入结果的途径。配额消费也在同一回调内，因此不能只修统计显示而保留错误扣额。

## 适配评审与下一步

当前公共 `adapter.ConnectionTracker` 只返回包装后的 inbound conn，不能替换 outbound writer；
`CountFunc` 只有字节参数，无法区分读入 pipe 与转发完成，也无法观测写失败后 pipe 丢弃量。
因此目前没有证明可行、保持 splice 与既有合同的微小 wrapper 调整。

1. **首选：为依赖增加可选的成功转发计数接口。** 上游需在 splice 每次目标写入后回调实际成功量，
   包括出错前完成的部分写入；保持原 read/write counter 语义兼容。overlay 将 uplink 统计和额度扣减
   绑定该接口。需形成可审核的上游补丁或精确 fork、供应链锁及维护策略；当前候选没有引入该补丁。
2. **更大改造：在本项目可控的 outbound forwarding 边界拥有 writer。** 覆盖 TCP、TLS/REALITY/Vision、
   mux 与 UDP 路径，复杂度超出依赖补丁升级，须单独设计与性能对账。
3. **不采用的捷径：** 改成按读取收费；减少计费断言；将 ReaderReplaceable 设为 false；禁用 splice。
   前三者不能保持合同，最后一项也不能证明所有 WriteBuffer 部分写入精确计数，并会损害原性能目标。

解除阻塞前至少要通过：上述 0 写入和部分写入 oracle、RST/half-close/取消、source 与 destination
方向均精确、配额扣减与统计一致、多 tracker unwrap、真实 Linux splice、SS TCP/UDP、VLESS
与 Vision 的正常流量字节 oracle，以及 Linux/macOS 无抑制 race。

工具链安全重建与旧节点监听器修复可独立推进，不依赖本候选放行。

## 本次验证结果

- 官方 proxy/sumdb、独立 module cache：`go mod download` / `go mod verify` 通过，未删锁绕过校验。
- Go 1.26.8 / darwin arm64：vet、生产标签 / 无标签 / Windows amd64 构建、规范上游漂移、
  准备树哈希、9 份复制文件双向锁、AppendTracker 全树调用点、普通与无抑制 race 测试通过。
- 新 CLI 上下文隔离用例通过；Vision 仍需非 race 轮执行，sing-vmess 的 checkptr 限制仍存在。
- Linux amd64 全量 userstats 含子测试：115 通过、2 失败、1 跳过；失败恰为本文两个计费契约，
  跳过项是需要 race 的负向用例。以测试进程非零退出如实记录，不称完整门禁通过。
- Linux 新 sing-tun 的 `TestNetworkUpdateMonitorReceiveOverrun`：4.52 秒通过，独立 netns 内实际
  制造接收溢出、检查空闲 CPU 和后续通知，未对生产网络执行故障注入。
- Python 107 项通过，敏感信息扫描与 diff whitespace 检查通过。
- 提交前纳入已发布工具链提交 `9bb67f8`；工具链锁为 Go 1.26.8。保留独立研发分支供后续修复；未合并主线、未构建发布包或部署。
