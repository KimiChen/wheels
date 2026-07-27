# 故障恢复手册

Controller 是 SQLite 的唯一写者。备份、替换数据库、轮换主密钥或人工修复状态前，应先停止
Controller。

## 日常备份

至少备份以下内容：

- 声明式配置目录。
- Controller SQLite 状态库的一致快照。
- 当前和仍在使用的历史主密钥。
- 每台 Agent 的 enrollment 文件。
- systemd 单元、反向代理配置和必要的防火墙规则。

主密钥与数据库快照应分开保管。数据库处于 WAL 模式时，不要直接复制正在写入的主文件；可在停止
Controller 后复制，或使用 SQLite `VACUUM INTO` 创建一致快照：

```bash
sqlite3 /var/lib/sing-box-manager/state.db \
  "VACUUM INTO '/var/backups/sing-box-manager/state-$(date +%F).db';"
```

备份完成后应限制权限、加密离机保存，并定期在隔离环境演练恢复。

## Controller 进程崩溃

已部署的 Agent 和 sing-box 会继续运行，订阅是否可用取决于 Controller 是否已恢复。

1. 检查服务日志和磁盘空间。
2. 确认数据库及主密钥文件仍可读。
3. 重启 Controller。
4. 检查 `/readyz`、`status --json` 和失败部署记录。
5. 对中断的部署重新执行 `apply --deploy`；命令和 Agent 操作具有幂等保护。

不要通过启动第二个 Controller 并指向同一个 SQLite 来“临时容灾”。

## Agent 离线

已运行的 sing-box 通常继续服务，但该节点无法接收新配置、回滚或上报状态。

1. 检查 Agent 服务、证书时间、系统时间和到 Controller 的网络。
2. 确认 `39736/tcp` 防火墙只允许 Controller 来源。
3. 修复后重启 Agent。
4. Agent 会根据本地活动版本恢复 sing-box；Controller 后续轮询会重新确认状态。
5. 使用 `status --json` 检查活动修订、错误和在线状态，再重试部署。

如果 enrollment 丢失或证书失效，重新生成 enrollment、以安全通道安装并重启 Agent。

## sing-box 启动失败

Agent 在切换版本前会执行 `sing-box check`。校验失败时不会把错误配置设为活动版本。

1. 查看 Agent 日志中的校验错误。
2. 在 Controller 运行 `plan`，确认拓扑、监听器和授权关系。
3. 核对 Agent 的 `SINGBOX_BIN` 与实际二进制路径。
4. 修正声明式配置后重新执行 `apply --deploy`。
5. 如果活动版本也无法启动，使用最近可用修订回滚；不要手工覆盖 Agent 的运行目录。

## 从数据库快照恢复

1. 停止 Controller。
2. 备份当前故障现场，包括主数据库、`-wal` 和 `-shm` 文件。
3. 将一致快照恢复到新的临时路径。
4. 确认 `ENCRYPTION_MASTER_KEY` 及仍被引用的历史版本与备份时一致。
5. 以临时 `DATABASE_PATH` 启动或运行 `status --json` 验证数据。
6. 验证通过后再替换生产数据库并启动 Controller。
7. 检查 `/readyz`、拓扑、用户、Agent 信任状态和活动修订。

不要在未验证快照和主密钥匹配前覆盖唯一的生产数据库。

## 主密钥丢失或疑似泄露

### 主密钥丢失

若没有可用的主密钥备份，数据库中的 CA 私钥、业务凭据和配置制品不可解密，无法从数据库恢复。
此时应重建控制面 PKI、重新 enrollment 所有 Agent，并轮换全部用户和节点凭据。

### 主密钥轮换

在维护窗口内执行：

1. 停止 Controller，并创建数据库一致快照。
2. 保留旧密钥为 `ENCRYPTION_MASTER_KEY_V<旧版本>`。
3. 设置新的 `ENCRYPTION_MASTER_KEY` 和 `ENCRYPTION_MASTER_KEY_VERSION`。
4. 执行 `sing-box-manager key-rotation run`。
5. 用 `sing-box-manager key-rotation status` 确认所有待迁移项为零。
6. 启动 Controller 并完成读写检查。
7. 观察一个维护周期后，再从安全环境中移除旧密钥。

轮换主密钥只重新加密库内数据；如果业务凭据本身可能泄露，还必须轮换相应协议凭据和订阅令牌。

## 证书过期或吊销

- Agent 证书临期：生成新的 enrollment，在对应主机安全替换后重启 Agent。
- Agent 证书已失效：Controller 会拒绝连接；重新 enrollment 后再恢复部署。
- Controller 客户端证书需要更换：安排维护窗口，确保 Agent 先信任新身份，再停用旧身份。
- CA 泄露：视为整个 Agent 信任域失守，重建 CA、重新 enrollment 全部 Agent，并吊销旧信任。

证书操作后，应逐台确认主机身份、证书指纹和 Agent 在线状态。

## 部署中断

部署状态和幂等标识保存在 Controller 与 Agent 本地。网络中断或进程重启后：

1. 先恢复 Controller 与相关 Agent。
2. 查看 `status --json` 和双方日志，确认活动修订。
3. 对相同期望配置重新执行 `apply --deploy`。
4. 如果只有部分节点成功，不要手工拼接版本；让部署流程重试或统一回滚。
5. 验证所有节点后，再交付新的订阅。

## 恢复验收

- `/healthz` 和 `/readyz` 返回成功。
- `status --json` 中所有预期 Agent 在线且活动修订一致。
- `/metrics` 未出现持续增长的失败部署或离线 Agent。
- 抽样运行订阅导入、TCP、UDP 和各转发链出口验证。
- 检查审计记录、证书到期时间、磁盘空间和备份新鲜度。
- 将故障原因、恢复时间点和仍需轮换的凭据记录到受控运维系统，避免粘贴秘密。
