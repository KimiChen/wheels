# 部署指南

本文给出单 Controller、多 Entry/Node 的类 Unix 生产部署流程。端口和环境变量的精确定义见
[参考手册](reference.md)。

## 部署拓扑

推荐每个角色使用独立 systemd 服务：

```text
Controller 主机
  └─ sing-box-manager controller

每台 Entry/Node
  └─ sing-box-manager agent
       └─ 由 Agent 启动和管理 sing-box 子进程
```

不要再为同一数据面配置启动独立的 `sing-box.service`。Agent 需要拥有 sing-box 子进程，
才能在部署时完成受控重启、健康检查和回滚。

## 前置要求

- Controller 与 Agent 使用同一版本的 `sing-box-manager`。
- Controller 和所有 Agent 主机安装 sing-box `1.13.14`。
- Controller 可以访问每台 Agent 的 `39736/tcp`。
- Entry/Node 数据面地址彼此可达。
- 主机时间通过 NTP 同步，证书验证依赖正确时间。
- 所有真实配置、环境、数据库和 enrollment 文件权限受限。

检查：

```bash
sing-box version
sing-box-manager --version
```

## 构建与安装

在可信构建机执行：

```bash
cargo build --locked --release
install -m 0755 target/release/sing-box-manager /usr/local/bin/sing-box-manager
```

把同一二进制分发到 Controller 和全部 Agent 主机。正式升级前应核对构建产物 SHA-256。

每台运行 Controller 或 Agent 的主机创建固定的非登录账号：

```bash
useradd --system \
  --home-dir /var/lib/sing-box-manager \
  --shell /usr/sbin/nologin \
  sing-box-manager
```

若系统使用不同的账号管理命令，可创建等价的系统用户和同名组。不要让服务以 root 运行。

## Controller 文件布局

```text
/etc/sing-box-manager/
  controller.env
  config/
    config.toml
    servers.toml
    protocols.toml
    listeners.toml
    relays.toml
    users.toml

/var/lib/sing-box-manager/
  controller.db
  controller.db-wal
  controller.db-shm
```

建议权限：

```bash
install -d -o root -g sing-box-manager -m 0750 /etc/sing-box-manager
install -d -o root -g sing-box-manager -m 0750 /etc/sing-box-manager/config
install -d -o sing-box-manager -g sing-box-manager -m 0700 /var/lib/sing-box-manager
chown root:sing-box-manager /etc/sing-box-manager/controller.env
chown root:sing-box-manager /etc/sing-box-manager/config/*.toml
chmod 0640 /etc/sing-box-manager/controller.env
chmod 0640 /etc/sing-box-manager/config/*.toml
```

## Controller 环境

`/etc/sing-box-manager/controller.env`：

```text
DATABASE_PATH=/var/lib/sing-box-manager/controller.db
MANAGER_LISTEN=127.0.0.1:9736
ENCRYPTION_MASTER_KEY=<openssl rand -base64 32 的结果>
ENCRYPTION_MASTER_KEY_VERSION=1
SINGBOX_BIN=/usr/local/bin/sing-box
RUST_LOG=info,sqlx=warn
```

主密钥必须单独异地备份。丢失后数据库内的 CA、PSK 和配置 artifact 无法恢复。

## Controller systemd

`/etc/systemd/system/sing-box-manager-controller.service`：

```ini
[Unit]
Description=sing-box-manager Controller
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sing-box-manager
Group=sing-box-manager
EnvironmentFile=/etc/sing-box-manager/controller.env
WorkingDirectory=/etc/sing-box-manager
ExecStart=/usr/local/bin/sing-box-manager controller \
  --config /etc/sing-box-manager/config/config.toml
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/sing-box-manager

[Install]
WantedBy=multi-user.target
```

首次同步前可以暂不启动 Controller；enrollment 签发会自行引导 PKI。需要 Agent 轮询和部署时再启动：

```bash
systemctl daemon-reload
systemctl enable --now sing-box-manager-controller
journalctl -u sing-box-manager-controller -f
```

## 准备清单和首次同步

在项目目录或部署配置目录中：

```bash
sing-box-manager plan --config /etc/sing-box-manager/config/config.toml
sing-box-manager apply --config /etc/sing-box-manager/config/config.toml
sing-box-manager status --json
```

保存首次 `apply` 输出的用户 token。不要直接把含 token 的 JSON 输出上传到日志平台。

## Agent enrollment

为每个 server ID 执行：

```bash
sing-box-manager enrollment issue \
  --config /etc/sing-box-manager/config/config.toml \
  --server entryA \
  --output /secure/staging/entryA.enroll.json
```

命令拒绝覆盖已有文件，并在 Unix 上创建 `0600` 文件。记录输出的 package fingerprint。

通过安全通道把二进制和 enrollment 包分发到目标主机：

```text
/usr/local/bin/sing-box-manager
/etc/sing-box-manager/agent.enroll.json
```

分发完成后删除 Controller 上的临时副本。不要通过聊天、工单或公开 CI artifact 传输。

## Agent 文件布局

```text
/etc/sing-box-manager/
  agent.env
  agent.enroll.json

/var/lib/sing-box-manager/
  agent.db
  config.json
  revisions/
  ssm-cache.json
```

enrollment 和 env 只能让 root 与服务账号读取；状态目录建议 `0700`。

