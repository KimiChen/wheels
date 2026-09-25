# Patches

本目录用于保存对固定 frp 上游提交中“既有文件”的最小修改。

约定：

- 文件按 `0001-...patch`、`0002-...patch` 顺序命名；
- `series` 每行记录一个补丁文件名，并决定应用顺序；
- 补丁必须能在 `upstream.lock` 指定的源码 Commit 上干净应用（`patch --fuzz=0`）；
- 不把完整上游文件当作补丁提交；
- 允许补丁以「新增文件」形式加入上游包内的薄接入胶水（如生命周期钩子、
  配置类型），胶水只做类型转换与转发，业务代码仍放 `extension/frpmonitor/`；
- 不在补丁中混入格式化、重命名或与监控扩展无关的修改；
- 每次升级上游后重新生成并验证全部补丁。

当前补丁：

| 补丁 | 内容 |
|---|---|
| 0001-version-suffix | `pkg/util/version` 增加 `Suffix`/`Base()`，扩展版本后缀注入 |
| 0002-monitor-config-types | `pkg/config/v1` 增加 `[monitor]` 配置类型与校验（frpc/frps） |
| 0003-client-monitor-hook | frpc 生命周期钩子：`Service.Run` 首次登录前启动监控 |
| 0004-server-monitor-hook | frps 生命周期钩子 + monitor 开启时强制启用内存统计 collector |

当前尚未进入实现阶段，因此 `series` 中没有补丁。
