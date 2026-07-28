# 故障排查

排查时优先使用：

```bash
sing-box-manager plan --config config/config.toml
sing-box-manager status --json
curl -i http://127.0.0.1:9736/healthz
curl -i http://127.0.0.1:9736/readyz
RUST_LOG=debug sing-box-manager controller --config config/config.toml
```

不要把完整 `status --json`、订阅 URL、enrollment、真实配置或数据库上传到公开 Issue。

## plan 报 include 错误

常见原因：

- `config.toml` 缺少 `includes`。
- include 使用绝对路径或包含 `..`。
- 同一文件重复 include。
- 两个文件重复定义 `[servers]`、`[protocols]` 等顶层字段。
- 实际复制的文件名和 `includes` 不一致。

检查：

```bash
sed -n '1,120p' config/config.toml
find config -maxdepth 1 -type f -print | sort
```

## plan 报引用不存在

引用顺序：

```text
listener.server → servers
relay.listener → listeners
relay.chain[] → servers
user.relays[] → relays
```

名称区分大小写。修改 ID 后需要同步修改所有引用。

## plan 报端口或 listener 冲突

当前版本固定：

- Entry `19736`
- Node `29736`
- Agent `39736`
- SSM `49736`

同一 server 的 `19736` 只能有一个 listener。多条 Route 共享 listener，不能为每条 Route
重复声明一个相同端口的 listener。

## 缺少 DATABASE_PATH

除 `plan` 外，涉及状态库的命令都需要：

```bash
export DATABASE_PATH="$PWD/state/controller.db"
```

项目不会自动读取 `.env`。systemd 使用 `EnvironmentFile=`，shell 需要显式加载。

## 主密钥错误

现象：

- `缺少 ENCRYPTION_MASTER_KEY`
- base64 解码失败
- 长度不是 32 字节
- `缺密钥版本`
- 解封失败

生成新库主密钥：

```bash
openssl rand -base64 32
```

已有数据库不能随意更换主密钥。轮换时必须同时提供旧版本：

```text
ENCRYPTION_MASTER_KEY=<新密钥>
ENCRYPTION_MASTER_KEY_VERSION=2
ENCRYPTION_MASTER_KEY_V1=<旧密钥>
```

然后运行 `key-rotation run`。如果旧密钥已经丢失，旧密文无法恢复。

## apply --deploy 拒绝首次用户

这是 token 保护，不是错误。先执行：

```bash
sing-box-manager apply --config config/config.toml
```

安全保存新用户 token，再执行：

```bash
sing-box-manager apply --config config/config.toml --deploy
```

## VLESS 用户的 quotaBytes 被拒绝

VLESS 当前没有 SSM 式 per-user 计量，因此授权 VLESS relay 的用户只能使用：

```toml
quotaBytes = 0
```

若同一用户同时授权 Shadowsocks 与 VLESS relay，也必须保持 `0`。需要配额时，将 VLESS
线路授权给独立的无配额用户，或只保留 Shadowsocks relay。

## 找不到 sing-box

默认从 `PATH` 查找：

```bash
sing-box version
```

不在 `PATH` 时：

```bash
export SINGBOX_BIN=/usr/local/bin/sing-box
"$SINGBOX_BIN" version
```

Controller 在 `apply --deploy` 时需要它；Agent 在 check、run 和启动恢复时需要它。

## sing-box check 失败

Controller 和 Agent 都会 check。常见原因：

- sing-box 版本与 `singboxVersion` 目标不一致。
- 所选 Shadowsocks 方法不被本机版本支持。
- 端口已经被其他进程占用。
- 使用了当前版本尚未实现的配置。

先确认：

```bash
"${SINGBOX_BIN:-sing-box}" version
```

check 输出会对已识别的 password/username 做脱敏，但仍不要直接粘贴完整日志到公开渠道。

## enrollment issue 拒绝输出文件

命令故意不覆盖文件。更换一个不存在的路径，或先人工确认旧文件已经安全归档/销毁。

每次重新 issue 都会生成新证书，并把信任状态重置为 `pending`，需要重新带外核对和 trust。

## fingerprint 不匹配

只接受该 Host 最近一次 enrollment 的 package fingerprint。确认：

