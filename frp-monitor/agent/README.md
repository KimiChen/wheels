# Agent

原 `overlay/client/telemetry/` 已迁到本目录。当前 P0 尚无采集器或运行服务；
具体实施规格以根目录 [README.md](../README.md) 为准。

后续实现 `service/collect/probe/transport`，由构建工具映射到固定 FRP 源码的
`extension/frpmonitor/agent`，共享上游 Go module。通过窄接口读取 FRP 状态，
本包不得反向 import 上游 `client` 根包而造成循环依赖。

P1 生命周期在首次 FRP 登录前启动，每进程仅一个实例，reload 更新配置快照。
默认每秒采样，经独立 WSS 主动上报；无 Proxy 或 FRP 登录失败也能采集。
CPU/网速首样本和读取失败应保留未知状态，不能用 0 冒充有效测量。
P2 增加受限 TCP 探测。协议和质量规则见 [shared](../shared/README.md)。
