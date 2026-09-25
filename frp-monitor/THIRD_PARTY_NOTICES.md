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
然后在临时工作树中应用 `patches/`，并映射本项目扩展源码。当前目录迁移与构建方案
以根目录 `README.md` 为准。

计划中的实质修改包括：

- 在 frpc 进程级生命周期中增加 Linux Telemetry、TCP 探测和独立 WSS 上报；
- 采集主机资源、网络、连接总览和本地 Proxy 状态；
- 在 frps 中增加与原 Dashboard/API 分离的接收、聚合、存储与查询模块；
- 在独立 Listener 提供基于 web-standard-kit 的网页、公开摘要和认证管理接口；
- 保持 frp wire protocol、数据转发路径以及原有管理功能不变。

实际引入代码后，应在每个补丁或 Overlay package 中保留适用的上游版权、许可证和
来源说明，并同步更新本文件中的“实质修改”列表。

## monitor-probe 设计参考

本轮仅调研以下 MIT 项目，未复制其实现代码：

- https://github.com/monitor-probe/agent
  - Commit: `cebc5383abb5963abeb722877a2d82c1a7aea5c6`
- https://github.com/monitor-probe/monitor
  - Commit: `fb4c4a4ce0b3a665ab7a4bd491d2dd447a78e6bc`

上述源码的版权声明为 `Copyright (c) 2026 stqfdyr`。后续如移植源码、测试或其他
受版权保护材料，须随材料保留原版权声明与 MIT 许可证全文，并更新本文件。

## web-standard-kit

网页计划复用本仓库 `web-standard-kit/`，参考快照为
`6d41d588b3537b2d2460d73f999d958fd37eb00d` 中的该目录。
本轮未复制网页资源；实际引入时记录所用文件与版本，并保留适用的来源和许可说明。
