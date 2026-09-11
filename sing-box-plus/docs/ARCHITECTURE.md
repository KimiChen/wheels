# 架构

## 形态

零补丁 overlay：独立 Go module，`require github.com/sagernet/sing-box v1.14.0`，自有 main 位于
`cmd/sing-box-plus`，**不修改上游任何源码文件**。版本由 `go.sum` 与 `upstream.lock` 的 commit 双重固定。

```text
cmd/sing-box-plus/     自有 main：9 个复制自上游的 CLI 文件 + 最小 registry + tracker 注入
internal/minreg/       最小 registry：只注册 vless/shadowsocks inbound、direct/block outbound
internal/userstats/    计量、快照导出、配额闸断与访问审计
internal/testenv/      集成测试用的客户端拓扑（与生产 registry 严格分开）
```

## 数据面

```text
协议握手与用户认证
  -> metadata { inbound, network, user }
  -> 路由与 outbound 选择
  -> UserStatsTracker            <- 本项目的唯一挂点
       RoutedConnection          -> tcp_uplink / tcp_downlink
       RoutedPacketConnection    -> udp_uplink / udp_downlink
       RoutedFlow                -> 固定返回 nil，不参与计费
  -> 通用 copy / packet copy / splice
  -> 进程级 registry
  -> 本机只读累计快照
```

三个必须记住的位置关系：

1. **tracker 在认证之后**。因此失败关闭表现为「连接建立后被重置」，不是协议层认证失败。
2. **tracker 在拨号之前，但阻断不了拨号**。`ConnectionManager.NewConnection` 先拨号后 copy，
   tracker 只能决定交给它的是哪个 conn，无法阻止路由去调用它。被拒绝的连接仍会让目的地
   看到一次完成握手、0 字节的 TCP 连接。
3. **tracker 恒在包装链最外层**。上游两处 `AppendTracker` 都在 `box.New` 体内，自有 main 在
   `box.New` 之后追加，因此本项目的包装最外。这是四向口径正确的前提。

## 注入点时序

`route.Router.AppendTracker` 是无锁 `append`，数据面无锁遍历该切片。因此「`Start()` 之前追加」
不是风格偏好而是正确性约束——`internal/userstats/racecheck_test.go` 用子进程固定住了这一点：
在 `Start()` 之后追加必被 race detector 报出。

## 计数与闸断为什么都在 CountFunc 里

sing 的 `bufio.Copy` 会用 `UnwrapCountReader` / `UnwrapCountWriter` 把包装一路解包到底层 conn、
只把 `CountFunc` 摘走，splice 路径同样只调 `CountFunc`。所以：

- 包装层的 `Read` / `Write` 在快路径上根本不执行，任何写在那里的逻辑都会静默失效；
- `N.CountFunc` 的签名是 `func(n int64)`，没有 error 返回，中止的唯一手段是关闭连接。

于是四向累加与配额扣减都发生在回调内，闸断时由回调关闭本连接，让正在跑的 copy 在下一次 I/O 上
自然出错退出。**计数点即执行点**，registry 不需要额外维护活跃连接集合。

## 生命周期

`runtime_id`、`started_at_unix_ms`、`sequence` 三者由自有 main 在进入 run 循环**之前**生成，
由进程级 registry 持有。`services[]` 中的 `user_stats` 实例只负责监听 UDS、读取 registry 并序列化。

理由：SIGHUP 在同一进程内重建整个 Box 与全部 service 实例。把这三者挂在实例上，每次重载都会重置，
而三种错误拆法的后果依次递减——仅 `sequence` 归 1 会让采集端**静默丢弃**快照直到序号追平；
`runtime_id` 一并重置会静默丢失重载窗口的增量；只有 `started_at_unix_ms` 单独重置才会抛错。

## 与上游账本互斥

`common/trafficcontrol` 被 `box.go` 无条件 import，`service/api` 无 build 约束地注册，
`ssm-api` 同样无条件注册。**裁 build tag 挡不住它们**，因此：

- 自有 main 传入自建的 service registry、不注册 ssmapi，使含 `ssm-api` 的配置在解析期失败；
- 配置校验另外拒绝 `services[] type:"api"`、`experimental.clash_api` 与
  `experimental.v2ray_api.listen`。

两层是先后关系而非互为兜底。
