# Rust VPS 流量 JSON 服务：公网鉴权版

## Summary

构建一个 Linux Rust 常驻服务，适配 CentOS 7+、Debian 11+、Ubuntu 20.04+。程序默认监听公网地址，所有流量数据接口必须使用 Bearer Token 鉴权；通过读取指定网卡的 Linux 字节计数器，按购买日月周期计算 VPS 本账期已用和剩余流量。

## Key Changes

- 构建与兼容：
  - 发布 `x86_64-unknown-linux-musl` 静态二进制，避免 CentOS 7 的旧 glibc / OpenSSL 依赖问题。
  - 不依赖 systemd 以外的发行版特性；统计只读 `/sys/class/net/<iface>/statistics/{rx_bytes,tx_bytes}`。
  - 提供 systemd unit，适用于 CentOS 7+、Debian 11+、Ubuntu 20.04+。
- 默认配置：
  - `listen_addr = "0.0.0.0:9733"`，默认允许公网访问。
  - `auth_token` 必填；缺失、为空或仍是示例值时服务拒绝启动。
  - `interfaces = ["eth0"]`，用户按实际网卡名修改。
  - `node_id = "vps-trafficd-01"`，用于多 VPS 查询时区分节点；不参与鉴权。
  - `quota_bytes`、`billing_mode = "total"`、`cycle_anchor`、`cycle_months = 1`。
  - `state_path = "/var/lib/vps-trafficd/state.json"`。
- 鉴权与接口：
  - `GET /api/v1/traffic` 必须带 `Authorization: Bearer <token>`。
  - 鉴权失败返回 `401`，不泄露流量、网卡、账期等信息。
  - `GET /health` 可不鉴权，只返回最小健康信息，不包含敏感数据。
- 统计与账期：
  - 聚合配置中指定网卡的 rx/tx。
  - 接口同时返回 `rx_bytes`、`tx_bytes`、`used_bytes`、`remaining_bytes`、`usage_ratio`。
  - `used_bytes` 按 `billing_mode` 选择 rx、tx 或 total。
  - 根据购买日锚点推算当前账期；短月份没有对应日期时使用月末同一时间。
- 运维命令：
  - `vps-trafficd --config /etc/vps-trafficd/config.toml`
  - `vps-trafficd check --config ...` 检查配置、网卡、权限、token。
  - `vps-trafficd calibrate --rx <bytes> --tx <bytes>` 手动对齐服务商面板。

## Data Storage Plan

- 单 VPS 存储：
  - 不引入数据库，使用本地 JSON 状态文件持久化运行状态，默认路径为 `/var/lib/vps-trafficd/state.json`。
  - 配置仍放在 `/etc/vps-trafficd/config.toml`，包括 `interfaces`、`quota_bytes`、`billing_mode`、`cycle_anchor`、`cycle_months`、`auth_token`、`node_id`。
  - 状态文件保存当前账期边界、每个网卡的上次 rx/tx 计数器值、账期内累计 rx/tx、手动校准偏移、更新时间和状态格式版本。
  - 状态文件不保存 `auth_token`，避免泄露鉴权凭据。
- 多 VPS 存储：
  - 每台 VPS 各自运行一份 `vps-trafficd`，各自保存自己的 `/etc/vps-trafficd/config.toml` 和 `/var/lib/vps-trafficd/state.json`。
  - 多台 VPS 可以使用相同的默认 `state_path`，因为该路径只在本机文件系统内生效，不会互相冲突。
  - 不让多台 VPS 写同一个共享状态文件，不把 `state.json` 放到 NFS、对象存储或远程挂载目录上。
  - 不引入中心数据库；统一查看时由外部脚本或面板分别请求每台 VPS 的 `GET /api/v1/traffic`，再按 `node_id` 聚合展示。
  - 客户端汇总时可将各节点的 `quota_bytes`、`used_bytes`、`remaining_bytes` 相加；如果各 VPS 账期不同，应按节点分别展示账期边界。
- 状态更新：
  - 服务启动时如果状态文件不存在，以当前网卡计数器作为本账期基准创建初始状态。
  - 如果当前时间进入新账期，重置账期内累计值，并以当前网卡计数器作为新账期基准。
  - 如果当前网卡计数器小于上次记录值，视为系统重启、网卡重置或计数器回绕，不产生负增量，只更新基准值。
  - 写状态文件使用临时文件加原子 rename，避免写入过程中崩溃导致状态文件损坏。

## Test Plan

- 在 CentOS 7、Debian 11、Ubuntu 20.04 的容器或虚拟机中验证二进制可运行、systemd unit 可启动。
- 测试公网监听默认值为 `0.0.0.0:9733`，但 `/api/v1/traffic` 无 token 或 token 错误时返回 `401`。
- 测试购买日账期计算：跨月、跨年、29/30/31 号、短月份。
- 测试网卡计数器正常增长、系统重启归零、计数器变小后的累计逻辑。
- 测试 `rx` / `tx` / `total` 三种计费口径和剩余流量不低于 0。
- 测试 `check` 能发现缺失网卡、不可写状态目录、空 token、示例 token。
- 测试两台模拟 VPS 使用相同默认 `state_path` 时，只写各自本机状态文件，互不影响。
- 测试不同 `node_id` 的节点 API 返回可区分来源，外部汇总脚本能正确累加多节点用量。

## Assumptions

- 程序本身只提供 HTTP，不内置 HTTPS；公网部署时仍建议外层加 Nginx/Caddy/TLS。
- 默认公网监听是产品默认值，但 API 强制鉴权，避免裸露流量信息。
- 纯网卡计数器无法补回程序停止期间的流量；需要用 `calibrate` 手动校准。
- 多个 VPS 默认采用各自独立存储模式，不做中心端持久化；统一查看属于外部客户端或面板职责。
- 默认提供 amd64 静态二进制；如 VPS 是 ARM，再额外发布 `aarch64-unknown-linux-musl`。
