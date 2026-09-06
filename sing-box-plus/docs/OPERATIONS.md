# 运维手册

## 部署

```bash
install -m 0755 sing-box-plus /usr/bin/sing-box-plus
install -m 0644 packaging/sing-box-plus.service /etc/systemd/system/
install -m 0644 packaging/sing-box-plus.sysusers /usr/lib/sysusers.d/sing-box-plus.conf
install -m 0644 packaging/sing-box-plus.tmpfiles /usr/lib/tmpfiles.d/sing-box-plus.conf
systemd-sysusers && systemd-tmpfiles --create
install -d -m 0750 /etc/sing-box-plus
install -m 0640 config/server.example.json /etc/sing-box-plus/config.json   # 改完再启
```

### 配置门禁必须用本项目的二进制

产出的二进制保持 `run` / `check` / `format` / `version` 的 argv 与退出码与上游兼容，
但**配置文件不再向上游二进制兼容**：统计配置以自有 `services[]` 类型承载，stock sing-box 会拒绝解析。

**不要用官方或 Homebrew 的 sing-box 校验本项目的配置。**

```bash
sing-box-plus -c /etc/sing-box-plus/config.json check
```

另外，上游 `sing-box check` 本身也不能当作唯一门禁：实测 v1.14.0 在 `ssm-api` 的 `servers` 键
缺前导 `/` 时会 panic 退出（exit=2）而非返回可读配置错误。本项目的全部校验在 `box.New()` 之前
独立完成；编排脚本应对 `check` 的**非 0 且非 1** 退出码单独告警。

### 启动会直接失败的配置

统计是硬依赖，不是可选旁路。下列任一情形都会**启动失败**，不会降级运行：

- 计费 inbound 不在白名单内（首期只有 `vless` 与 `shadowsocks`）；
- Shadowsocks 不是 `2022-blake3-aes-128-gcm` / `-aes-256-gcm` 的具名多用户形态
  （单用户 `password`、legacy AEAD、`destinations[]` relay、`managed: true` 一律拒绝）；
- 任一 `users[].name` 为空或在 inbound 内重复；
- 两个 `users[]` 归一化后的 uPSK 相同（上游会静默把两个计费身份合并成一个）；
- `listen_port` 缺省或为 0；`listen.detour` 非空；
- `route.rules[]` 出现 `reject` 或 `hijack-dns` 动作；
- 出现 `services[] type:"api"` / `ssm-api`、`experimental.clash_api`、`experimental.v2ray_api.listen`；
- `network_namespaces` 非空；
- 快照 socket 与配额 socket 使用同一路径。

## 采集

```bash
scripts/user-stats-client.py --socket /run/sing-box-plus/user-stats.sock
tests/reference_collector.py --socket /run/sing-box-plus/user-stats.sock \
  --ledger /var/lib/sing-box-plus/ledger --first-snapshot baseline
```

`--first-snapshot` **必须显式选择**，不留隐式行为：`baseline` 只建基线（降低重复风险），
`include` 首次累计全部计入（降低漏记风险）。

远程读取须经节点上的独立反向代理提供 HTTPS/mTLS/来源限制与审计，且该代理不得缓存快照。
UDS 不得直接映射为公网监听。

## 计划重启与最终结算

进程重启会清零内存计数。任何计划内的重启或配置变更按下列顺序执行，否则会留下未闭合窗口：

1. 停止接入新连接（下线、防火墙或上游负载均衡）；
2. 轮询快照的 `tcp_sessions` / `udp_sessions` 直至为 0 或达到超时；
3. 采集最终快照并**确认已持久化入账**；
4. 停止进程、应用变更、启动新进程（新 `runtime_id`）；
5. 采集端按首快照策略处理新周期。

**不要直接 `systemctl restart`**——那会跳过第 2、3 步。

仅调整用户集合时可用 `systemctl reload`（SIGHUP）代替重启：registry 跨 Box 存活，
`runtime_id` 与累计值都不变，代价是在途连接仍会被强制关闭。超时强切是允许的——已计字节不会丢失
——但必须在采集端标记该窗口为未排空以便审计。

**重载不变量**：`user_stats` 的 `node_id`、`listen_path` 与 `quota_control.listen_path` 必须与
进程启动时逐字节相同。任一变化会**拒绝本次重载、保留旧实例继续服务并告警**，进程不会退出。
需要更换这三项必须走上面的完整计划重启。

## 配额闸断（可选，默认关闭）

判定在 collector，进程只负责执行。启用后必须同时接受两件事：

1. **额度是纯内存的**，进程重启即全部解封，直到 collector 按新 `runtime_id` 重推。
   `PUT /v2/quota` 返回的 409 是这条义务的兜底信号。
2. **两个「没有额度信息时怎么办」的开关是商业决策，没有安全的默认值**：
   - `startup_action`：`allow`（默认）意味着重启窗口内超额用户可继续跑；`deny` 意味着
     collector 没起来时全节点不可用。
   - `stale_action`：`allow`（默认）意味着 collector 挂掉后所有人无限用；`deny` 意味着把
     collector 变成转发链路上的单点。

选哪个都要写进对外的服务说明，不要留给默认值替你做决定。

### 闸断的两点已知表现

- 客户端看到的是**连接建立后被重置**，不是协议层认证失败——tracker 位于握手之后。
- 被闸断身份的每次重连**仍会向目的地拨号一次**（完成握手、0 字节）。逐 lineage 的重连节流
  （`reconnect_throttle`）压低的是本机侧的握手后成本，**不消除拨号**。

## 访问审计（可选，默认关闭）

见 `docs/ACCESS_AUDIT.md`。一条必须先记住的：**不要给 `/var/lib/sing-box-plus/` 配 logrotate**，
文件由进程自轮转。

## 已知行为差异（相对上游 sing-box）

- 最小 registry 丢掉了「已移除类型」的三个友好报错桩：配置里写了被移除的旧类型时，
  你会看到一个通用的「未知类型」错误，而不是上游那句更具体的提示。
- 最小 DNS registry 只保留 `udp` / `tcp` / `local`。生产配置若需要 DoT/DoH 或 `hosts`，
  必须先按 README §6 末段补注册并走决策记录。
- 不支持 `daemon`、libbox、`boxdd` 与 `network_namespaces`；`merge` / `generate` / `tools` /
  `rule-set` / `geoip` / `geosite` / `api` / `schema` 子命令未复制。

## 回滚

发布产物是自包含二进制，回滚即换回上一版二进制并按上面的计划重启流程执行。
回滚**不需要**回滚 collector 侧的账本：`runtime_id` 会变，采集端按首快照策略处理新周期即可。
若回滚到不含 `user_stats` 的上游二进制，配置必须同时回滚——stock sing-box 无法解析本项目的配置。

## 排障

| 症状 | 首先检查 |
| --- | --- |
| 快照 404 | 采集器是否还在打 `/v1/snapshot`。本项目只有 `/v2/snapshot` |
| 快照 429 | 并发上限；采集端应重试且**不得入账** |
| `health` 有位为真 | 三位都是粘滞的。`identity_limit_reached` 意味着该 runtime 余下时间全部不可入账，必须走计划重启 |
| 采集端报四向计数倒退 | 失败关闭，不要猜测。先确认是否有第二个进程在同一 `node_id` 下产出快照 |
| 客户端连上就被重置 | 计费身份为空、未注册，或额度已耗尽。查进程日志里的「拒绝转发」行 |
| 进程反复退出 | exporter 与数据面同受监督：socket 绑定失败、accept 连续失败都会让进程失败退出 |
