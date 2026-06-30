# Resend Ticketd 工单服务方案

## 目标

`resend-ticketd` 是一个基于 Resend 发信和 Inbound Email 的最简化工单系统，交付为 Linux Rust 常驻服务。第一版面向单机部署，支持 CentOS 7+、Debian 11+、Ubuntu 20.04+，默认监听公网地址 `0.0.0.0:9734`，也可配置为 `0.0.0.0:443`。

第一版提供：

- Resend Inbound Webhook 收信建单。
- Resend Send Email API 回复客户。
- SQLite 本地持久化。
- 单管理员 Web 后台，支持登录、列表、详情、回复、关闭。
- 内置 HTTPS，证书通过配置调用 `lego` CLI 使用 DNS-01 申请和续期。
- systemd 常驻运行和安装防护。

不包含多人坐席、工单分配、内部备注、全文搜索、附件本地归档。

## Resend 对接

Resend 侧手动完成收信域名和 webhook 配置：

1. 在 Resend 配置 Receiving Domain 和 MX 记录。
2. 添加 webhook endpoint：`https://<domain>:<port>/webhooks/resend`。
3. webhook event 选择 `email.received`。
4. 将 webhook signing secret 写入服务配置。

服务处理流程：

1. `POST /webhooks/resend` 接收 webhook。
2. 使用 raw body 和 Resend/Svix 签名头校验请求，签名失败直接拒绝。
3. 对 `email.received` 事件做幂等去重。
4. 调用 Resend Receiving API 拉取完整邮件正文、头信息和附件元数据。
5. 根据主题编号和邮件头匹配工单，匹配失败则新建工单。
6. 管理员在后台回复时调用 Resend Send Email API，并写入本地消息记录。

工单匹配规则：

- 优先匹配主题中的 `[TKT-000001]`。
- 再匹配 `Message-ID`、`References`、`In-Reply-To`。
- 已关闭工单收到客户回复时自动重开。
- 新工单回复主题统一为 `Re: [TKT-编号] 原主题`。

附件策略：

- 第一版只保存附件元数据，如文件名、大小、MIME 类型、Resend attachment id、临时下载链接过期时间。
- 不下载附件到本地，避免磁盘占用、恶意文件扫描和清理策略复杂化。

## 服务接口

公开接口：

- `GET /healthz`：健康检查。
- `POST /webhooks/resend`：Resend inbound webhook。

后台接口：

- `GET /login`、`POST /login`。
- `POST /logout`。
- `GET /tickets?status=&page=`。
- `GET /tickets/:id`。
- `POST /tickets/:id/reply`。
- `POST /tickets/:id/close`。

后台安全要求：

- 单管理员账号。
- 密码只保存 Argon2 哈希。
- 登录成功后写入 SQLite session。
- Cookie 必须启用 `HttpOnly`、`Secure`、`SameSite=Lax` 或更严格。
- 表单提交必须带 CSRF token。
- 登录接口需要限速。
- 邮件 HTML 不直接渲染，后台默认展示转义后的文本或安全转换后的正文。

## 配置

子项目根目录提供 `.env.example`，安装后配置文件放在 `/etc/resend-ticketd/.env`。

示例配置：

```dotenv
RESEND_TICKETD_LISTEN_ADDR=0.0.0.0:9734
RESEND_TICKETD_PUBLIC_BASE_URL=https://tickets.example.com:9734

DATABASE_URL=sqlite:///var/lib/resend-ticketd/resend-ticketd.db

RESEND_API_KEY=re_xxxxxxxxx
RESEND_WEBHOOK_SECRET=whsec_xxxxxxxxx
RESEND_FROM="Support <support@example.com>"
SUPPORT_ADDRESSES=support@example.com

TLS_CERT_PATH=/etc/resend-ticketd/tls/fullchain.pem
TLS_KEY_PATH=/etc/resend-ticketd/tls/privkey.pem

ADMIN_USERNAME=admin
ADMIN_PASSWORD_HASH=$argon2id$v=19$...

ACME_EMAIL=admin@example.com
ACME_DOMAIN=tickets.example.com
ACME_LEGO_PATH=/usr/local/bin/lego
ACME_DNS_PROVIDER=cloudflare
ACME_DNS_ENV_FILE=/etc/resend-ticketd/acme.env
ACME_CERT_DIR=/etc/resend-ticketd/tls
```

配置校验必须拒绝：

- 空 Resend API key 或 webhook secret。
- 示例值、占位值、弱管理员密码哈希。
- TLS 证书或私钥不存在。
- 数据库目录不可写。
- 监听 `443` 但 systemd 未授予低端口能力。

## 数据模型

SQLite 表：

