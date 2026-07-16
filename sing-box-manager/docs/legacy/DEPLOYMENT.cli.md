# sing-box-manager 部署指南

生产环境推荐使用 **Linux 原生 sing-box + systemd**，入口协议优先选择 Shadowsocks-2022 + SSM。
管理节点（`manage`）运行 manager、计费、订阅和反向代理；入口节点（`entry`）运行客户端入口
sing-box；其余中继、出口和家宽 SOCKS5 统一称为终端节点（`node`）。管理节点与入口节点可以同机，
但属于不同逻辑角色。相较容器，原生部署更容易保护和备份 SQLite、密钥与
[SSM 缓存](https://sing-box.sagernet.org/configuration/service/ssm-api/)。

## 节点分工

```text
订阅用户 ── HTTPS 443 ──> 管理节点 manage
                              Caddy/nginx ──> manager 127.0.0.1:9736
                                                 │
                                                 └─ SSM/gRPC ──> 入口节点 entry

代理客户端 ── 唯一入口端口 19736 ─────────────────────────────> entry sing-box
                                                               ├─> 香港中继终端 :9736
                                                               ├─> 日本中继终端 :9736 ──> 出口终端
                                                               └─> 美国中继终端 :9736 ──> 家宽 SOCKS5
```

| 角色 | 部署内容 | 持久数据 | 说明 |
|---|---|---|---|
| 管理节点 `manage` | manager、Caddy/nginx | 主配置、密钥、SQLite | 唯一计费与订阅控制面 |
| 入口节点 `entry` | 客户端入口 sing-box、SSM/gRPC | `entry.json`、SSM 缓存 | 接收客户端并持有全部 detour 链 |
| 中继终端 `node` | sing-box | 当前终端 JSON | 只接受直接上一跳发来的 `9736` TCP/UDP |
| 出口终端 `node` | sing-box 或外部 SOCKS5 | 终端 JSON或外部凭据 | 连接真实目标地址 |

初期不建议部署两个同时运行的 manager：它们会同时操作 SSM 用户和各自的 SQLite 基线。需要容灾时，
使用“单活管理节点 + 定期备份 + 手动提升备用实例”，不要让两个 manager 主动计量同一入口节点。

## 端口与防火墙

| 位置 | 端口 | 来源 | 建议 |
|---|---|---|---|
| 管理节点 | `443/tcp` | 公网 | 只用于 HTTPS 订阅页和订阅内容 |
| 管理节点 | `80/tcp` | 公网 | 仅在证书签发或 HTTP 跳转需要时开放 |
| 管理节点 | `9736/tcp` | 本机 | manager HTTP，只监听 `127.0.0.1` |
| 入口节点 | `entry_port` | 客户端 | 唯一入口端口；Shadowsocks 模式开放 TCP/UDP |
| 入口节点 | `8081/tcp` | 管理节点 | SSM API；同机时只监听回环地址，分机时只走可信私网 |
| 中继终端 | `9736/tcp`、`9736/udp` | 链路中的直接上一跳 | 不向任意公网来源开放 |
| 家宽出口终端 | 服务端口 | 链路中的直接上一跳 | 按 `network` 开放 TCP、UDP 或两者 |
| 所有服务器 | SSH 端口 | 固定管理地址 | 禁止密码登录并限制来源 |

manager 和中继终端都使用数字端口 `9736`，但 manager 只在管理节点的回环地址监听。若在同一台物理机
合并多个逻辑角色，必须确保监听地址不重叠；不要让终端 sing-box 的 `::`:9736 与 manager 冲突。

`/status` 没有认证，反向代理只应转发 `/sub/*`。以 Caddy 为例：

```caddyfile
sub.example.com {
    handle /sub/* {
        reverse_proxy 127.0.0.1:9736
    }

    handle {
        respond 404
    }
}
```

这里使用 `handle` 保留 `/sub/` 路径；不要直接改成会移除匹配前缀的 `handle_path`。反向代理语法参见
[Caddy 官方文档](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)。

## 文件布局与权限

推荐按逻辑角色使用以下布局：

| 路径 | 权限建议 | 用途 |
|---|---|---|
| `/usr/local/bin/sing-box-manager` | `root:root 0755` | 管理节点：manager 二进制 |
| `/etc/sing-box-manager/config.toml` | `root:<服务组> 0640` | 管理节点：主配置，可能含 SOCKS5 凭据 |
| `/var/lib/sing-box-manager/secrets.toml` | `root:<服务组> 0640` | 管理节点：PSK、UUID、令牌和私钥 |
| `/var/lib/sing-box-manager/state.db` | `<服务用户>:<服务组> 0600` | 管理节点：流量和配额状态 |
| `/var/lib/sing-box-manager/ssm-cache.json` | `<sing-box 用户>:<服务组> 0600` | 入口节点：sing-box SSM 状态 |
| `/etc/sing-box/config.json` | `root:<sing-box 组> 0640` | 入口节点或终端节点：sing-box 配置 |

`state.db` 由管理节点的 manager 写入，`ssm-cache.json` 由入口节点的 sing-box 写入。同机部署两个角色
时，推荐让两个服务使用同一个固定的非 root 服务账号，或加入同一个仅对状态目录有写权限的服务组。
下面的模板默认使用 `DynamicUser`，必须确认密钥文件可被动态用户读取；若不满足，应改用固定
`User`/`Group`，不要用 `chmod 777` 绕过权限问题。

VLESS-Reality reload 模式还需要允许 manager 原子写入 `singbox.config_out` 并执行
`backend.reload_cmd`，因此管理节点与入口节点必须同机或具备额外的远程部署机制，不能直接套用默认
只读沙箱；除非确实需要 Reality，否则推荐先使用权限边界更简单的 SSM 模式。

## systemd 单元

将下面内容保存为 `/etc/systemd/system/sing-box-manager.service`：

```ini
[Unit]
Description=sing-box-manager — sing-box 按用户计量与订阅服务
# 管理节点通过 SSM API 管理入口节点的独立 sing-box。
# 此模板面向默认 SSM 模式；同机 reload 模式需另行授予配置写权限和重载权限。
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# config 只读放 /etc；secrets 与 state.db 放可写的 StateDirectory（/var/lib/sing-box-manager）
# 配置里 db_path 应为 /var/lib/sing-box-manager/state.db
ExecStart=/usr/local/bin/sing-box-manager run \
    /etc/sing-box-manager/config.toml \
    --secrets /var/lib/sing-box-manager/secrets.toml
Restart=on-failure
RestartSec=3
# SIGTERM 触发优雅停机（守护进程内已处理）
KillSignal=SIGTERM
TimeoutStopSec=10

# 状态目录（自动创建，属主为服务用户）
StateDirectory=sing-box-manager
DynamicUser=yes

# 安全加固
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
# 如需绑定 <1024 端口的订阅监听，取消注释：
# AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

安装或修改单元后执行：

```bash
systemctl daemon-reload
systemctl enable --now sing-box-manager
systemctl status sing-box-manager
```

## 部署顺序

1. **准备配置**：空链路的数据面节点命名为 `entry`，其余数据面机器和服务归入终端 `node`；管理节点
   不属于 `[nodes]`。`service.listen` 保持管理节点回环地址；`backend.ssm_base` 同机部署时使用回环，
   分机时使用仅管理节点可访问的可信私网地址；`relay_port` 使用默认 `9736`。
2. **构建并生成配置**：在可信环境中复用同一份 `secrets.toml`，不要在每次发布时删除后重新生成，
   否则会使全部节点和订阅凭据变化。

   ```bash
   cargo build --release
   target/release/sing-box-manager check config.toml
   target/release/sing-box-manager gen-config config.toml \
       --out generated --secrets generated/secrets.toml
   find generated -name '*.json' -type f -exec sing-box check -c {} \;
   ```

3. **部署终端节点**：先确认外部 SOCKS5 出口终端可从直接上一跳连接，再从最远终端向入口方向依次安装
   `generated/nodes/<节点>.json`。每个终端先执行 `sing-box check`，成功后再重启服务。
4. **部署入口节点**：将 `generated/entry.json` 安装为入口节点的 sing-box 配置，检查通过后重启
   sing-box。SSM 同机时只监听回环地址；分机时只允许管理节点通过可信私网访问。
5. **部署管理节点**：安装 manager 二进制、`config.toml`、原有 `secrets.toml` 和 systemd 单元，确认
   状态目录权限后执行 `systemctl enable --now sing-box-manager`。
6. **部署 HTTPS**：配置 DNS 和 Caddy/nginx，只转发 `/sub/*`，不要公开 `/status` 或 SSM API。
7. **验收**：分别检查浏览器页面、命令行原始订阅、客户端导入、各出口公网地址、UDP、用量增长以及
   超额/到期停用行为。

常用检查命令：

```bash
# 管理节点
systemctl is-active sing-box-manager
journalctl -u sing-box-manager --since today
curl -fsS http://127.0.0.1:9736/status
curl -fsS -A curl "https://sub.example.com/sub/<token>"

# 入口节点和普通终端节点
systemctl is-active sing-box
journalctl -u sing-box --since today
```

## 更新、备份与回滚

- 每次变更先备份管理节点的 `config.toml`、`secrets.toml`、`state.db`，以及入口节点的
  `ssm-cache.json` 和当前 sing-box 配置；备份目录应加密并限制读取权限。
- 日常至少每天备份 `state.db`；配置、用户或拓扑变化后立即备份配置和密钥。不要只备份数据库而遗漏
  `secrets.toml`，否则已有订阅无法恢复。
- 拓扑更新先部署新增或变更的终端节点，再替换入口节点配置，并确认 manager 已热加载新配置；只有同时
  修改后端、监听地址、数据库路径或轮询间隔时才需要重启 manager。用户配额、重置周期、有效期和已生成
  用户的出口权限变化不需要重新生成拓扑；增加用户需要重新生成入口配置。
- 增加用户时，先更新配置并用原有 `secrets.toml` 执行 `gen-config`，再部署和检查新的 `entry.json`；入口
  sing-box 重启成功后再交付订阅。不要先发订阅后补入口路由。
- 发布失败时先恢复入口节点 sing-box 和管理节点 manager 配置，再按链路从入口向出口恢复终端节点；
  恢复数据库时应同时恢复相同时间点的 SSM 缓存，避免统计基线出现较大偏差。
- 监控至少覆盖服务存活、入口与终端端口、`/status`、磁盘空间、证书到期、SQLite/SSM 缓存更新时间和
  各 VPS 的月度流量额度。