以固定服务账号运行时，建议由 root 持有配置并授予服务组只读权限：

```bash
install -d -o root -g sing-box-manager -m 0750 /etc/sing-box-manager
install -d -o sing-box-manager -g sing-box-manager -m 0700 /var/lib/sing-box-manager
chown root:sing-box-manager /etc/sing-box-manager/agent.env
chown root:sing-box-manager /etc/sing-box-manager/agent.enroll.json
chmod 0640 /etc/sing-box-manager/agent.env
chmod 0640 /etc/sing-box-manager/agent.enroll.json
```

## Agent 环境

`/etc/sing-box-manager/agent.env`：

```text
AGENT_ENROLLMENT_PATH=/etc/sing-box-manager/agent.enroll.json
AGENT_STATE_PATH=/var/lib/sing-box-manager/agent.db
AGENT_CONFIG_DIR=/var/lib/sing-box-manager
AGENT_BIND_ADDRESS=0.0.0.0:39736
AGENT_SSM_ADDRESS=127.0.0.1:49736
SINGBOX_BIN=/usr/local/bin/sing-box
RUST_LOG=info,sqlx=warn
```

如果 Controller 与 Agent 同机，可以把 `AGENT_BIND_ADDRESS` 设为 `127.0.0.1:39736`，并确保
清单中的数据面地址能让 Controller 正确连接该 Agent。

## Agent systemd

`/etc/systemd/system/sing-box-manager-agent.service`：

```ini
[Unit]
Description=sing-box-manager Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sing-box-manager
Group=sing-box-manager
EnvironmentFile=/etc/sing-box-manager/agent.env
ExecStart=/usr/local/bin/sing-box-manager agent
Restart=on-failure
RestartSec=3
KillMode=control-group
TimeoutStopSec=15
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/sing-box-manager

[Install]
WantedBy=multi-user.target
```

`KillMode=control-group` 很重要：Agent 退出时必须同时结束其 sing-box 子进程。Agent 再次启动时，
若本地已有 active revision，会从 `config.json` 恢复 sing-box。

启动：

```bash
systemctl daemon-reload
systemctl enable --now sing-box-manager-agent
journalctl -u sing-box-manager-agent -f
```

## 带外授信

在目标主机确认 enrollment 文件属于预期 Host，核对 Controller 输出的 fingerprint 后执行：

```bash
sing-box-manager enrollment trust \
  --server entryA \
  --fingerprint '<已核对的 fingerprint>'
```

随后等待 Controller 轮询。首次部署时没有 active revision，sing-box 尚未运行是允许状态；
Agent 必须已授信、在线且状态新鲜。

## 首次部署

确认：

```bash
sing-box-manager status --json
curl -fsS http://127.0.0.1:9736/readyz
```

执行：

```bash
sing-box-manager apply \
  --config /etc/sing-box-manager/config/config.toml \
  --deploy
```

成功后检查：

```bash
sing-box-manager status --json
curl -fsS http://127.0.0.1:9736/metrics
systemctl status sing-box-manager-agent
```

首次部署没有旧进程和旧累计统计，因此不进入结算屏障。后续 Entry revision 切换会先结算旧 epoch。

## 防火墙

| 主机 | 入站规则 |
|---|---|
| Controller | `9736/tcp` 默认只回环；如需订阅，通过 TLS 反代 |
| Entry | `19736/tcp+udp` 允许客户端；`39736/tcp` 仅 Controller |
| Node | `29736/tcp+udp` 仅直接上游；`39736/tcp` 仅 Controller |

`49736/tcp` 必须只监听 `127.0.0.1`。不要把 Agent 或 SSM 端口直接暴露给互联网。

## TLS 反向代理

公网只应代理订阅路径。健康和指标保留在内网或监控网络。例如反向代理策略：

```text
/sub/*  → 127.0.0.1:9736
其他路径 → 拒绝
```

订阅 token 会出现在 URL 中：

- 禁止访问日志记录完整路径。
- 禁止上游缓存。
- 强制 HTTPS。
- 不把订阅地址放入公开网页、截图或工单。

## 配置变更

每次变更：

```bash
sing-box-manager plan --config /etc/sing-box-manager/config/config.toml
sing-box-manager apply --config /etc/sing-box-manager/config/config.toml
sing-box-manager status
sing-box-manager apply --config /etc/sing-box-manager/config/config.toml --deploy
```

如果新增用户，先保存普通 `apply` 的 token，再执行部署。

## 升级

1. 阅读 [变更记录](../CHANGELOG.md)。
2. 创建 Controller SQLite 一致性备份并备份主密钥。
3. 在测试环境运行 `cargo test --locked` 和配置 `plan`。
4. 先升级 Controller 二进制，再逐台升级 Agent。
5. 保持 Controller/Agent 主版本一致。
6. 观察 `/readyz`、`/metrics` 和 Agent 日志。

数据库迁移在打开状态库时自动执行，不能降级到不理解新 schema 的旧二进制。

## 备份

必须分别备份：

- SQLite 一致性快照。
- 当前及轮换期历史主密钥。
- Controller 配置。
- Agent enrollment 文件。

SQLite 使用 `VACUUM INTO` 或 SQLite Backup API，不直接复制正在写入的数据库文件。具体恢复流程见
[故障恢复](disaster-recovery.md)。