- `--server` 没有写错。
- 没有人在此后重新执行 issue。
- 复制时没有首尾空格。
- 核对的是 package fingerprint，不是证书 SPKI 或文件 SHA-256。

无法确认时不要授信，重新签发并通过独立渠道核对。

## 部署门禁失败

返回原因：

| 原因 | 含义 | 处理 |
|---|---|---|
| `no_agent` | 没有 Agent 登记 | 先普通 apply，再 enrollment |
| `untrusted` | 证书 pending/revoked | 核对 fingerprint 后 trust |
| `offline` | 最近轮询连接失败 | 检查 Agent 服务、DNS、防火墙和 mTLS |
| `stale` | 上次成功轮询超过新鲜度窗口 | 等待轮询或排查 Controller 后台循环 |
| `cert_expiring` | 证书不存在、过期或七天内到期 | 重新签发 enrollment |
| `singbox_down` | 已有 revision 但 SSM/进程不可用 | 检查 Agent 和 sing-box 日志 |

首次部署没有 current revision 时，允许 sing-box 尚未运行；其他门禁仍必须通过。

## Agent 显示 offline

目标主机检查：

```bash
systemctl status sing-box-manager-agent
journalctl -u sing-box-manager-agent --since -30min
ss -lntp | grep 39736
```

Controller 主机检查 TCP 可达性，但不要用普通 curl 判断 mTLS Agent：

```bash
nc -vz <agent-address> 39736
```

若 TCP 可达但仍失败，重点检查：

- enrollment 是否属于当前 Host。
- Controller 和 Agent 是否使用同一套 CA。
- 证书是否过期。
- Agent 是否只允许旧 Controller SPKI。
- 系统时间是否正确。

## 已有 revision 但 singbox_down

Agent 正常 systemd 停止时必须同时结束其子进程，服务文件应包含：

```ini
KillMode=control-group
```

Agent 再启动时会从 `AGENT_CONFIG_DIR/config.json` 恢复 active revision。若恢复失败：

```bash
"${SINGBOX_BIN:-sing-box}" check \
  -c /var/lib/sing-box-manager/config.json
journalctl -u sing-box-manager-agent -b
```

不要同时启动独立 `sing-box.service`，否则 Agent 部署时可能发生端口冲突。

## 部署处于 awaiting_meter_ack

表示 Entry 正在等待旧运行 epoch 的最终流量批次入账。检查：

- Controller 是否仍运行。
- Agent 与 Controller 的 mTLS 是否可达。
- Agent 本地数据库和磁盘是否可写。
- `entry_locks` 租约是否最终过期。

不要手工删除 Agent outbox 或 Controller traffic batch。修复连接后重新驱动同一声明式部署，
幂等键会避免重复计量。

## 订阅返回 404

- token 不存在或已轮换。
- URL 被代理改写。
- 请求路径没有保留 `/sub/<token>`。

数据库只保存 token hash，无法从库中恢复原明文。只能显式轮换 token；当前首发 CLI 尚未提供
独立 token 轮换命令，需保管首次输出。

## 订阅为空

用户存在但代理集为空时检查：

- 用户是否 disabled、过期或超额。
- Route 是否已经 active。
- 授权是否属于该用户。
- SS Entry 部署后 reconcile 是否成功。
- VLESS Entry 是否已经重新 `apply --deploy`，使 UUID 进入当前 revision。

## /readyz 返回 503

Controller 无法查询 SQLite。检查：

- `DATABASE_PATH` 父目录权限。
- 磁盘空间和 inode。
- SQLite/WAL 文件所有者。
- 是否有异常进程长时间持有写锁。

## /metrics 暴露风险

默认依赖 `MANAGER_LISTEN=127.0.0.1:9736` 的回环边界。不要直接把整个 9736 暴露公网。
公网订阅应由 TLS 反代只转发 `/sub/*`，指标交给受控监控网络采集。

## 收集诊断信息

报告问题前可收集以下脱敏信息：

```bash
sing-box-manager --version
sing-box version
sing-box-manager plan --config config/config.toml --json
cargo test --locked
```

删除或替换：

- IP、域名、SSH 用户和路径。
- Host/server ID 中的组织信息。
- token、PSK、UUID、证书、指纹和数据库内容。
- 完整订阅 URL。
