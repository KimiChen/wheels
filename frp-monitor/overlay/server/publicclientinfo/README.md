# frps Public Client Info Overlay

预留给 frps 独立公开客户端信息服务。当前仅有设计文档，没有实现代码。

实现必须与原 Dashboard/API 使用不同 Listener，只保存每个客户端的最新内存快照，
并严格隔离公开读取和经过认证的 Telemetry 写入。
