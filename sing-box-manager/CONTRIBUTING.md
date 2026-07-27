# 贡献指南

感谢你愿意改进 `sing-box-manager`。项目当前是 `wheels` 仓库中的独立 Rust 子项目，所有命令
默认从 `sing-box-manager` 目录执行。

## 交流语言

面向用户、贡献者的 Issue、提交说明和文档优先使用中文。代码标识、协议名称、环境变量和
上游错误信息可以保留英文。

## 开始之前

- 功能建议或普通缺陷可以先创建公开 Issue，说明场景、期望行为和最小复现。
- 安全问题不要公开披露，按 [安全政策](SECURITY.md) 报告。
- 大型架构变更建议先讨论，避免实现方向与单 Controller、无管理 API 等既定边界冲突。
- 不要在 Issue、日志或补丁中提交真实 IP、域名、SSH 用户、token、证书、私钥或数据库。

## 开发环境

```bash
cd sing-box-manager
cargo build --locked
cargo test --locked
```

详细依赖、源码结构和测试分层见 [开发与测试](docs/development.md)。

## 分支和提交

- 一个分支只解决一类问题。
- 不混入格式化生成物、数据库、真实配置或其他子项目改动。
- 提交信息使用明确中文，例如：
  - `修复：允许首次部署时 sing-box 尚未运行`
  - `文档：补充 Agent systemd 部署说明`
  - `新增：支持订阅 token 轮换命令`
- 保持历史可审查；合并前可按维护者要求整理提交。

## 代码要求

- 保持 UTF-8。
- 运行 `cargo fmt`，不手工制造无关格式变化。
- 所有新行为补充测试，尤其是失败关闭、幂等和秘密脱敏。
- 不引入任意 shell 执行。
- 不绕过 mTLS、证书状态、check、部署门禁或结算屏障。
- 数据库变更使用只向前 migration。
- 新秘密使用现有信封加密和主密钥轮换机制。
- 生产 Controller 不新增管理 HTTP 路由；管理能力通过本机 CLI 提供。

## 文档要求

- 只新增中文用户/贡献者文档。
- 新命令更新 README、参考手册和故障排查。
- 新配置字段更新 `config/README.md` 和公开示例。
- 新安全边界更新威胁模型。
- 新版本更新变更记录和路线图。
- 示例只能使用脱敏域名、占位符和文档保留地址。

## 提交前检查

```bash
cargo fmt --all -- --check
cargo check --locked
cargo test --locked
git diff --check
git status --short
```

如本机具备 sing-box `1.13.14`：

```bash
SINGBOX_BIN=/path/to/sing-box cargo test --locked
```

涉及 mTLS：

```bash
cargo test --locked -- --ignored
```

再检查暂存内容：

```bash
git diff --cached --name-only
git diff --cached
```

## Pull Request 内容

请说明：

- 问题和使用场景。
- 方案及关键取舍。
- 对兼容性、安全和运行状态的影响。
- 测试命令与结果。
- 是否包含 migration、环境变量或运维步骤变化。
- 尚未解决的限制。

维护者可能要求拆分无关变更、补充失败路径测试或重新脱敏。

## 许可证

提交代码即表示你有权贡献该内容，并同意按项目的 [MIT License](LICENSE) 发布。
