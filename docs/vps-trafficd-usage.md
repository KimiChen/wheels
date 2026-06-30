# vps-trafficd 使用说明

`vps-trafficd` 是一个从 Linux 网卡字节计数器统计 VPS 流量的 Bearer Token
鉴权 JSON 服务。

## 构建

```bash
rustup target add x86_64-unknown-linux-musl
cargo build --release --target x86_64-unknown-linux-musl
```

静态二进制输出位置：

```text
target/x86_64-unknown-linux-musl/release/vps-trafficd
```

## 配置

```bash
sudo install -d -m 0755 /etc/vps-trafficd /var/lib/vps-trafficd
sudo install -m 0600 config/config.example.toml /etc/vps-trafficd/config.toml
sudo editor /etc/vps-trafficd/config.toml
```

启动前必须修改 `auth_token`、`interfaces`、`quota_bytes` 和 `cycle_anchor`。
如果 `auth_token` 为空或仍是示例值，服务会拒绝启动。

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
curl http://127.0.0.1:9733/health
```

`GET /api/v1/traffic` 必须携带 `Authorization: Bearer <token>`。鉴权失败返回
`401`，且不会暴露节点、网卡、流量或账期数据。`GET /health` 公开访问，只返回最小健康信息。
