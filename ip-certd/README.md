# ip-certd 白名单 DNS 与 SSL 证书管理方案

## 1. 目标

`ip-certd` 是一个受控的 IP 域名解析、Let's Encrypt 证书申请与证书打包下载服务。`example.com` 是部署时使用的示例域名，项目中必须作为配置项存在，不能写死。

示例目标：

```text
https://52.0.56.137.example.com
```

应满足：

- `52.0.56.137.example.com` 解析到 `52.0.56.137`。
- 只有写入 `iplist.toml` 白名单的 IP 才能发起证书请求。
- API 必须校验客户端真实来源 IP，来源 IP 必须等于请求的 IP。
- hostname 由程序根据 `{ip}.{domain}` 统一生成，例如 `52.0.56.137.example.com`。
- 客户端使用 `curl POST` 请求证书包，不使用 agent。
- 不设计 host token、pull token、enrollment，也不提供 token rotation/revoke。
- DNS 托管只支持 Cloudflare，不考虑其他 DNS Provider。
- DNS 记录策略只做 upsert，不自动删除 Cloudflare 上已有但不在白名单内的记录。
- 主服务器负责按请求 upsert DNS A 记录、执行 ACME DNS-01、保存证书、返回证书 `tar.gz` 包。
- 主服务器 API 自身只监听本地或内网 HTTP 端口，公网 HTTPS 由 Nginx 反代提供。

## 2. 总体架构

```text
/etc/ip-certd/config.toml
/etc/ip-certd/iplist.toml
        |
        v
ip-certd 进程
        |
        +-- Cloudflare DNS API
        |     +-- upsert A 记录
        |     +-- upsert/delete ACME TXT 记录
        |
        +-- Let's Encrypt ACME
        |     +-- DNS-01 challenge
        |
        +-- 本地证书存储
        |     +-- /var/lib/ip-certd/certs/{ip}/
        |     +-- metadata.json
        |
        +-- HTTP API
              +-- certificate bundle request

Nginx HTTPS 反代
        |
        v
客户端 VPS
        |
        +-- curl POST /api/v1/certificates/{ip}/bundle
              +-- ip-certd 校验来源 IP 和白名单
              +-- 生成 hostname 并 upsert 当前 IP 的 A 记录
              +-- 没有证书则签发
              +-- 快过期则续期
              +-- 返回 fullchain.pem / privkey.pem 等 tar.gz 包
```

## 3. 安全模型

本方案不使用 token，安全边界是：

```text
IP 在 iplist.toml 白名单中
        +
客户端真实来源 IP 等于请求的 IP
```

请求必须同时满足：

- 请求 IP 存在于 `iplist.toml`。
- 客户端真实来源 IP 等于请求 IP。
- 配置中的 IP 是合法公网 IP，除非明确设置允许内网 IP。

由于服务端会返回 `privkey.pem`，必须遵守：

- `ip-certd` 默认只监听 `127.0.0.1` 或内网地址，不能直接暴露到公网。
- 公网 HTTPS 入口必须由 Nginx 反代提供。
- `ip-certd` 只能信任来自受信反代的 `X-Real-IP` 或 `X-Forwarded-For`。
- 如果 API 域名经过 Cloudflare Proxy、CDN 或其他代理，Nginx 必须先正确还原真实客户端 IP，否则会导致来源 IP 校验失效。
- 本模型适合客户端 VPS 独占公网 IP 的场景；如果多个不可信客户端共享同一个出口 IP，则不应使用无 token 模式。
- 日志不得输出证书私钥、Cloudflare API Token、ACME account key。
- 证书文件、metadata 文件、ACME account key 必须限制文件权限。
- API 建议做基础限流，尤其是证书签发/下载接口。

## 4. DNS 策略

`ip-certd` 根据客户端请求 IP 生成 hostname，并创建或更新真实 DNS 记录，而不是运行动态 DNS 服务。

普通 A 记录：

```text
A 52.0.56.137.example.com -> 52.0.56.137
```

