# Third-Party Notices

## frp

本项目计划通过补丁和 Overlay 修改以下上游项目：

- Project: frp
- Source: https://github.com/fatedier/frp
- Version: `v0.71.0`
- Tag object: `40adeed73b51e7ee1766d7cfb15d02ba9431ba2b`
- Source commit: `4a23aa181c1d7e28eecaa8216024ed753b9d27c8`
- License: Apache-2.0

完整上游源码不保存在本仓库中。准备构建时，工具必须取得上述固定提交并验证身份，
然后在临时工作树中应用 `patches/` 和 `overlay/`。

计划中的实质修改包括：

- 在 frpc 进程级生命周期中增加每 60 秒执行的 Linux Telemetry 采集与 HTTPS 上报；
- 采集系统资源、网络、连接总览和本地 Proxy 端点信息；
- 在 frps 中增加与原 Dashboard/API 分离的 Telemetry Receiver 和内存 Registry；
- 在 frps 的独立 Listener 上提供公开、只读的客户端信息页面；
- 保持 frp wire protocol、数据转发路径以及原有管理功能不变。

实际引入代码后，应在每个补丁或 Overlay package 中保留适用的上游版权、许可证和
来源说明，并同步更新本文件中的“实质修改”列表。
