# sing-box-manager

`sing-box-manager` 是一个以拆分 TOML 为管理入口、以 sing-box 为数据面的多跳代理控制器。
管理员在本机声明服务器、入口、转发链和用户授权，通过 CLI 完成校验、状态同步、Agent 发牌、
部署和巡检。后台 Controller 负责 mTLS 编排、运行状态采集、Shadowsocks SSM 用户同步、
流量计量和订阅分发。

项目不提供 Web 管理台，也不对外暴露运行时管理 API。管理权限由本机系统账户、配置文件权限
和 SSH 边界承担。

当前版本：`0.1.0`，首个公开预览版本。

## 当前能力

| 能力 | 状态 | 说明 |
|---|---|---|
| Shadowsocks-2022 Entry | 可用 | 单端口 managed inbound，支持独立用户 uPSK |
| 公网多跳 Node | 可用 | Node 固定 SS-2022 中继入站，支持共享链路前缀 |
| 用户与线路授权 | 可用 | 每个“用户 × 转发链”生成独立身份和凭据 |
| 订阅 | 可用 | raw `ss://`、Clash/mihomo YAML、浏览器状态页 |
| Agent mTLS | 可用 | 双 CA、主机身份校验、指纹带外授信、吊销 |
| 配置发布 | 可用 | 编译、真实 `sing-box check`、Node→Entry、健康失败回滚 |
| 流量与配额 | 可用 | 按用户增量计量、周期配额、重启前结算屏障 |
| 主密钥轮换 | 可用 | 多版本解密、幂等 re-seal、旧密钥退休检查 |
| VLESS-Reality | 未完成 | 清单结构和用户 UUID 已预留，尚不能编译或部署 |
| SSH 自动装机 | 未完成 | SSH 字段已进入清单，当前仍需人工或配置管理工具装机 |
| 多 Controller | 不支持 | SQLite 为单写者状态库，只允许一个 Controller 写入 |

已验证的数据面版本为 sing-box `1.13.14`。`plan` 会对包含 VLESS listener 的清单给出警告，
`apply --deploy` 会在写数据库和连接 Agent 前拒绝这类清单，避免部分发布。

## 设计目标

- 声明式配置：真实配置只存在于本机 `config/*.toml`，仓库仅保留脱敏示例。
- 数据面独立：sing-box 始终作为独立进程运行，本项目不包含或链接 sing-box。
- 失败关闭：未知字段、无效引用、端口冲突和不支持协议都会在部署前失败。
- 密钥最小暴露：业务密钥和配置产物使用 XChaCha20-Poly1305 信封加密。
- Agent 被动：Agent 不主动连接 Controller，只接受通过 mTLS 验证的预定义命令。
- 可恢复发布：配置先 check，再原子替换；健康失败自动回滚到上一成功 revision。
- 计量一致性：使用幂等批次、运行 epoch 和重启前结算屏障降低重复或遗漏风险。

## 架构概览

```text
管理员
  │
  ├─ plan ───────────────► 合并 TOML、引用校验、端口和协议校验
  ├─ apply ──────────────► SQLite 期望状态、加密凭据、用户授权
  ├─ enrollment ─────────► Agent 证书签发、带外指纹核对、授信/吊销
  └─ apply --deploy ─────► 编译、sing-box check、门禁、Node→Entry 发布
                                  │
                                  ▼
                    Controller（单写者、无管理 API）
                       │                   │
                       │ mTLS :39736       └─ /sub /healthz /readyz /metrics
                       ▼
                 各主机 Agent
                       │ 本机受控操作
                       ▼
                    sing-box

客户端 ──► Entry :19736 ──► Node :29736 ──► … ──► 最终出口
```

固定端口：

- `19736`：Entry 客户端入站。
- `29736`：Node 中继入站。
- `39736`：Agent mTLS。
- `49736`：Entry 本机 sing-box SSM API，仅回环。
- `9736`：Controller 的订阅、健康检查和指标。

更完整的状态流、发布顺序和信任边界见
[架构与机制](docs/architecture.md)。

## 运行要求

- Rust `1.88` 或更高版本。
- sing-box `1.13.14`，Controller 执行部署检查以及每台 Agent 运行数据面时都需要。
- Linux 或 macOS。当前文件权限、进程控制和部署路径按类 Unix 系统设计。
- Controller 到所有 Agent 的 TCP 可达性。
- SQLite 由程序内置使用，无需单独安装数据库服务。

默认从 `PATH` 查找 `sing-box`。若二进制不在 `PATH`，设置：

```bash
export SINGBOX_BIN=/absolute/path/to/sing-box
```

## 快速开始

以下命令从 `sing-box-manager` 目录执行。

### 1. 构建

```bash
cargo build --release
./target/release/sing-box-manager --version
```

### 2. 准备本地配置

```bash
for name in config servers protocols listeners relays users; do
  cp "config/example.${name}.toml" "config/${name}.toml"
done
chmod 600 config/*.toml
```

示例清单默认只启用已支持的 Shadowsocks listener，可以直接通过 `--deploy` 的协议预检。
`example.protocols.toml` 中保留了未启用的 VLESS 目标模板，方便后续迁移。

编辑真实服务器地址和转发链后先执行无副作用检查：

```bash
./target/release/sing-box-manager plan --config config/config.toml
```

