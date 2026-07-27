# 开发与测试

本文面向希望阅读、修改或发布 `sing-box-manager` 的开发者。

## 开发环境

- Rust `1.88` 或更高版本。
- Cargo 使用仓库内 `Cargo.lock`。
- 可选 sing-box `1.13.14`，用于真实配置 check 测试。
- Linux 或 macOS。

初始化：

```bash
cd sing-box-manager
rustc --version
cargo --version
cargo build --locked
```

如果 sing-box 不在 `PATH`：

```bash
export SINGBOX_BIN=/absolute/path/to/sing-box
```

## 源码布局

```text
src/
  main.rs          CLI 与 Controller/Agent 入口
  manifest/        TOML 清单加载、校验和幂等 apply
  compiler/        拓扑快照到 sing-box JSON 的纯编译器
  domain/          领域类型和状态枚举
  store/           Controller SQLite 存储与迁移编排
  manager/         轮询、门禁、部署、reconcile、计量、观测
  agent/           mTLS 服务、本机部署、SSM、结算 outbox
  pki/             CA、证书、enrollment 和身份验证
  subscription/    raw/Clash/HTML 订阅

migrations/        Controller 与 Agent SQL 迁移
config/            公开脱敏配置示例
docs/              中文设计、部署、运维和安全文档
```

`_legacy/` 是早期实现参考，不参与当前 crate 编译，也从 crate 发布包排除。新功能不要继续添加到该目录。

## 架构约束

修改时必须保持：

- CLI 是唯一管理入口；生产 Controller 不挂载管理 API。
- Agent 不主动连接 Controller。
- Agent 不接受任意 shell、命令名或参数。
- Entry/Node/Agent/SSM 固定端口分别为 `19736/29736/39736/49736`。
- 所有秘密在数据库中必须信封加密，不能进入 Debug、日志、审计和命令队列。
- 部署必须先 check，Node 先于 Entry，失败可回滚。
- Entry 重启前必须完成最终流量结算。
- 配置生成必须确定性，避免无意义 revision。
- VLESS 未完整实现前必须失败关闭，不能部分发布。
- 一个 Controller SQLite 只允许一个后台 Controller。

完整机制见 [架构与实现机制](architecture.md)。

## 常用检查

```bash
cargo fmt --all -- --check
cargo check --locked
cargo test --locked
```

真实 mTLS loopback 测试默认 ignored：

```bash
cargo test --locked -- --ignored
```

该测试需要本机 loopback TLS 环境稳定。默认测试集中有两类 sing-box check 测试：如果找不到
`SINGBOX_BIN`，测试会打印 skip 并返回成功；发布前应在装有目标 sing-box 的环境再次运行。

## 测试分层

- 纯函数测试：拓扑校验、配置规范化、路由链、门禁状态、配额周期。
- SQLite 测试：迁移、外键、密钥信封、用户授权、计量幂等、部署状态。
- 文件系统测试：原子替换、revision 快照、回滚、权限。
- Mock Agent 测试：超时重试、幂等命令、部署批次、reconcile。
- 真实 sing-box check：用临时 `0700` 目录和 `0600` 配置验证目标 JSON。
- mTLS 测试：证书角色、SAN、SPKI pin、过期和错误 CA。

新增行为必须至少覆盖正常路径、失败路径和幂等重试。

## 本地清单回归

公开示例：

```bash
tmpdir="$(mktemp -d)"
for name in config servers protocols listeners relays users; do
  cp "config/example.${name}.toml" "$tmpdir/${name}.toml"
done
target/debug/sing-box-manager plan --config "$tmpdir/config.toml"
```

不要在测试输出中打印真实本地配置。真实 `config/*.toml` 默认 ignored，但仍需在提交前检查。

## 数据库迁移

Controller 和 Agent 使用独立迁移序列：

```text
migrations/0001_*.sql ...       Controller
migrations/agent_0001.sql ...   Agent
```

新增迁移步骤：

1. 创建下一个连续版本 SQL 文件。
2. 在 `src/store/migrations.rs` 或 `src/agent/state.rs` 中用 `include_str!` 注册。
3. 迁移只向前，不修改已经发布的旧 SQL。
4. 增加迁移幂等、外键和新列测试。
5. 更新故障恢复和升级文档。

Docker 构建必须复制 `migrations/`，否则 `include_str!` 在构建阶段失败。

## 密钥相关修改

任何新增秘密字段都需要回答：

- 明文从哪里产生？
- 内存中存活多久？
- 存储时使用哪个 credential kind/scope？
- 是否进入 Debug、serde 响应、错误、日志或审计？
- 主密钥轮换扫描是否覆盖？
- 删除对象时是否清理 credential 和版本？
- 测试是否验证密文不可直接看到明文？

禁止为了调试临时打印 PSK、UUID、私钥、token 或编译后的完整配置。

## CLI 变更

新增 CLI 时：

- 命令和帮助文案使用中文。
- 默认只做最小必要副作用。
- 会覆盖文件时使用 `create_new` 或明确确认机制。
- 一次性秘密只输出一次，并在文档中标记日志风险。
- `--json` 输出保持机器可读且字段稳定。
- 更新 README、参考手册、故障排查和变更记录。

## 文档要求

- 面向用户和贡献者的文档只使用中文。
- 命令、变量名、协议名和代码标识保留原文。
- 所有示例必须使用 `example.com`、文档保留地址或明显占位符。
- 不把尚未实现的路线图描述成当前能力。
- 文档链接使用相对路径并在提交前检查。

## 提交前脱敏

```bash
git status --short --ignored config
git diff --cached --check
git diff --cached --name-only
git diff --cached | rg \
  'BEGIN .*PRIVATE KEY|ENCRYPTION_MASTER_KEY=|ssh://[^@ ]+@|([0-9]{1,3}\\.){3}[0-9]{1,3}'
```

匹配结果需要人工判断。回环地址、`0.0.0.0` 和文档保留网段可以保留；真实基础设施信息不能提交。

## 发布检查

首发或后续版本：

1. 更新 `Cargo.toml` 版本。
2. 更新 `CHANGELOG.md`。
3. 确认 README 能从空目录流程运行。
4. 运行格式、check、完整测试和 ignored mTLS 测试。
5. 使用目标 sing-box 版本跑真实 check。
6. 用公开 `example.*.toml` 运行 plan 和两次 apply 幂等检查。
7. 构建 release 二进制和 Docker 镜像。
8. 扫描秘密、真实地址和未跟踪生成物。
9. 从远端基线检查最终提交范围。
10. 创建签名 tag 和发布说明；推送前再次人工复核。
