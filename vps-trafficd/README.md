# vps-trafficd

`vps-trafficd` 是一个轻量的 VPS 流量统计服务。它运行在 Linux VPS 上，
通过读取 `/sys/class/net/<iface>/statistics/{rx_bytes,tx_bytes}` 中的网卡字节计数器，
按服务商的流量充值周期计算当前周期已用流量、剩余流量，并提供带 Bearer Token 鉴权的 JSON API 和内置网页。

适合这些场景：

- 你的 VPS 服务商按月或多月周期重置流量额度。
- 你想用一个很小的常驻进程查看本机剩余流量。
- 你需要给自建面板、脚本或多 VPS 汇总工具提供统一 JSON 数据。
- 你希望手动录入服务商面板的当前已用流量，校准本周期剩余额度。

## 特性

- **无数据库依赖**：配置保存在 TOML 文件，运行状态保存在本机 JSON 文件。
- **低侵入统计**：只读取 Linux sysfs 网卡计数器，不抓包，不改 iptables/nftables。
- **周期化流量计算**：按 `cycle_anchor` 和 `cycle_months` 推算当前流量充值周期；遇到短月份会自动使用月末同一时间。
- **后台主动采样**：服务运行时默认每 `3600` 秒刷新一次状态，并在流量周期边界主动唤醒重置基准。
- **多网卡聚合**：可同时统计 `eth0`、`ens3` 等多个网卡。
- **多种计费口径**：支持 `total`、`rx`、`tx`、`max`。
  - `total`：接收 + 发送
  - `rx`：只算接收
  - `tx`：只算发送
  - `max`：取接收/发送中的较大值
- **鉴权 API**：流量和配置接口必须携带 `Authorization: Bearer <token>`。
- **可选内置 HTTPS**：把已有证书放到 `/etc/vps-trafficd/tls/fullchain.pem`，私钥放到 `/etc/vps-trafficd/tls/privkey.pem`，服务重启后自动启用 HTTPS。
- **内置网页**：浏览器打开 `/` 后输入 token，即可查看流量、更新周期/额度/计费口径，并录入本周期已用流量做校准。
- **配置在线更新**：`PUT /api/v1/config` 会写回 `config.toml`；`current_cycle_used_bytes` 只用于校准状态，不写入配置。
- **原始方向流量独立展示**：API 返回的 `rx_bytes` / `tx_bytes` 始终是网卡原始周期增量；校准只影响 `used_bytes` 和 `remaining_bytes`。
- **静态二进制友好**：推荐构建 `x86_64-unknown-linux-musl`，避免旧发行版 glibc/OpenSSL 兼容问题。
- **systemd 部署**：提供 unit 文件，包含基础硬化选项和明确的可写路径。

## 快速开始

### 构建

```bash
rustup target add x86_64-unknown-linux-musl
cargo build --release --target x86_64-unknown-linux-musl
```

二进制输出位置：

```text
target/x86_64-unknown-linux-musl/release/vps-trafficd
```

启用内置 HTTPS 后，TLS 依赖使用 Rustls/ring。交叉构建 musl 目标时，本机还需要可用的 C 交叉编译器，
例如 `x86_64-linux-musl-gcc` 或 Zig；也可以直接在目标 Linux 机器上执行 `cargo build --release` 做原生构建。

### 安装

```bash
sudo install -d -m 0755 /etc/vps-trafficd /etc/vps-trafficd/tls /var/lib/vps-trafficd
sudo install -m 0755 target/x86_64-unknown-linux-musl/release/vps-trafficd /usr/local/bin/vps-trafficd
sudo install -m 0600 config/config.example.toml /etc/vps-trafficd/config.toml
sudo editor /etc/vps-trafficd/config.toml
```

启动前至少修改：

- `auth_token`
- `interfaces`
- `quota_bytes`
- `cycle_anchor`

如果 `auth_token` 为空或仍是示例值，服务会拒绝启动。

如果要直接让服务提供 HTTPS，把已有 PEM 证书和私钥放到默认位置：

```bash
sudo install -m 0644 fullchain.pem /etc/vps-trafficd/tls/fullchain.pem
sudo install -m 0600 privkey.pem /etc/vps-trafficd/tls/privkey.pem
sudo systemctl restart vps-trafficd
```