### 3. 准备 Controller 环境

```bash
umask 077
export DATABASE_PATH="$PWD/state/controller.db"
export ENCRYPTION_MASTER_KEY="$(openssl rand -base64 32)"
export ENCRYPTION_MASTER_KEY_VERSION=1
export MANAGER_LISTEN=127.0.0.1:9736
```

主密钥不能丢失，也不能写入 Git。项目不自动读取 `.env`；如使用 `.env`，请由 systemd、
容器编排器或 shell 显式加载。可参考 [.env.example](.env.example)。

### 4. 首次同步

```bash
./target/release/sing-box-manager apply --config config/config.toml
./target/release/sing-box-manager status
```

第一次 `apply` 会为新用户输出一次性订阅 token。立即安全保存；数据库只保存 token 的
SHA-256，后续 `apply` 不会再次显示同一个明文 token。

首次建用户不要直接使用 `--deploy`。CLI 会主动阻止这种操作，避免后续门禁失败时丢失
一次性 token。

### 5. 为每台主机安装 Agent

先签发不覆盖已有文件的 enrollment 包：

```bash
./target/release/sing-box-manager enrollment issue \
  --config config/config.toml \
  --server entryA \
  --output /secure/path/entryA.enroll.json
```

将二进制和 enrollment 文件安全分发到目标主机，配置 Agent 环境并启动：

```bash
export AGENT_ENROLLMENT_PATH=/etc/sing-box-manager/agent.enroll.json
export AGENT_STATE_PATH=/var/lib/sing-box-manager/agent.db
export AGENT_CONFIG_DIR=/var/lib/sing-box-manager
export AGENT_BIND_ADDRESS=0.0.0.0:39736
export AGENT_SSM_ADDRESS=127.0.0.1:49736
export SINGBOX_BIN=/usr/local/bin/sing-box

sing-box-manager agent
```

带外核对 enrollment 指纹后，在 Controller 主机授信：

```bash
./target/release/sing-box-manager enrollment trust \
  --server entryA \
  --fingerprint '<issue 命令输出的指纹>'
```

对每台 Entry/Node 重复此流程。防火墙必须把 `39736/tcp` 限制为仅 Controller 可访问。

### 6. 启动 Controller 并部署

```bash
./target/release/sing-box-manager controller --config config/config.toml
```

待 `status --json` 显示全部目标 Agent 已授信、在线且状态新鲜后，在另一终端执行：

```bash
./target/release/sing-box-manager apply \
  --config config/config.toml \
  --deploy
```

部署会先在 Controller 本机完成所有 artifact 的编译和 `sing-box check`，再检查全部目标
Agent 门禁。只有这些步骤全部通过后才开始远端变更。

## 日常变更流程

```text
编辑 config/*.toml
  → plan
  → apply
  → 检查新用户的一次性 token
  → status
  → apply --deploy
  → /readyz 与 /metrics 巡检
```

注意：

- `apply` 只管理清单明确声明的资源，不自动删除清单外对象。
- 撤销 active Route 的授权前必须完成最终流量结算；不满足时 `apply` 会拒绝。
- 用户停用、到期或超额通过 SSM reconcile 生效，不需要为每次运行态变化重启 sing-box。
- Controller 启动会校验清单，但不会隐式执行 `apply`，避免意外创建用户或改变期望状态。

## 订阅

用户订阅地址：

```text
http://127.0.0.1:9736/sub/<一次性保存的 token>
```

格式选择：

- `?target=raw`：base64 编码的 `ss://` URI 列表。
- `?target=clash`：Clash/mihomo YAML。
- 浏览器访问：显示用户状态、线路数量和本周期用量。

订阅 token 等价于代理凭据。公网提供订阅时必须使用 TLS 反向代理，并限制日志、缓存和
Referer 泄露。

## 容器构建

```bash
docker build -t sing-box-manager:0.1.0 .
docker run --rm sing-box-manager:0.1.0 --help
```

镜像只包含 `sing-box-manager`，不包含 sing-box。若在容器内执行 `apply --deploy` 或运行
Agent，需要额外提供 sing-box 二进制、持久化状态目录、配置和证书。生产环境更推荐使用
systemd 管理 Controller 和 Agent，并由 Agent 管理 sing-box 子进程。

## 文档

- [配置目录说明](config/README.md)
- [架构与机制](docs/architecture.md)
- [部署指南](docs/deployment.md)
- [参考手册](docs/reference.md)
- [故障排查](docs/troubleshooting.md)
- [故障恢复](docs/disaster-recovery.md)
- [威胁模型](docs/threat-model.md)
- [开发与测试](docs/development.md)
- [路线图](ROADMAP.md)
- [变更记录](CHANGELOG.md)
- [贡献指南](CONTRIBUTING.md)
- [安全政策](SECURITY.md)

## 安全与隐私

公开仓库只应包含 `config/example.*.toml`。真实配置、`.env`、数据库、证书、私钥、
enrollment 包、生成配置和 SSM 缓存均已加入 `.gitignore`，提交前仍应人工执行脱敏检查。

发现安全问题时不要在公开 Issue 中粘贴密钥、真实服务器地址、订阅 token 或漏洞利用细节，
请按 [安全政策](SECURITY.md) 中的私密渠道报告。

## 许可证

本项目使用 [MIT License](LICENSE)。
