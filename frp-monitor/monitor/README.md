# frp-monitor Monitor

frps + Go 监控模块的扩展源码，构建时由 `scripts/assemble.sh` 映射到临时上游树
`extension/frpmonitor/monitor/`，共享上游 go module。整体架构、身份规则与存储设计见
根目录 [README.md](../README.md)（§1、§4、§5、§6）。

## 包布局（P1 实时闭环、P2 持久化与探测已落地）

- `service/`：对外唯一入口 `Start(ctx, Config, FRPRegistrySource)`，组合认证、存储、
  WSS 接收与 API/SSE；FRP Registry 只读适配器接口（`FRPRegistrySource`）由上层胶水实现，
  本包按 2s 周期轮询快照。ctx 取消后 5 秒内优雅关闭；运行期错误只记日志并降级，
  不拖停 frps。`Config.DataDir` 非空时启用 SQLite 持久化（初始化失败降级为仅内存），
  `Config.RetentionDays` 默认 7、范围 [1,365]。Dashboard 统计 collector 的显式启用
  （根 README §4）随生命周期补丁落地。
- `auth/`：节点凭据（token sha256 摘要、常量时间比较、按 mtime 自动重载）与
  管理端会话（HMAC 令牌、12 小时、`fm_admin` Cookie：HttpOnly + SameSite=Strict，
  TLS 下加 Secure）；写操作 Origin 校验（CSRF）。
- `store/`：节点最新状态内存快照；会话取代与序号严格递增；新鲜度
  `max(10s, 3×ReportInterval)`（以服务端接收时间为准）；FRP 注册表对账（只读，
  冲突不自动绑定）；变更 hub（SSE 消费方做 1 秒合并）。P2 子模块：
  - `db.go`：SQLite（modernc，WAL + busy_timeout，`PRAGMA user_version` 迁移，
    当前 v1）。全部写操作走单 worker 有界队列（4096，满则丢弃并限频记日志）；
    每小时清理早于保留期的 `metrics_1m` 与 `probe_results`。
  - `aggregate.go`：分钟聚合器（窗口内样本均值，unknown 质量组记 NULL 不纳入，
    重启重叠写按样本数加权合并）。
  - `traffic.go`：累计流量内存 tracker（首见只建基线、同范围差分、范围变化或
    回退只重建基线不清累计、日归属按服务端接收时间 UTC）；DB 侧 `applyTrafficTx`
    基线与累计同事务。
  - `probe.go`：探测任务版本（服务端单调递增，恢复拒绝回退）与每任务近 15 分钟
    滑窗统计（last_latency_ms 取窗口内最近成功样本，fail_rate 为失败/样本）。
- `ingest/`：`/agent/v1/ws` 接收端点：Bearer 认证（401 不升级）、5 秒 hello 超时、
  帧上限 256KiB、pong 保活；消息级错误回 JSON-RPC 错误继续，会话级错误断开。
  hello 声明 `ping` 能力的连接下发当前任务列表（等 ack 5s；离线期间变更上线即
  补发，PUT 变更版本 +1 全量重发）；`ping.result` 校验会话/序号后更新滑窗并落库。
- `api/`：公开/管理 JSON API 与 SSE（`/events/public`、`/events/admin`）。
  公开 DTO 服务端裁剪，绝不含 hostname/IP/kernel/boot_id/iface/local_addr/session_id
  及探测 target；大整数一律十进制字符串，质量 unknown 或缺失输出 null。P2 端点：
  节点 DTO 增加 `probes`/`traffic`；`GET /api/{public,admin}/v1/nodes/{id}/metrics`
  （range=1h|6h|24h|7d 固定点数，缺口 null，无 DB 时 `{"enabled":false}`）；
  `GET|PUT /api/admin/v1/nodes/{id}/probe-tasks`（写操作 Origin 校验）；
  `GET /api/admin/v1/nodes/{id}/traffic/daily`（仅管理端）。通用安全响应头
  （CSP self + connect-src self、nosniff、DENY）。

## 静态资源与版本

页面由 `../web/`（`extension/frpmonitor/web`）以 `web.Static()` 嵌入提供；
`api.NewHandler` 接受 `static fs.FS`，为空时页面路由降级 503 而不 panic。
hello 的 `server_version` 为 `frpversion.Full()`（`shared/version` 的 init
已注入 `frp-monitor/<版本>` 后缀，生命周期补丁提供 `Suffix`/`Base()`）。