ACME DNS-01 TXT 记录：

```text
TXT _acme-challenge.52.0.56.137.example.com -> <letsencrypt-token>
```

CAA 记录建议：

```text
CAA example.com 0 issue "letsencrypt.org"
```

策略要求：

- 只支持 Cloudflare。
- A 记录必须使用 DNS only。
- 请求证书包时，先根据请求 IP 生成 hostname，再 upsert 当前 IP 的 A 记录。
- 不提供全量同步命令。
- 不自动删除 Cloudflare 上不在白名单中的旧记录。

Cloudflare A 记录示例：

```json
{
  "type": "A",
  "name": "52.0.56.137.example.com",
  "content": "52.0.56.137",
  "ttl": 60,
  "proxied": false
}
```

Cloudflare API 权限建议限制为：

- Zone:Read
- DNS:Edit
- 仅限 `example.com` 所在 zone

## 5. Let's Encrypt 证书策略

本方案申请的是域名证书：

```text
DNS:52.0.56.137.example.com
```

不是裸 IP 证书：

```text
IP Address:52.0.56.137
```

因此浏览器访问：

```text
https://52.0.56.137.example.com
```

证书匹配的是 `52.0.56.137.example.com` 这个域名。

推荐使用 DNS-01 challenge：

- 不要求 `52.0.56.137:80` 对外开放。
- 不要求客户端 VPS 参与 ACME challenge。
- `ip-certd` 通过 Cloudflare API 自动创建和删除 `_acme-challenge` TXT 记录。

注意：

- `52.0.56.137.example.com` 是多级子域名。
- `*.example.com` 通配符证书不能覆盖它。
- 因此建议每个白名单 IP 单独签发一张证书。

## 6. 配置文件

配置拆分为两个文件：

- `config.toml`：服务、Cloudflare、ACME、证书存储、安全策略。
- `iplist.toml`：允许请求证书的 IP 白名单。

### 6.1 config.toml

示例路径：

```text
/etc/ip-certd/config.toml
```

示例配置：

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

### 6.2 iplist.toml

示例路径：

```text
/etc/ip-certd/iplist.toml
```

示例配置：

```toml
ips = [
  "52.0.56.137",
  "1.2.3.4",
]
```

`iplist.toml` 只保存允许使用服务的 IP。程序会按 `{ip}.{domain}` 自动生成 hostname。例如 `ip = "52.0.56.137"` 且 `domain = "example.com"` 时，生成的 hostname 是 `52.0.56.137.example.com`。

`iplist.toml` 变更后通过重启 `ip-certd` 生效。

## 7. API 设计

### 7.1 拉取证书包

客户端执行：

```bash
curl -fsS -X POST \
  "https://example.com/api/v1/certificates/52.0.56.137/bundle" \
  -o /tmp/ip-certd-bundle.tar.gz
```

HTTP API：

```http
POST /api/v1/certificates/52.0.56.137/bundle
Accept: application/gzip
```

成功响应：

```http
HTTP/1.1 200 OK
Content-Type: application/gzip
Content-Disposition: attachment; filename="52.0.56.137.tar.gz"
X-Certificate-Hostname: 52.0.56.137.example.com
X-Certificate-IP: 52.0.56.137
X-Certificate-Not-After: 2026-09-30T00:00:00Z
```

`tar.gz` 包内容：

```text
fullchain.pem
privkey.pem
cert.pem
chain.pem
metadata.json
```

`metadata.json` 示例：

```json
{
  "ip": "52.0.56.137",
  "not_before": "2026-07-02T00:00:00Z",
  "not_after": "2026-09-30T00:00:00Z",
  "renewed_at": "2026-07-02T01:00:00Z",
  "source_ip": "52.0.56.137"
}
```

### 7.2 接口行为

收到请求后，`ip-certd` 按顺序执行：

