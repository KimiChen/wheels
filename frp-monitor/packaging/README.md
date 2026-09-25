# Packaging

发布与安装模板。发布包由 `../scripts/package.sh` 产出（Linux amd64/arm64 tar.gz +
sha256sums），内容：二进制、`LICENSE`、`THIRD_PARTY_NOTICES.md` 与本目录的安装模板；
不含上游临时树、token、真实服务器地址或本地 `.env`。

| 文件 | 用途 |
|---|---|
| `frp-monitor-server.service` / `frp-monitor-agent.service` | systemd 单元（最小权限；agent 需读 /proc、/sys，勿加 ProcSubset=pid） |
| `frps.toml.example` / `frpc.toml.example` | 示例配置（含 `[monitor]` 段注释说明） |
| `credentials.json.example` | 节点凭据文件骨架（sha256 摘要，不是明文 token） |
| `install.sh` | 安装二进制/配置/单元，不覆盖已有配置；`sudo ./install.sh server\|agent <发布包目录>` |

手动备份历史数据：管理端 `GET /api/admin/v1/backup` 下载 SQLite 快照，
或直接备份 `dataDir`（WAL 模式下建议用管理端备份端点保证一致性）。
