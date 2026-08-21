# 文档索引

- [`frps.md`](frps.md)：当前 MVP 的主设计文档，定义 frps 独立公开端口、Telemetry
  Receiver、内存 Registry、公开页面和安全边界。
- [`frpc.md`](frpc.md)：frpc Telemetry/管理模块的完整设计。当前 MVP 仅采用其中与
  60 秒指标采集、Proxy 本地端点采集和可靠上报有关的部分。

当两份文档在当前实现范围上存在差异时，以根目录 `README.md` 和 `frps.md` 的范围
边界为准；远程配置下发和 Proxy/Visitor 管理需要单独立项。