1. 从受信反代请求头获取真实客户端 IP。
2. 校验请求 IP 格式合法。
3. 校验请求 IP 存在于 `iplist.toml` 的 `ips` 列表。
4. 校验真实客户端 IP 等于请求 IP。
5. 根据 `{ip}.{domain}` 生成 hostname。
6. Upsert 当前 IP 对应 hostname 的 Cloudflare A 记录。
7. 如果本地没有证书，执行 ACME DNS-01 签发。
8. 如果证书将在 `renew_before_days` 内过期，执行续期。
9. 保存或更新本地证书文件与 `metadata.json`。
10. 返回证书 `tar.gz` 包。

推荐错误码：

```text
400 Bad Request       请求 IP 格式错误
403 Forbidden         来源 IP 与请求 IP 不匹配
404 Not Found         IP 不在 iplist.toml
429 Too Many Requests 请求过于频繁
500 Internal Error    DNS、ACME 或存储失败
```

## 8. 客户端使用方式

客户端不需要安装 agent，只需要用 `curl` 拉取证书包并安装。

也可以直接使用仓库提供的客户端脚本。脚本只接收一个参数：公网 API 入口，例如 `https://example.com/api`。脚本会自动识别当前机器公网 IPv4，请求证书包，解包到 `/etc/nginx/ssl/{ip}.{domain}/`，并在本机存在 Nginx 时执行配置测试和 reload。

```bash
sudo ./client/pull-ip-certd-cert.sh https://example.com/api
```

如果自动识别公网 IPv4 失败，可以用环境变量覆盖；这不改变脚本的参数数量：

```bash
sudo IP_CERTD_IP="52.0.56.137" ./client/pull-ip-certd-cert.sh https://example.com/api
```

示例：

```bash
IP="52.0.56.137"
DOMAIN="example.com"
HOST="$IP.$DOMAIN"
INSTALL_DIR="/etc/nginx/ssl/$HOST"

mkdir -p "$INSTALL_DIR"

curl -fsS -X POST \
  "https://example.com/api/v1/certificates/$IP/bundle" \
  -o /tmp/ip-certd-bundle.tar.gz

tar -xzf /tmp/ip-certd-bundle.tar.gz -C "$INSTALL_DIR"
chmod 600 "$INSTALL_DIR/privkey.pem"

nginx -t && systemctl reload nginx
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

客户端可以用 cron 周期性请求。由于签发和续期由客户端请求驱动，如果客户端长期不请求，证书不会被后台自动续期。

```cron
15 3 * * * /usr/local/bin/pull-ip-certd-cert.sh
```

## 9. 主服务器部署

`ip-certd` 只作为 server 运行：

```bash
ip-certd \
  --config /etc/ip-certd/config.toml \
  --iplist /etc/ip-certd/iplist.toml
```

建议使用 systemd 托管：

```ini
[Unit]
Description=ip-certd
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ip-certd/ip-certd.env
ExecStart=/usr/local/bin/ip-certd --config /etc/ip-certd/config.toml --iplist /etc/ip-certd/iplist.toml
Restart=always
RestartSec=5
User=ip-certd
Group=ip-certd

[Install]
WantedBy=multi-user.target
```

Nginx 反代示例：

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
}
```

如果 API 入口所在域名放在 Cloudflare DNS 中，建议该记录使用 DNS only，避免 Nginx 收到的是 Cloudflare 节点 IP。若必须开启 Cloudflare Proxy，则需要在 Nginx 中配置 Cloudflare IP 段的 real IP 还原。

## 10. 证书存储结构

默认保存为：

```text
/var/lib/ip-certd/certs/
  52.0.56.137/
    fullchain.pem
    privkey.pem
    cert.pem
    chain.pem
    metadata.json
```

`metadata.json` 记录：

- ip
- certificate path，以 IP 目录保存
- not_before
- not_after
- issued_at
- renewed_at
- last_requested_at
- last_source_ip
- last_bundle_sha256

证书写入要求：

- 私钥文件权限限制为 `0600`。
- 证书目录权限限制为 `0700` 或至少避免普通用户读取私钥。
- 签发和续期写入临时目录，校验证书和私钥匹配后再原子替换。
- 同一 IP 的签发、续期、打包必须加锁，避免并发请求重复签发。
- 运行状态以证书目录中的 `metadata.json` 为准。