证书和私钥必须同时存在。两个文件都不存在时，服务会继续以 HTTP 方式运行；只存在其中一个时，`check` 和启动都会失败。

### 检查并运行

```bash
vps-trafficd check --config /etc/vps-trafficd/config.toml
vps-trafficd --config /etc/vps-trafficd/config.toml
```

安装 systemd 服务：

```bash
sudo install -m 0644 packaging/vps-trafficd.service /etc/systemd/system/vps-trafficd.service
sudo systemctl daemon-reload
sudo systemctl enable --now vps-trafficd
sudo systemctl status vps-trafficd
```

## 配置

示例配置见 [config/config.example.toml](config/config.example.toml)。

```toml
listen_addr = "0.0.0.0:9733"
tls_cert_path = "/etc/vps-trafficd/tls/fullchain.pem"
tls_key_path = "/etc/vps-trafficd/tls/privkey.pem"
auth_token = "replace-with-a-long-random-token"
interfaces = ["eth0"]
node_id = "vps-trafficd-01"
quota_bytes = 1099511627776
billing_mode = "total"
cycle_anchor = "2026-01-31T08:00:00+08:00"
cycle_months = 1
state_path = "/var/lib/vps-trafficd/state.json"
```

| 字段 | 说明 |
| --- | --- |
| `listen_addr` | HTTP/HTTPS 监听地址。默认 `0.0.0.0:9733`，协议由 TLS 证书是否存在决定。 |
| `tls_cert_path` | PEM 证书路径。默认 `/etc/vps-trafficd/tls/fullchain.pem`。 |
| `tls_key_path` | PEM 私钥路径。默认 `/etc/vps-trafficd/tls/privkey.pem`。 |
| `auth_token` | API 和网页使用的 Bearer Token。必须替换示例值。 |
| `interfaces` | 要统计的网卡名列表，多网卡会聚合 rx/tx。 |
| `node_id` | 节点标识，用于多 VPS 汇总时区分来源。 |
| `quota_bytes` | 本流量周期额度，单位为字节。 |
| `billing_mode` | 计费口径：`total`、`rx`、`tx`、`max`。 |
| `cycle_anchor` | 流量充值周期锚点，使用 RFC 3339 时间格式并携带时区。 |
| `cycle_months` | 流量充值周期月数，`1` 表示每月重置，`3` 表示每三个月重置。 |
| `state_path` | 本机状态文件路径。状态文件不保存 `auth_token`。 |

## 网页

访问服务根路径：

```text
http://<server>:9733/
https://<server>:9733/
```

未放置 TLS 证书时使用 HTTP；证书和私钥同时存在时使用 HTTPS。页面会弹框要求输入 Bearer Token。登录后可以：

- 查看节点、周期、已用、剩余、RX、TX、计费口径和额度。
- 修改流量充值开始时间、周期月数、额度和计费口径。
- 输入服务商面板上的“本周期已使用流量”，用于校准当前不完整周期。

页面中的 `Traffic quota` 和 `Current cycle used` 表单默认以 `G` 为单位回填，提交时会转换为字节。

## API

除 `/health` 外，接口都需要：

```http
Authorization: Bearer <token>
```

### 健康检查

```bash
curl http://127.0.0.1:9733/health
```

返回：

```json
{"status":"ok"}
```

### 查询流量

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:9733/api/v1/traffic
```

启用 TLS 后，把 URL 中的 `http://` 换成 `https://`。

返回字段示例：

```json
{
  "node_id": "vps-trafficd-01",
  "cycle_start": "2026-07-01T08:00:00+08:00",
  "cycle_end": "2026-08-01T08:00:00+08:00",
  "quota_bytes": 1099511627776,
  "billing_mode": "total",
  "rx_bytes": 123456789,
  "tx_bytes": 987654321,
  "used_bytes": 1111111110,
  "remaining_bytes": 1098400516666,
  "updated_at": "2026-07-02T13:00:00+08:00"
}
```

