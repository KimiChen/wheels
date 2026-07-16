# 威胁模型（Threat Model）

范围：`sing-box-manager` 控制面 + Agent + 订阅面。数据面（sing-box 本体、SS-2022/VLESS 协议安全）不在此列，
由 sing-box 及其协议保证。

## 资产

| 资产 | 保护目标 | 存放 |
|---|---|---|
| 主密钥 `ENCRYPTION_MASTER_KEY` | 机密（信封加密根） | 仅 env（`.env`/systemd），**不入库、不入 Git、不入日志** |
| CA 私钥（agent_ca / client_ca） | 机密 | `ca_keypairs` 信封密文 |
| 用户 uPSK / Entry serverPSK / Node PSK | 机密 | `credential_versions` 信封密文；下发时仅内存 + mTLS |
| 编译后 sing-box 配置（含 PSK） | 机密 | `config_artifacts` 信封密文；推送时仅内存 + mTLS，绝不落 `agent_commands` |
| 订阅 token | 机密（等价用户凭据） | 只存 sha256；明文一次性下发 |
| 管理员密码 | 机密 | Argon2id PHC（不可逆，非信封加密） |
| Agent 服务端证书 / Manager 客户端证书 | 完整性/真实性 | 公证书入库；Agent 私钥只在 enrollment 文件 |
| 会话 / CSRF token | 机密 | 只存 sha256；明文只在 Cookie / 登录响应 |
| 流量用量 / 审计日志 | 完整性 | SQLite（用量权威、审计 append-only） |

## 信任边界（三套认证面，互不串门）

1. **管理面（9736 `/api`）**：会话 Cookie（HttpOnly+SameSite=Strict[+Secure]）+ CSRF 同步器 token + RBAC（readonly/operator/admin）+ 敏感操作 re-auth。
2. **公开订阅面（9736 `/sub/{token}`）**：无会话；凭高熵 token（32B CSPRNG，库存 sha256）。安全响应头（CSP/no-store/noindex）。
3. **Agent 面（39736 mTLS）**：双向证书。Manager 侧 pin Agent 的 host_id URI-SAN（可选叶 SPKI）；Agent 侧 pin Manager 客户端证书 SPKI + client_ca 根。Agent 只被动监听、只执行预定义操作、只访问本机回环 SSM。

`/metrics`、`/healthz`、`/readyz` 为公开探针面：默认依赖 Manager 回环绑定；非回环暴露 `/metrics` 须设 `metrics_scrape_token`。

## 攻击面与缓解

| 攻击 | 缓解 |
|---|---|
| 未授权访问管理 API | 全 `/api` 经会话中间件；未登录 401；RBAC 越权 403；readonly 不可写。 |
| CSRF | 同步器 token（不落 cookie）+ SameSite=Strict，写方法双校验，常量时间比较。 |
| 会话劫持/固定 | 只存 token sha256；登录必发新 sid；改密/禁用即批量吊销；idle+absolute 双过期；改角色即时生效（每请求重载 admin.role）。 |
| 在线口令爆破 | Argon2id + 失败计数锁定；用户不存在也跑假校验（抗时序枚举）；登录恒定文案。 |
| 密钥泄露（偷库） | 全部业务密钥信封加密；主密钥不在库→单偷 SQLite 无法解密。 |
| 密钥泄露（偷运行机） | 主密钥在 env；配合备份独立 BK 异地保管，偷主密钥无法解历史备份（备份领域）。 |
| 中间人假冒 Manager | Agent pin Manager SPKI + client_ca 根；假 Manager 无对应私钥→握手失败。 |
| 中间人假冒 Agent | Manager pin host_id URI-SAN（+可选叶 SPKI）；host 混淆被拒。 |
| 命令注入 / 任意执行 | Manager 绝不下发 shell；Agent 只执行预定义操作（deploy/reconcile/rollback/stats/meter-ack…）。 |
| 明文密钥外泄（日志/审计/API） | 响应默认脱敏；审计 detail 不含密钥（enrollment 仅记指纹前缀）；deploy 明文不落 `agent_commands`；uPSK/stats 明文仅 mTLS+内存。 |
| 重放（部署/结算） | command_id 幂等（execute_once）；结算最终批经 `traffic_batches` PK 精确一次。 |
| 计量重复计费 | 增量 `max(0,cur-last)`；结算屏障 poll 交错不双计（详见结算屏障设计）。 |

## 残余风险（已知，按偏向用户/低概率排序）

- **主密钥丢失且无备份 → 全部业务密钥不可解**（不可逆）。缓解：备份把主密钥用独立 BK 包裹（备份领域，尚在实施）。
- **Manager 客户端证书轮换未按屏障执行可能失联**（cert-rotation 编排半边尚未实现；当前不提供在线换 Manager 证书）。
- **结算屏障 forced 尾字节有界少计**（偏向用户）。
- **带外 sing-box 重启（同逻辑 epoch）少计**（偏向用户）。
- **`/metrics` 非回环暴露未设 token → 泄露拓扑计数**（无密钥，但暴露规模信息）。运维须设 token 或仅回环。
- **单控制器约束**：不允许多个 Manager 并发写同一 SQLite（无分布式锁）。
