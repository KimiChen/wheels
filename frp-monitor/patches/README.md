# Patches

本目录用于保存对固定 frp 上游提交中“既有文件”的最小修改。

约定：

- 文件按 `0001-...patch`、`0002-...patch` 顺序命名；
- `series` 每行记录一个补丁文件名，并决定应用顺序；
- 补丁必须能在 `upstream.lock` 指定的源码 Commit 上干净应用；
- 不把完整上游文件当作补丁提交；
- 不在补丁中混入格式化、重命名或与 Telemetry 无关的修改；
- 每次升级上游后重新生成并验证全部补丁。

`0001-monitor-lifecycle.patch` 接入配置、版本命令、client/server 异步监控生命周期，
增加只读 Proxy 快照与内存 metrics collector 的线程安全单次注册。
监控开关默认关闭；FRP wire protocol、转发逻辑、原生 `-v` 输出与 Dashboard 保留。
客户端源快照保持上游“先各源排除 disabled，再合并启用项”的优先级，并补充仅有
disabled 配置的名称供展示；新增方法不改变原生 Load。补丁附带回归测试。
每次准备源码都从已验证的固定版本重新组装，不能在上一次已打补丁的目录上重复应用。
