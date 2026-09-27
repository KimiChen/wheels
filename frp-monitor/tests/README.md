# Tests

本目录保存 P0 原生 FRP 基线回归和脱敏参考数据。阶段范围以根目录
[README.md](../README.md) 为准；P0 通过不代表采集、WSS、存储或网页已实现。

## P0 原生基线

在子项目根目录执行，Python 仅使用标准库：

```sh
python3 scripts/frp.py build --native
python3 tests/smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
python3 -m unittest discover -s tests -p '*test*.py' -v
```

上例是 macOS arm64；Linux 或其他架构使用本机构建输出的
`dist/<GOOS>-<GOARCH>/`。参数必须指向当前机器能执行的两个文件，不能直接运行异构
Linux 发布包。可加 `--wire v1` 或 `--wire v2` 单独定位；默认依次运行两者。
`--timeout 30` 为每项 verify / 就绪等待的秒数，允许 1–300 秒。

每个 wire 版本实际覆盖：

- 用原生 `verify -c` 校验 frps、无 Proxy frpc 和 TCP Proxy frpc 的 TOML；
- 原生 frps Dashboard 匿名首页返回 401，临时 Basic 认证后 `/static/` 返回 HTML 200，
  且至少一个首页实际引用的同源 JavaScript 资源可读，避免误用 `noweb` 隐去原生界面；
- 无 Proxy 配置完成 token 认证登录，并在登录后保持运行；
- FRP TCP 隧道把包含全部字节值的二进制载荷原样传到本地 echo 服务；
- 终止 frps、确认旧隧道端口关闭，再启动 frps；同一个 frpc 进程恢复转发；
- 退出后回收进程和临时文件，正常终止超时则强制清理并判失败。

`transport.wireProtocol = "v1" / "v2"` 来自固定上游
[frpc 配置](https://github.com/fatedier/frp/blob/4a23aa181c1d7e28eecaa8216024ed753b9d27c8/conf/frpc_full_example.toml)；
frps 自动接收两种 wire。无 Proxy 登录事件按同一 commit 的
[client/service.go](https://github.com/fatedier/frp/blob/4a23aa181c1d7e28eecaa8216024ed753b9d27c8/client/service.go)
识别，升级基线时应复核该事件。测试使用默认 TLS、token 认证与 TCP transport。

所有监听、FRP 连接和 echo 目标均为 `127.0.0.1`，动态分配端口，不访问外部服务，
不读取 `.env` 或已有 FRP 配置。父进程的代理和凭据环境变量不传给测试进程。
临时目录权限为 `0700`、配置文件为 `0600`，每轮生成独立随机测试 token；
退出时删除。进程日志持续读取但不保存、不原样打印，只保留固定登录事件和脱敏的
失败阶段/退出码；失败时可用 `--wire` 缩小范围。端口预留在启动前才释放，
操作系统不支持将预留 socket 移交给 FRP，因此极少数抢占会表现为明确失败，可重跑。

Dashboard 在 v1、v2 两轮均验证，使用独立动态回环端口和临时凭据；不跟随外部资源，
不读取或调用管理 API。这是原生 FRP 界面资源与认证回归，不是新的监控网页验收。

`test_smoke.py` 验证测试工具自身的环境隔离、临时配置权限、子进程提前退出、
超时、强制清理和二进制 echo；它不替代真实 FRP smoke。当前基线只含 TCP、token、
wire v1/v2 和上述 Dashboard 页面检查，未覆盖 UDP/HTTP/STCP、OIDC、reload、
Dashboard 管理 API、Prometheus 或混合新旧二进制。

## P0 协议测试与采集参考向量

`shared/testdata/` 保存 `hello`、首报、TCP 探测任务和结果的协议黄金帧，
由 `shared/` 的 Go 测试直接消费，检查协议解析与验证。将扩展映射到固定上游
Go module 后，可用以下入口运行协议测试及所选原生 FRP 单元测试：

```sh
python3 scripts/frp.py test
```

`pipeline_test.py` 检查源码准备/构建脚本的本地边界与失败行为；上方
`python3 -m unittest discover -s tests -p '*test*.py' -v` 会同时运行它和
smoke 工具自测，无需把测试文件名约定限定为 `test_` 前缀。

[`fixtures/collect/`](fixtures/collect/README.md) 目前包含 17 个自行构造的 Linux
采集输入/预期向量，供 P1 的真实采集函数消费。它们覆盖 CPU 差分、内存、磁盘、
网络计数器等边界；输入没有读取当前机器状态。**P0 尚无采集器消费这些向量，
因此不能声称采集算法或与参考实现的比对已经通过。**

## 后续验收（尚未实现）

- **P1 实时闭环**：默认每 1 秒报告与 1–3600 秒配置范围；首样本质量、Facts/Metrics
  字段及采集算法；WSS 认证、重连退避、无 Proxy/首次登录失败仍能监控；
  `agent_id`、`session_id + sequence` 和 FRP 关联键隔离；旧会话、重复和迟到报告；
  心跳与指标过期分别判断；报告长度、频率、畸形 JSON 和非有限值约束。
- **P2 完整采集体验**：TCP 成功/拒绝/超时、DNS 缺样、多地址回退、任务替换和限额；
  网卡过滤、集合变化、计数器回退、进程/主机重启；SQLite 恢复、流量事务与分钟聚合；
  丢样保持缺口，监控重启不将持久化节点直接标在线。
- **P3 FRP 与发布**：UDP/HTTP/STCP 及新旧二进制组合；Proxy/Visitor 生命周期；
  Dashboard 关闭/开启都能取得统计且无重复 collector；监控、数据库及慢浏览器故障
  不阻塞转发；公开 API/SSE 字段裁剪、管理鉴权/CSRF、CSP 和输出转义；
  Linux amd64/arm64、systemd 恢复、备份以及 100/500 节点容量验收。
