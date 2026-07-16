# 故障恢复 Runbook

前置：Manager 是单写者控制器。任何直接改库/恢复操作前，**先停 Manager**（`systemctl stop sbm-manager`）以保证单写。

## 1. Manager 进程崩溃

- 数据面不受影响：已部署的 sing-box 与已下发的 SSM 身份继续工作（Agent 被动、自持）。
- 恢复：重启 Manager。启动即跑迁移（幂等）、引导 PKI（幂等）、声明式 reconcile 扫描回填 active Entry 用户。
- 计量：Manager 恢复后继续读 SSM 累计统计；`traffic_batches` PK 保证不重复计量。
- **进行中的部署**：若崩在结算屏障中途，旧进程仍服务（未被停）；下次部署自愈（幂等重驱动）。检查 `deployments` 表非终态行，必要时重新触发发布。

## 2. Agent 离线

- 该 Entry 的 sing-box 与已注入用户继续工作；Manager 对其发布/reconcile/统计暂停并在 `/api/health` 显示 degraded/critical，`health_events` 记 `stats_stale`。
- 其他 Entry 不受影响（每 Entry 隔离，单点失败不阻塞他者）。
- 恢复：修复网络/重启 Agent；Manager 下一轮轮询恢复在线，计量续接（新 boot id 从 0 起，结构性无负增量）。

## 3. SQLite 恢复（从备份）

> 备份/恢复 CLI 属备份领域（`.sbmbak` 加密容器 + 独立 BK）。当前版本尚在实施，以下为通用一致性恢复流程。

1. 停 Manager。
2. 用 SQLite 一致快照（`VACUUM INTO` 产物或 `.sbmbak` restore）替换 `DATABASE_PATH`；**先删 `-wal`/`-shm`** 再放入恢复文件。
3. 确认 env 中 `ENCRYPTION_MASTER_KEY`（及历史 `_V{n}`）与备份时一致——否则信封密文不可解。
4. 启动 Manager，`/readyz` 应 200；抽查 `/api/hosts`、`/api/traffic/users` 与订阅解封是否正常。
5. Agent 信任关系随库恢复（`agent_certificates.trust_status`）；如证书已过期见 §5。

## 4. 主密钥丢失或疑似泄露

- **丢失**：若无用 BK 包裹主密钥的备份，则库内全部业务密钥（CA/uPSK/配置）**不可恢复**。这是不可逆结果——务必按部署文档异地保管主密钥与备份 BK。
- **疑似泄露 → 轮换**（在线、可续跑）：
  1. 备份当前库（保护级）。
  2. env 增 `ENCRYPTION_MASTER_KEY_V{old}=<旧>` + 新 `ENCRYPTION_MASTER_KEY=<新>` + `ENCRYPTION_MASTER_KEY_VERSION=<新号>`，重启 Manager（ring 同时装新旧）。
  3. 运行 `sing-box-manager key-rotation run` 把全部信封密文 re-seal 到新版本。
  4. `key-rotation status` 显示三表待迁移全 0 后，方可从 env 删除 `ENCRYPTION_MASTER_KEY_V{old}`（门禁：非零拒绝退休）。

## 5. 证书过期 / 吊销

- **Agent 服务端证书临期**：`/api/health` 与 `health_events` 提示；重签发新 enrollment 并重装 Agent（当前无热重载，需重启 Agent；平滑热切属 cert-rotation 编排半边，尚未实现）。
- **Manager 客户端证书**：当前版本不提供在线轮换（编排屏障半边未实现）。如证书临期，规划维护窗口：重签 + 重发全体 enrollment + 重启，避免把自己锁在门外（务必先让全体 Agent 信任新证书再弃旧）。
- Agent 证书已过期会 fail-fast 拒绝启动（不静默降级）——先补发有效 enrollment。

## 6. 结算屏障中断

- 现象：某部署停在 `awaiting_meter_ack`；旧 sing-box 进程仍在服务（约束保证 ack 前不停旧）。
- 恢复：重新驱动该部署（幂等）。Manager 会重新 GET 最终批（`traffic_batches` PK 去重）、重发 meter-ack；Agent phase B 以 `active_revision==revision` 为幂等门，绝不二次重启/膨胀 epoch。
- 若长期卡住：确认 Agent 在线、`entry_locks` 无僵尸锁（有租约自动过期），必要时重新发起发布。

## 巡检清单

- `GET /readyz` → 200；`GET /api/health`（登录后）status=ok。
- `GET /metrics`：`sbm_agents_online == sbm_agents_total`、`sbm_entries_stale == 0`、`sbm_deployments{status="failed"} == 0`。
- 审计：`GET /api/audit` 有无异常登录/越权（`admin.login.fail`/`authz.denied`）。
- 备份新鲜度与异地 BK 可用性（备份领域落地后）。