## 11. Rust 项目模块设计

```text
src/
  main.rs
  config.rs
  iplist.rs
  whitelist.rs
  real_ip.rs
  cert_store.rs
  bundle.rs

  cloudflare.rs

  acme/
    mod.rs
    manager.rs
    dns01.rs
    account.rs

  api/
    mod.rs
    certificates.rs
    errors.rs
```

核心职责：

- `config.rs`：读取 `config.toml` 与环境变量。
- `iplist.rs`：读取 `iplist.toml`。
- `whitelist.rs`：校验 IP 是否在白名单中，并根据 `{ip}.{domain}` 生成 hostname。
- `real_ip.rs`：从受信反代请求头解析真实客户端 IP。
- `cloudflare.rs`：调用 Cloudflare DNS API。
- `acme/manager.rs`：申请和续期 Let's Encrypt 证书。
- `acme/dns01.rs`：创建、等待、清理 DNS-01 TXT 记录。
- `cert_store.rs`：保存证书、私钥和 metadata。
- `bundle.rs`：生成证书 `tar.gz` 包。
- `api/certificates.rs`：处理证书包请求。

Cloudflare DNS 方法：

```rust
pub struct CloudflareDns {
    // zone_id, api token, http client
}

impl CloudflareDns {
    pub async fn upsert_a(&self, name: &str, ip: &str, ttl: u32) -> anyhow::Result<()>;
    pub async fn upsert_txt(&self, name: &str, value: &str, ttl: u32) -> anyhow::Result<()>;
    pub async fn delete_txt(&self, name: &str, value: &str) -> anyhow::Result<()>;
    pub async fn upsert_caa(&self, name: &str, value: &str, ttl: u32) -> anyhow::Result<()>;
}
```

## 12. MVP 开发顺序

1. 实现 `config.toml` 与 `iplist.toml` 读取。
2. 实现白名单校验和真实来源 IP 校验。
3. 实现 HTTP server 和 `POST /api/v1/certificates/{ip}/bundle`。
4. 实现 Cloudflare A 记录 upsert。
5. 实现 Cloudflare TXT 记录 upsert/delete。
6. 实现 ACME DNS-01 单 IP 生成域名签发。
7. 实现证书文件存储和 `metadata.json`。
8. 实现 `tar.gz` 证书包返回。
9. 实现 `renew_before_days` 请求驱动续期。
10. 增加 per-IP 文件锁、限流、日志脱敏和错误码整理。
11. 增加 Nginx 反代与客户端 curl 部署文档。

## 13. 最终边界

这个系统负责：

- 根据白名单 IP 和来源 IP 控制证书请求。
- 请求时生成 hostname 并 upsert 当前 IP 的 Cloudflare A 记录。
- 根据请求申请或续期 Let's Encrypt 域名证书。
- 自动完成 DNS-01 TXT 验证。
- 本地保存证书、私钥和续期状态。
- 返回包含证书和私钥的 `tar.gz` 包。

这个系统不负责：

- 支持 Cloudflare 之外的 DNS Provider。
- 运行权威 DNS Server。
- 自动 SSH 登录目标服务器。
- 提供独立 agent。
- 自动修改目标服务器业务配置。
- 自动删除 Cloudflare 上不在白名单中的 DNS 记录。
- 后台定时签发或续期没有请求过的证书。
- 为没有写入 `iplist.toml` 的 IP 签发证书。
- 为来源 IP 不匹配的客户端提供证书下载。

## 14. 参考文档

- Cloudflare DNS Records API: https://developers.cloudflare.com/api/resources/dns/subresources/records/
- Cloudflare DNS Proxy Status: https://developers.cloudflare.com/dns/proxy-status/
- Let's Encrypt Challenge Types: https://letsencrypt.org/docs/challenge-types/
- Let's Encrypt Rate Limits: https://letsencrypt.org/docs/rate-limits/
