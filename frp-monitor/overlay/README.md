# Overlay

本目录只保存相对于上游 frp 新增的文件。对上游已有文件的修改应放在 `patches/`。

预留区域：

- `client/telemetry/`：frpc 进程级采集、调度、报告和状态模型；
- `server/publicclientinfo/`：frps Receiver、Registry、Listener 和页面数据模型；
- `web/`：将由 frps 独立 Listener 提供的静态页面资源。

新增源码时尽量保持 package 独立，并将对上游生命周期的接入点压缩到少量补丁中。