- `tickets`：工单编号、客户邮箱、主题、状态、创建时间、更新时间、关闭时间。
- `messages`：工单消息、方向、Resend email id、message id、正文、头信息摘要、创建时间。
- `attachments`：消息附件元数据，不保存文件正文。
- `webhook_events`：Resend webhook event id、事件类型、处理状态、创建时间。
- `admin_sessions`：后台会话 id 哈希、过期时间、创建时间。
- `schema_migrations`：数据库迁移版本。

状态建议：

- `open`：待处理或已重开。
- `closed`：已关闭。

## 证书申请和续期

服务提供证书命令：

```bash
resend-ticketd cert issue --config /etc/resend-ticketd/.env
resend-ticketd cert renew --config /etc/resend-ticketd/.env
```

证书命令读取配置后调用 `lego` CLI，默认使用 DNS-01：

- DNS 服务商凭据写入 `ACME_DNS_ENV_FILE`。
- `ACME_DNS_ENV_FILE` 权限必须是 `0600`。
- 证书输出到 `ACME_CERT_DIR`。
- 续期成功后执行 `systemctl reload-or-restart resend-ticketd`。

第一版不内置 ACME 客户端逻辑，避免把 DNS provider 差异、续期失败恢复和账号状态管理放进主服务。

## 安装防护

发布包建议包含：

- `resend-ticketd` 静态链接二进制。
- `resend-ticketd.service`。
- `resend-ticketd-cert-renew.service`。
- `resend-ticketd-cert-renew.timer`。
- `install.sh`。
- `.env.example`。

安装脚本必须执行：

1. 创建系统用户 `resend-ticketd`，禁止登录 shell。
2. 创建 `/etc/resend-ticketd`、`/var/lib/resend-ticketd`、`/var/log/resend-ticketd`。
3. 配置目录权限：
   - `/etc/resend-ticketd`：`0750 root:resend-ticketd`
   - `/etc/resend-ticketd/.env`：`0640 root:resend-ticketd`
   - TLS 私钥和 DNS 凭据：`0600 root:root`
   - `/var/lib/resend-ticketd`：`0750 resend-ticketd:resend-ticketd`
4. 安装 systemd unit 和 timer。
5. 运行配置检查。
6. 提示防火墙只开放实际监听端口。

systemd hardening 建议：

```ini
[Service]
User=resend-ticketd
Group=resend-ticketd
EnvironmentFile=/etc/resend-ticketd/.env
ExecStart=/usr/local/bin/resend-ticketd serve --config /etc/resend-ticketd/.env
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/resend-ticketd /var/log/resend-ticketd
CapabilityBoundingSet=
AmbientCapabilities=
```

如果监听 `443`，额外启用：

```ini
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
```

公网部署要求：

- 默认只开放 `9734/tcp`，如改为 `443` 则只开放 `443/tcp`。
- webhook endpoint 必须使用 HTTPS。
- 不建议把服务放在无防火墙、无 TLS、弱密码环境中。
- `.env`、TLS 私钥、DNS provider 凭据不得提交到 git。

## 构建和兼容性

目标平台：

- `x86_64-unknown-linux-musl`
- `aarch64-unknown-linux-musl`

采用 musl 静态链接发布，降低 CentOS 7 glibc 版本和系统 OpenSSL 差异带来的部署风险。

建议 release 产物：

```text
resend-ticketd-linux-amd64.tar.gz
resend-ticketd-linux-arm64.tar.gz
checksums.txt
```

## 验收测试

必须覆盖：

- 配置校验：缺少 secret、证书路径错误、数据库不可写、监听地址非法。
- webhook 签名：合法请求通过，非法签名拒绝，重复 event 幂等。
- 收信建单：新邮件建单，主题编号匹配，邮件头匹配。
- 工单状态：关闭后收到客户回复自动重开。
- 发信回复：调用 Resend Send Email API，带 `In-Reply-To` 和 `References`。
- 后台认证：登录、退出、会话过期、CSRF 拒绝、登录限速。
- HTTPS 启动：使用测试证书启动并访问 `/healthz`。
- 打包：两个 musl target release 构建成功，安装脚本在支持系统上冒烟通过。

## 参考文档

- [Resend Receiving Emails](https://resend.com/docs/dashboard/receiving/introduction)
- [Resend Get Email Content](https://resend.com/docs/dashboard/receiving/get-email-content)
- [Resend Verify Webhooks Requests](https://resend.com/docs/webhooks/verify-webhooks-requests)
- [Resend Send Email](https://resend.com/docs/api-reference/emails/send-email)
- [Resend Retrieve Received Email](https://resend.com/docs/api-reference/emails/retrieve-received-email)
- [Resend Reply to Receiving Emails](https://resend.com/docs/dashboard/receiving/reply-to-emails)
- [Resend Process Receiving Attachments](https://resend.com/docs/dashboard/receiving/attachments)
- [lego ACME client](https://github.com/go-acme/lego)
