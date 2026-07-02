# ip-certd

白名单驱动的 IP Hostname DNS 与 ACME DNS-01 证书分发服务。

`ip-certd` 用来给一组受控 VPS 自动生成 `https://{ip}.{domain}` 形式的可访问域名，并按需签发、续期和分发 Let's Encrypt 证书。客户端不需要常驻 agent，也不需要 token；它只需要从自己的公网 IP 向统一 API 入口发起请求，服务端会校验来源 IP、更新 Cloudflare DNS、完成 ACME DNS-01 验证，并返回可直接安装到 Nginx 的证书包。

典型结果：

```text
52.0.56.137.example.com -> 52.0.56.137
https://52.0.56.137.example.com
```

## 目录

- [核心特性](#核心特性)
- [适用场景](#适用场景)
- [工作原理](#工作原理)
- [快速开始](#快速开始)
- [配置说明](#配置说明)
- [API](#api)
- [客户端脚本](#客户端脚本)
- [部署建议](#部署建议)
- [安全模型](#安全模型)
- [开发与测试](#开发与测试)
- [项目边界](#项目边界)
- [许可证](#许可证)

## 核心特性

- **IP 白名单控制**：只有写入 `iplist.toml` 的公网 IPv4 才能申请证书。
- **来源 IP 校验**：请求路径中的 IP 必须等于客户端真实来源 IP，默认不依赖共享 token。
- **统一 Hostname 规则**：按 `{ip}.{domain}` 生成域名，例如 `52.0.56.137.example.com`。
- **Cloudflare DNS 自动化**：请求证书时自动 upsert A 记录，并为 ACME DNS-01 创建和清理 TXT 记录。
- **真实 ACME 签发**：基于 Let's Encrypt ACME v2 和 DNS-01 challenge 签发域名证书。
- **请求驱动续期**：本地证书不存在或接近过期时自动签发/续期，默认提前 30 天。
- **证书包下载**：返回包含 `fullchain.pem`、`privkey.pem`、`cert.pem`、`chain.pem`、`metadata.json` 的 `tar.gz`。
- **公网 API 前缀**：推荐通过 `https://<domain>/api` 暴露 API，后端默认监听 `127.0.0.1:9735`。
- **Nginx/宝塔友好**：客户端脚本默认安装到 `/etc/nginx/ssl/{ip}.{domain}/`，并优先兼容宝塔 Nginx 路径。
- **内置基础防护**：支持受信反代真实 IP 解析、每 IP 限流、每 IP 证书操作锁、证书文件权限收紧。

## 适用场景

`ip-certd` 适合这些场景：

- 你维护一批拥有独立公网 IPv4 的 VPS。
- 你希望每台 VPS 都获得一个稳定的 HTTPS 域名。
- 你不想在客户端部署长期运行的 agent。
- 你可以把 DNS 托管在 Cloudflare，并授予受限的 DNS 编辑权限。
- 你接受“客户端主动请求时签发/续期”的模型。

不建议在这些场景使用：

- 多个不可信客户端共享同一个出口 IP。
- 需要支持 Cloudflare 之外的 DNS Provider。
- 需要服务端主动 SSH 登录客户端机器安装证书。
- 需要签发裸 IP 证书，而不是域名证书。

## 工作原理

```text
客户端 VPS
  |
  | POST https://example.com/api/v1/certificates/52.0.56.137/bundle
  v
Nginx / 宝塔反代
  |
  | X-Real-IP: 52.0.56.137
  v
ip-certd 127.0.0.1:9735
  |
  +-- 校验 52.0.56.137 是否在 iplist.toml
  +-- 校验来源 IP 是否等于请求 IP
  +-- 生成 52.0.56.137.example.com
  +-- Cloudflare upsert A 记录
  +-- Let's Encrypt DNS-01 签发或续期
  +-- 本地保存证书与 metadata
  +-- 返回 tar.gz 证书包
```

DNS 记录示例：

```text
A   52.0.56.137.example.com                  -> 52.0.56.137
TXT _acme-challenge.52.0.56.137.example.com  -> <acme-token>
```

证书匹配的是域名 `52.0.56.137.example.com`，不是裸 IP `52.0.56.137`。

## 快速开始

### 1. 构建

```bash
cargo build --release
```

生成的二进制位于：

```text
target/release/ip-certd
```

### 2. 准备配置目录

```bash
sudo mkdir -p /etc/ip-certd /var/lib/ip-certd/certs
sudo cp config/config.example.toml /etc/ip-certd/config.toml
sudo cp config/iplist.example.toml /etc/ip-certd/iplist.toml
```

### 3. 配置 Cloudflare Token

Cloudflare API Token 建议只授予目标 Zone 的最小权限：

- Zone:Read
- DNS:Edit

写入环境变量文件：

```bash
sudo tee /etc/ip-certd/ip-certd.env >/dev/null <<'EOF'
CLOUDFLARE_API_TOKEN=replace-with-cloudflare-dns-edit-token
EOF
sudo chmod 600 /etc/ip-certd/ip-certd.env
```

### 4. 修改主配置

推荐公网入口使用 `https://example.com/api`，后端服务继续只监听本机默认端口 `9735`：

```toml
domain = "example.com"
ttl = 60

[server]
listen = "127.0.0.1:9735"
public_base_url = "https://example.com/api"
storage = "/var/lib/ip-certd"
real_ip_header = "x-real-ip"
trusted_proxies = ["127.0.0.1", "::1"]

[cloudflare]
zone_id = "your-cloudflare-zone-id"
api_token_env = "CLOUDFLARE_API_TOKEN"

[acme]
enabled = true
email = "admin@example.com"
directory = "https://acme-v02.api.letsencrypt.org/directory"
staging_directory = "https://acme-staging-v02.api.letsencrypt.org/directory"
use_staging = false
storage = "/var/lib/ip-certd/certs"
renew_before_days = 30
dns_propagation_timeout_seconds = 120

[security]
allow_private_ip = false
rate_limit_per_ip_per_minute = 6
```

### 5. 添加 IP 白名单

```toml
ips = [
  "52.0.56.137",
]
```

`iplist.toml` 更新后需要重启 `ip-certd` 生效。

### 6. 检查配置并启动

```bash
ip-certd --config /etc/ip-certd/config.toml --iplist /etc/ip-certd/iplist.toml check
ip-certd --config /etc/ip-certd/config.toml --iplist /etc/ip-certd/iplist.toml serve
```

不指定子命令时默认执行 `serve`。

## 配置说明

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `domain` | `ip.example.com` | Hostname 后缀。最终域名为 `{ip}.{domain}`。 |
| `ttl` | `60` | Cloudflare DNS 记录 TTL，允许 `1` 或 `60..86400`。 |
| `server.listen` | `127.0.0.1:9735` | 后端 HTTP 监听地址，只允许 loopback、私网或 link-local。 |
| `server.public_base_url` | 空 | 对外 API 基础地址，建议为 `https://<domain>/api`。 |
| `server.real_ip_header` | `x-real-ip` | 从受信反代读取真实客户端 IP 的请求头。 |
| `server.trusted_proxies` | `127.0.0.1`, `::1` | 只有这些反代来源的真实 IP 请求头会被信任。 |
| `cloudflare.zone_id` | 空 | Cloudflare Zone ID，必填。 |
| `cloudflare.api_token_env` | `CLOUDFLARE_API_TOKEN` | 保存 Cloudflare API Token 的环境变量名。 |
| `acme.enabled` | `true` | 是否允许签发/续期。关闭后只能返回已经存在且未到续期窗口的证书。 |
| `acme.use_staging` | `false` | 是否使用 Let's Encrypt staging 环境。 |
| `acme.storage` | `/var/lib/ip-certd/certs` | 证书、metadata 和 ACME account 凭据目录。 |
| `acme.renew_before_days` | `30` | 距离过期多少天以内触发续期。 |
| `security.allow_private_ip` | `false` | 是否允许内网 IP 出现在白名单和请求路径中。 |
| `security.rate_limit_per_ip_per_minute` | `6` | 每个来源 IP 每分钟请求上限，`0` 表示关闭。 |

## API

推荐公网 API 入口：

```text
https://example.com/api
```

后端路由同时支持带 `/api` 前缀和直接访问形式，方便 Nginx 反代：

```text
GET  /api/health
GET  /health
POST /api/v1/certificates/{ip}/bundle
POST /v1/certificates/{ip}/bundle
```

### 健康检查

```bash
curl -fsS https://example.com/api/health
```

响应：

```json
{"status":"ok"}
```

### 获取证书包

```bash
curl -fsS -X POST \
  -H 'Accept: application/gzip' \
  "https://example.com/api/v1/certificates/52.0.56.137/bundle" \
  -o /tmp/ip-certd-bundle.tar.gz
```

成功响应头：

```http
HTTP/1.1 200 OK
Content-Type: application/gzip
Content-Disposition: attachment; filename="52.0.56.137.tar.gz"
X-Certificate-Hostname: 52.0.56.137.example.com
X-Certificate-IP: 52.0.56.137
X-Certificate-Not-After: 2026-09-30T00:00:00Z
```

证书包内容：

```text
fullchain.pem
privkey.pem
cert.pem
chain.pem
metadata.json
```

错误响应为 JSON：

```json
{"error":"source IP must match requested IP 52.0.56.137"}
```

常见状态码：

| 状态码 | 含义 |
| --- | --- |
| `400` | 请求 IP 格式错误，或请求了不允许的 IP 类型。 |
| `403` | 无法确认真实来源 IP，或来源 IP 与请求 IP 不一致。 |
| `404` | 请求 IP 不在 `iplist.toml` 白名单中。 |
| `429` | 触发来源 IP 限流。 |
| `500` | Cloudflare、ACME 或本地存储失败。 |
| `501` | ACME 已关闭，且本地没有可返回的有效证书。 |

## 客户端脚本

仓库提供 `client/pull-ip-certd-cert.sh`，脚本只接收一个参数：公网 API 入口。

```bash
sudo ./client/pull-ip-certd-cert.sh https://example.com/api
```

脚本会执行：

1. 自动识别当前机器公网 IPv4。
2. 请求 `POST <api-base-url>/v1/certificates/{ip}/bundle`。
3. 解包证书到 `/etc/nginx/ssl/{ip}.{domain}/`。
4. 校验证书、私钥和 SAN。
5. 测试并 reload Nginx。

如果自动识别公网 IPv4 失败，可以用环境变量覆盖，脚本参数仍然只有一个：

```bash
sudo IP_CERTD_IP="52.0.56.137" ./client/pull-ip-certd-cert.sh https://example.com/api
```

可选环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `IP_CERTD_IP` | 自动检测 | 手动指定当前客户端公网 IPv4。 |
| `IP_CERTD_INSTALL_ROOT` | `/etc/nginx/ssl` | 证书安装根目录。 |
| `IP_CERTD_RELOAD_NGINX` | `1` | 是否安装后 reload Nginx，设为 `0` 可跳过。 |

安装后的目录示例：

```text
/etc/nginx/ssl/52.0.56.137.example.com/
  fullchain.pem
  privkey.pem
  cert.pem
  chain.pem
  metadata.json
```

目标服务器 Nginx 示例：

```nginx
server {
    listen 443 ssl;
    server_name 52.0.56.137.example.com;

    ssl_certificate     /etc/nginx/ssl/52.0.56.137.example.com/fullchain.pem;
    ssl_certificate_key /etc/nginx/ssl/52.0.56.137.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
    }
}
```

建议使用 cron 周期执行。续期由客户端请求驱动，如果客户端长期不请求，服务端不会后台主动续期。

```cron
15 3 * * * /usr/local/bin/pull-ip-certd-cert.sh https://example.com/api
```

## 部署建议

### systemd

```ini
[Unit]
Description=ip-certd
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ip-certd/ip-certd.env
ExecStart=/usr/local/bin/ip-certd --config /etc/ip-certd/config.toml --iplist /etc/ip-certd/iplist.toml serve
Restart=always
RestartSec=5
User=ip-certd
Group=ip-certd

[Install]
WantedBy=multi-user.target
```

### Nginx 反代

`ip-certd` 不应直接暴露公网，推荐只把 `/api/` 转发到后端默认端口 `9735`，这样同一个域名的普通网站根路径仍可正常访问。

```nginx
server {
    listen 443 ssl http2;
    server_name example.com;

    ssl_certificate     /etc/nginx/ssl/example.com/fullchain.pem;
    ssl_certificate_key /etc/nginx/ssl/example.com/privkey.pem;

    location /api/ {
        proxy_pass http://127.0.0.1:9735;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    location / {
        root /www/wwwroot/example.com;
        index index.html index.htm;
    }
}
```

宝塔面板中可以先添加站点和反向代理，再在站点 Nginx 配置里确认代理规则只匹配 `/api/`。不要用全站 `location /` 代理到 `ip-certd`，否则会影响 `<domain>` 网站首页。

如果 API 入口域名经过 Cloudflare Proxy、CDN 或其他代理，Nginx 必须先把真实客户端 IP 还原到 `X-Real-IP`，否则 `ip-certd` 会看到代理节点 IP，来源 IP 校验会失败。

## 安全模型

默认安全边界是：

```text
IP 在 iplist.toml 白名单中
  +
客户端真实来源 IP 等于请求路径 IP
```

部署时应遵守：

- 后端 `ip-certd` 只监听 `127.0.0.1` 或内网地址。
- 公网 HTTPS 由 Nginx、宝塔或其他可信反代负责。
- 只信任 `trusted_proxies` 中来源设置的真实 IP 请求头。
- Cloudflare Token 使用最小权限，并通过环境变量传入。
- 不在日志、README、配置示例或 Git 提交中写入真实 token、私钥、证书、服务器 IP 或生产域名。
- `privkey.pem`、`metadata.json`、ACME account 凭据应使用 `0600` 权限。
- 若多个不可信客户端共享同一出口 IP，不应使用当前无 token 模型。

## 证书存储

默认存储结构：

```text
/var/lib/ip-certd/certs/
  accounts/
    production-account.json
  52.0.56.137/
    fullchain.pem
    privkey.pem
    cert.pem
    chain.pem
    metadata.json
```

`metadata.json` 记录：

- `ip`
- `hostname`
- `certificate_path`
- `not_before`
- `not_after`
- `issued_at`
- `renewed_at`
- `last_requested_at`
- `last_source_ip`
- `last_bundle_sha256`

## 技术栈

- Rust 2021
- Tokio async runtime
- Axum HTTP server
- Reqwest + rustls
- instant-acme
- Cloudflare DNS API
- TOML / JSON 配置与元数据
- tar + gzip 证书包

## 开发与测试

格式化：

```bash
cargo fmt
```

运行测试：

```bash
cargo test
```

检查配置：

```bash
cargo run -- --config config/config.example.toml --iplist config/iplist.example.toml check
```

本地启动：

```bash
CLOUDFLARE_API_TOKEN=replace-with-token \
cargo run -- --config config/config.example.toml --iplist config/iplist.example.toml serve
```

调试日志：

```bash
RUST_LOG=debug ip-certd --config /etc/ip-certd/config.toml --iplist /etc/ip-certd/iplist.toml serve
```

## 项目边界

当前项目负责：

- 根据 IP 白名单和真实来源 IP 控制证书请求。
- 生成 `{ip}.{domain}` hostname。
- upsert Cloudflare A 记录。
- 通过 Cloudflare TXT 记录完成 ACME DNS-01。
- 保存和复用本地证书。
- 按请求返回证书包。
- 提供一参数客户端安装脚本。

当前项目不负责：

- 支持 Cloudflare 之外的 DNS Provider。
- 运行权威 DNS Server。
- 为未加入白名单的 IP 签发证书。
- 为来源 IP 不匹配的请求返回证书。
- 主动 SSH 登录客户端安装证书。
- 自动修改客户端业务站点配置。
- 删除 Cloudflare 上不在白名单内的历史 DNS 记录。
- 后台定时续期从未请求过的证书。

## 许可证

MIT

## 参考资料

- Cloudflare DNS Records API: https://developers.cloudflare.com/api/resources/dns/subresources/records/
- Cloudflare DNS Proxy Status: https://developers.cloudflare.com/dns/proxy-status/
- Let's Encrypt Challenge Types: https://letsencrypt.org/docs/challenge-types/
- Let's Encrypt Rate Limits: https://letsencrypt.org/docs/rate-limits/
