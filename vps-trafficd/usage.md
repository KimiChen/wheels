# vps-trafficd 使用说明

`vps-trafficd` 是一个从 Linux 网卡字节计数器统计 VPS 流量的 Bearer Token
鉴权 JSON 服务。

## 构建

```bash
rustup target add x86_64-unknown-linux-musl
cargo build --release --target x86_64-unknown-linux-musl
```

内置 HTTPS 依赖 Rustls/ring；交叉构建 musl 时需要 `x86_64-linux-musl-gcc` 或 Zig 等 C 交叉编译器。
也可以在目标 Linux 机器上直接执行 `cargo build --release`。

静态二进制输出位置：

```text
target/x86_64-unknown-linux-musl/release/vps-trafficd
```

## 配置

```bash
sudo install -d -m 0755 /etc/vps-trafficd /etc/vps-trafficd/tls /var/lib/vps-trafficd
sudo install -m 0600 config/config.example.toml /etc/vps-trafficd/config.toml
sudo editor /etc/vps-trafficd/config.toml
```

启动前必须修改 `auth_token`、`interfaces`、`quota_bytes` 和 `cycle_anchor`。
如果 `auth_token` 为空或仍是示例值，服务会拒绝启动。
如果 `/etc/vps-trafficd/tls/fullchain.pem` 和 `/etc/vps-trafficd/tls/privkey.pem`
同时存在，服务会使用 HTTPS；两个文件都不存在时保持 HTTP。默认 `tls_auto_restart = true`，
服务会轮询证书和私钥内容变化，变化稳定后优雅退出并交给 systemd 重启加载新证书。

如果证书由 Nginx、Caddy 或 Certbot 维护，把 `tls_cert_path` / `tls_key_path` 指向它们的 PEM 文件即可。
使用 `ip-certd` 时可把证书拉到专用目录：

```bash
sudo IP_CERTD_INSTALL_ROOT=/etc/vps-trafficd/ip-certd \
  IP_CERTD_RELOAD_NGINX=0 \
  /usr/local/bin/pull-ip-certd-cert.sh https://example.com/api
```

然后把配置指向脚本输出目录中的 `fullchain.pem` 和 `privkey.pem`。

## 运行

```bash
vps-trafficd --config /etc/vps-trafficd/config.toml
vps-trafficd check --config /etc/vps-trafficd/config.toml
vps-trafficd calibrate --config /etc/vps-trafficd/config.toml --rx 1234 --tx 5678
```

安装 systemd unit：

```bash
sudo install -m 0755 target/x86_64-unknown-linux-musl/release/vps-trafficd /usr/local/bin/vps-trafficd
sudo install -m 0644 packaging/vps-trafficd.service /etc/systemd/system/vps-trafficd.service
sudo systemctl daemon-reload
sudo systemctl enable --now vps-trafficd
```

## API

```bash
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9733/api/v1/traffic
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9733/api/v1/config
curl http://127.0.0.1:9733/health
```

启用 TLS 后，把示例中的 `http://` 换成 `https://`。

`GET /api/v1/traffic` 必须携带 `Authorization: Bearer <token>`。鉴权失败返回
`401`，且不会暴露节点、网卡、流量或账期数据。`GET /api/v1/config` 和
`PUT /api/v1/config` 用于读取和更新不含 token 的配置字段，包括流量充值周期、流量限额和计费口径。
`PUT /api/v1/config` 也可以携带 `current_cycle_used_bytes` 来校准当前未完整周期的已用流量。
`GET /health` 公开访问，只返回最小健康信息。

浏览器打开 `/` 会弹框输入 Bearer token，页面可查看流量，并保存流量充值周期和流量限额到
`/etc/vps-trafficd/config.toml`；计费口径可选 total、rx、tx、max，其中 max 取接收/发送较大值。
“Traffic quota”和“Current cycle used” 会以 G 作为表单回填单位；“Current cycle used” 会更新状态文件中的校准偏移，用来计算当前周期剩余流量。
API 返回的 `rx_bytes` / `tx_bytes` 始终是网卡原始周期增量，校准只影响账单口径的 `used_bytes` 和 `remaining_bytes`。
