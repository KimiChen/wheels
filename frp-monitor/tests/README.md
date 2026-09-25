# Tests

本目录保存脱敏 fixture 与后续的协议、集成验收测试。协议 golden 随代码放在
`shared/*/testdata/`（由 `go test ./extension/...` 逐字节比对）；本目录的
`fixtures/proc/` 为 Linux 采集算法的对照样本，详见 [fixtures/README.md](fixtures/README.md)。

随实施阶段至少需要覆盖：

- Facts/Metrics 逐字段对照 fixture：首样本、MemAvailable 为 0、读取失败、
  特殊挂载、接口过滤、计数器回退、进程/主机重启；
- TCP 探测成功/拒绝/超时、DNS 超时缺样、多地址回退、任务更新、资源限额；
- `session_id + sequence` 重复、乱序、旧会话迟到不覆盖新会话，凭据轮换终止旧会话；
- 同名 clientID 在不同 FRP user 下不串节点；
- 报告大小限制、认证失败、畸形 JSON 与非有限数拒绝；
- 监控/数据库/慢浏览器不阻塞 FRP 数据面；
- 公开 DTO 无私有字段、管理写操作鉴权、CSP 与页面输出转义。