`rx_bytes` 和 `tx_bytes` 表示本机网卡在当前周期内的原始方向累计值。
`used_bytes` 表示按 `billing_mode` 和校准偏移计算后的账单口径用量，因此手动校准后它不一定等于原始 `rx_bytes` / `tx_bytes` 的简单组合。

### 读取配置

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:9733/api/v1/config
```

返回字段不包含 `auth_token`。

### 更新配置并校准已用流量

```bash
curl -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "traffic_cycle_anchor": "2026-07-01T08:00:00+08:00",
    "traffic_cycle_months": 1,
    "quota_bytes": 1099511627776,
    "billing_mode": "max",
    "current_cycle_used_bytes": 536870912000
  }' \
  http://127.0.0.1:9733/api/v1/config
```

`current_cycle_used_bytes` 是可选字段。传入时，服务会根据当前 `billing_mode`
更新状态文件中的校准偏移，让 API 里的 `used_bytes` 与输入值对齐；`rx_bytes` / `tx_bytes`
仍然返回网卡原始周期增量。

## 命令行

```bash
vps-trafficd --config /etc/vps-trafficd/config.toml
vps-trafficd check --config /etc/vps-trafficd/config.toml
vps-trafficd calibrate --config /etc/vps-trafficd/config.toml --rx 1234 --tx 5678
```

- 默认子命令为空时启动 HTTP 服务。
- `check` 会检查配置、token、网卡计数器和状态目录写入权限。
- `calibrate` 用于手动设置当前周期 rx/tx 校准偏移，影响账单口径的 `used_bytes`，不改变 API 返回的原始 `rx_bytes` / `tx_bytes`。

## 状态存储

`vps-trafficd` 使用本地 JSON 状态文件保存运行状态，默认路径是：

```text
/var/lib/vps-trafficd/state.json
```

状态文件包含：

- 当前周期边界。
- 每个网卡上次读取到的 rx/tx 计数器。
- 当前周期累计 rx/tx。
- 校准偏移。
- 更新时间和状态格式版本。

服务启动、查询 `/api/v1/traffic`、更新配置或执行校准时都会刷新状态。服务常驻运行时还会默认每 `3600`
秒后台采样一次；如果下一次流量周期边界早于采样间隔，会在周期边界主动唤醒并重置本周期基准。

服务启动时如果状态文件不存在，会以当前网卡计数器作为本周期基准创建状态。
如果进入新周期，会重置周期累计并以当前计数器作为新周期基准。
如果网卡计数器变小，服务会视为系统重启、网卡重置或计数器回绕，不产生负增量。
如果服务进程停止并跨过周期边界，网卡计数器无法回溯切分停机期间的流量；重启后可通过网页或 `calibrate`
命令用服务商面板数据校准本周期已用量。

状态写入使用临时文件和原子 rename，降低崩溃时损坏状态文件的风险。

## 安全说明

- `auth_token` 只保存在 `config.toml`，不会写入状态文件，也不会通过配置 API 返回。
- 公开接口只有 `/health`，且只返回最小健康信息。
- 服务可直接使用本地 PEM 证书提供 HTTPS；如果需要自动签发/续期、多域名或 80/443 标准入口，仍可放在 Nginx、Caddy 等反向代理后面。
- 私钥文件建议使用 `0600` 权限，仅允许运行服务的用户读取。
- 默认 systemd unit 使用 `NoNewPrivileges=true`、`PrivateTmp=true`、`ProtectSystem=full`，
  并只开放 `/etc/vps-trafficd` 和 `/var/lib/vps-trafficd` 写权限。

## 兼容性

- 目标系统：CentOS 7+、Debian 11+、Ubuntu 20.04+ 或其他提供 Linux sysfs 网卡统计的发行版。
- 推荐架构：`x86_64-unknown-linux-musl`。
- systemd 不是程序运行的硬依赖，但推荐用于生产部署和自动重启。

## 开发

```bash
cargo fmt --all
cargo test
cargo run -- --help
```

本项目使用：

- Rust 2021
- Axum + Tokio 提供 HTTP 服务
- Axum Server + Rustls 提供可选 HTTPS
- Clap 提供 CLI
- Serde/TOML/JSON 处理配置和状态
- Chrono 处理时区和周期边界

## License

MIT
