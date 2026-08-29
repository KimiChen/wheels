# shadowsocks-rust-plus

`shadowsocks-rust-plus` 是固定在 `shadowsocks-rust v1.24.0` 上维护的轻量
overlay。它已经实现 AEAD-2022 EIH 多用户服务端的用户级 TCP/UDP 应用负载累计统计，
并通过仅本机可访问的只读 HTTP/1.1-over-Unix-stream exporter 输出可结算快照；在启用
`user-audit` 时还会记录满足成功条件的 TCP/UDP 目标访问，并由独立 `shadowsocks-auditd`
负责本机 durable spool 与受 HMAC 保护的导出。

仓库包含完整实现、可重放补丁、自动化与集成测试、构建脚本和可复现性能工具。真实节点的灰度、
分批发布和上游提案属于生产或外部变更，必须另行取得明确授权。本仓库不包含真实节点、用户或
密钥信息。

## 固定基线

| 项目 | 值 |
| --- | --- |
| 上游 | `https://github.com/shadowsocks/shadowsocks-rust.git` |
| release | `v1.24.0` |
| commit | `7ee1aa9223ed8f4d34734aac919036c8ad4502c2` |
| 许可证 | MIT |
| 扩展方式 | 按 `patches/series` 顺序重放的 feature-gated overlay |

完整锁定信息见 [`upstream.lock`](upstream.lock) 与
[`docs/UPSTREAM_BASELINE.md`](docs/UPSTREAM_BASELINE.md)。准备脚本会校验精确提交；补丁无法
零 fuzz 应用时直接失败，不会静默跟随其他上游版本。

## 已实现能力

- 非默认 Cargo feature `user-stats`；未编译该 feature、非 Unix 平台却配置统计，都会明确失败。
- 非默认 Linux-only Cargo feature `user-audit`；它依赖 `user-stats`，未编译该 feature 但配置
  `user_audit` 时会明确失败，未配置时不创建任何审计 task 或元数据。
- 复用上游已经完成认证的 EIH 用户，不重新解析密码，也不把密钥或 identity hash 暴露给
  service/exporter。
- 每个用户维护 `tcp_uplink_bytes`、`tcp_downlink_bytes`、`udp_uplink_bytes`、
  `udp_downlink_bytes` 四个饱和 `u64` 累计值；溢出通过 `health` 报告，禁止回绕。
- 启用统计后，UDP 首包必须同时携带已认证 EIH 用户并取得该用户的活动计数器；任一条件缺失
  都会在创建 NAT 关联和发送目标数据报前失败关闭，不能继续转发但静默漏计。
- registry 导出进程 `runtime_id`、启动时间、严格递增的 `sequence`，以及稳定逻辑服务/用户的
  `generation`、`active`；同一 `server_id` 或用户名重激活时复用累计计数器，内部生命周期令牌会拒绝
  过期句柄。服务和用户按稳定顺序输出，逻辑记录数也有严格上限。
- 独立 HTTP/1.1-over-Unix-stream JSON v1 exporter，只接受 `GET /v1/snapshot` 与
  `GET /healthz`，具备请求/响应大小、读写超时、身份数和并发数上限。
- socket 默认 `0600`，支持受控 `0660`；绑定前使用严格 umask，逐级检查路径祖先的属主、权限
  与符号链接，使用同路径 lockfile 的非阻塞独占锁串行化清理与绑定。加锁后会重验
  lockfile 的路径/inode；Linux/Android 以原生 `open(2)` 精确传入 `O_PATH|O_NOFOLLOW` 固定
  socket inode，再通过 `/proc/self/fd` 修改权限，避免标准库访问模式在 musl 发布目标上把
  path-only 打开降为会对 Unix socket 返回 `ENXIO` 的只读打开。其他 Unix 使用不跟随符号链接的
  `fchmodat`，两者都会重验设备号、inode 和 mode。清理也按绑定后记录的设备号/inode 执行。
- 单个非法或超时的 exporter 请求在连接仍可写时只返回固定错误对象；客户端主动断开、响应
  写超时或截断也只影响该连接，不影响代理转发。
- 已启动的快照序列化即使遇到连接写超时也会继续持有该客户端的并发许可，直到后台工作及其
  响应缓冲真正结束；期间超出 `max_concurrent_clients` 的连接通常由有界 worker 返回 429，该 worker
  上限也满时则直接关闭，两种路径都不会绕过资源上限。
- exporter 启动失败会阻止 `ssserver` 启动；运行中的 exporter task 若意外退出、panic 或
  连续 3 次 `accept()` 失败，
  会由 relay 同一监督集合发现并使服务返回错误，避免“代理仍工作但统计已消失”。仓库提供的
  systemd 模板使用 `Restart=on-failure` 和 `RestartSec=3s` 自动重启整个服务。
- 运行时未配置顶层 `user_stats`/`user_audit` 时不创建对应 registry、exporter 或 auditd producer，单 server 保留上游快路径；
  Shadowsocks 线协议和 ACL 语义不变。统计模式与 builtin/standalone `ssmanager` 互斥，避免动态
  `add` 出现未注册、未计费却继续转发的服务。

## 用户成功访问审计

用户成功访问审计的权威开发合同见
[`docs/USER_ACCESS_AUDIT.md`](docs/USER_ACCESS_AUDIT.md)。交付包括 `0003-user-audit.patch`、
`shadowsocks-audit-protocol`/`shadowsocks-auditd` crates、共享 producer/AuditSupervisor、严格
ingest/export/spool/HMAC 协议、systemd/sysusers/tmpfiles 模板、双二进制 release manifest、故障与
协议测试，以及供下游开发使用的 [`tests/mock_collector.py`](tests/mock_collector.py)。审计只记录成功
目标访问和有界诊断 gap；auditd/collector/controller、数据库、反向代理、生产部署和业务管理系统仍由
下游集成方负责。功能边界、字段和失败语义以该规格为唯一权威。

## 快速开始

环境要求：Unix domain socket、Git、Rust/Cargo、Bash、Python 3、`patch`、`tar`、
`ripgrep`（命令名 `rg`），以及获取固定上游源码所需的网络访问；发布构建校验还需要
`cargo-zigbuild`、Zig、`shasum` 和 OpenSSL，且版本必须与
[`packaging/release-toolchain.lock`](packaging/release-toolchain.lock) 一致。`rg` 是完整验证的
必需依赖，缺失时 `verify.sh` 必须失败，不能把敏感扫描跳过后视为全绿。

可选环境变量见 [`.env.example`](.env.example)。需要覆盖上游仓库或 Cargo 缓存路径时，将它
复制为项目根目录下的 `.env` 并填写本机值；项目脚本会自动加载该文件。`.env` 已被忽略，真实
路径、凭据和密钥不得提交。

### 1. 本地验证和开发构建

Linux 主机直接运行完整开发路径：

```bash
./scripts/verify.sh
./scripts/build.sh --output-dir .cache/dev-dist
(cd .cache/dev-dist && shasum -a 256 -c ssserver.sha256 && shasum -a 256 -c shadowsocks-auditd.sha256)
```

macOS 等非 Linux 主机的 auditd 静态交叉检查使用默认 `x86_64-unknown-linux-gnu` target，也可通过
`SHADOWSOCKS_AUDIT_CHECK_TARGET` 选择另一个 target。推荐先安装该 target；未安装时
`verify.sh`/`test.sh` 会继续执行其余检查并明确打印“未验证”，设置
缺失 target 默认即为失败（fail-closed）；确知要放弃该覆盖面时用 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0` 显式降级。非 Linux 开发构建仍必须显式
关闭 auditd：

```bash
rustup target add x86_64-unknown-linux-gnu
SHADOWSOCKS_AUDIT_CHECK_TARGET=x86_64-unknown-linux-gnu ./scripts/verify.sh
./scripts/build.sh --without-audit
(cd .cache/dev-dist && shasum -a 256 -c ssserver.sha256)
```

`verify.sh` 会核对远端 tag 与锁定 commit、在临时目录重放补丁、运行 Rust/结算/HTTP Unix 与
mock-collector 测试，并对未被 ignore 规则排除的文件扫描常见私钥、`AKIA` access key ID 以及
`PrivateKey`/`Passphrase` 赋值。该检查用于提交前卫生检查，不是通用的 `secret`/`token`
扫描器，也不会检查被忽略的部署 `.env`。`build.sh` 在 Linux 主机默认构建启用 `user-audit`
feature 的开发用 `ssserver` 与 `shadowsocks-auditd`；该 feature 在非 Linux 主机上明确拒绝编译，
因此 macOS 等平台的本地构建应显式使用 `--without-audit`。Linux 发布构建仍必须使用专用脚本。
生成的源码、Cargo target
和 `dist/` 不应提交。

由于 `shadowsocks-auditd` 明确是 Linux-only，非 Linux 主机上的 `verify.sh` 会运行 feature-off
workspace、service/协议测试，并在 target 已安装时对上述 Linux target 执行 auditd
`cargo check --all-targets`；target 缺失时只报告未验证，使用
该门禁默认 fail-closed，`SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0` 才显式降级为“未验证”。auditd 原生单元/集成测试和完整
`user-audit` feature workspace 回归只能在 Linux 主机执行，且仍是发布前置硬条件。

### 2. Linux x86_64 可复现发布包与验签

生产候选包使用锁定的 Rust、Cargo、cargo-zigbuild、Zig 与 Python 版本，两次在不同源码/target
路径独立构建 `x86_64-unknown-linux-musl`，二进制逐字节相同才生成包含两个 ELF、两个 SHA-256
文件和规范 manifest 的发布目录：

```bash
./scripts/build-linux-release.sh --repository /absolute/path/to/upstream-mirror

release_dir=dist
./scripts/sign-release.sh \
  --release-manifest "$release_dir/release-manifest.json" \
  --private-key /secure/offline/release-private.pem \
  --output "$release_dir/release-manifest.sig"
./scripts/verify-release.sh \
  --release-manifest "$release_dir/release-manifest.json" \
  --signature "$release_dir/release-manifest.sig" \
  --public-key /secure/release-public.pem
```

`--repository` 可省略并使用 `upstream.lock` 的地址，也可指向本地镜像；两种路径都会由
`prepare-source.sh` 校验精确 tag/commit 并零 fuzz 应用 overlay。发布构建要求当前 overlay
工作树干净。固定版本见 [`packaging/release-toolchain.lock`](packaging/release-toolchain.lock)。
manifest 包含版本、上游 commit、overlay commit、目标、`SOURCE_DATE_EPOCH`、两次独立构建记录、
完整工具链和两个 ELF 的 SHA-256；验签工具还会验证 detached 签名、两个二进制 checksum、
manifest 字段、ELF64/x86_64 头和确定性输出元数据。发布私钥必须离线保管，不能放入仓库或构建主机。
overlay 还把上游默认的实时时钟 build timestamp 改为从 `SOURCE_DATE_EPOCH` 派生的固定 UTC 值；
未经过发布脚本显式设置时显示 `unknown`，避免伪造可复现结果。

### 3. 五节点凭据源和配置

当前五节点部署 profile 固定使用 `2022-blake3-aes-128-gcm`。全集群只有一个共享 iPSK，每个
用户只有一个独立 uPSK；两者都必须是 16 字节安全随机值的带 padding 标准 Base64。同一用户的
完整客户端密码为 `shared-iPSK:uPSK`，五个节点只改变地址和节点名称。

仓库工具默认生成 200 个正式账号和 4 个可辨识的测试账号（正式账号也支持 200+）：

```bash
install -d -m 0700 .artifacts/credentials
./scripts/cluster-users.py generate \
  --formal-count 200 \
  --test-count 4 \
  --test-prefix test_ \
  --output .artifacts/credentials/cluster-users.json

# 导入已有受控源时，校验后按 name 排序到一个新的、未存在的文件。
./scripts/cluster-users.py normalize \
  --input /secure/import/cluster-users.json \
  --output .artifacts/credentials/cluster-users.normalized.json

# 渲染 ssserver 可直接注入的 users[]；输出仍含密钥并保持 0600。
./scripts/cluster-users.py render-users \
  --source .artifacts/credentials/cluster-users.json \
  --output .artifacts/credentials/ssserver-users.json
```

生成和规范化命令绝不向 stdout/stderr 输出 iPSK/uPSK，只写入显式目标；目标以 `0600` 创建并
禁止覆盖，父目录不得允许 group/other 写入。仓库内目标还必须已经被 Git ignore，推荐使用
`.artifacts/`；真实凭据产物不得执行 `git add -f`。部署系统应从这一份源原样注入五份配置，禁止
人工分别维护 `users[]`。私有源 schema v2 的每项另含 `kind: formal|test`，规范顺序是正式账号
按 name 排序后接测试账号按 name 排序；注入 ssserver 时必须剥离 `kind`，运行配置的用户项仍然
严格只有 `name`/`password`。`render-users` 已实现这一投影，不应另写临时文本处理命令。

配置注入完成后，在启动服务前执行跨五配置校验：

```bash
./scripts/cluster-users.py verify-five \
  --source .artifacts/credentials/cluster-users.json \
  --expected-formal-users 200 \
  --expected-test-users 4 \
  --config /secure/staging/node-1.json \
  --config /secure/staging/node-2.json \
  --config /secure/staging/node-3.json \
  --config /secure/staging/node-4.json \
  --config /secure/staging/node-5.json
```

校验会失败关闭地确认：五份文件互不相同且各含一个服务、node/service ID 全集群唯一、method
固定为 AES-128-GCM、共享 iPSK 相同、正式/测试账号数量分别正确，以及剥离 `kind` 后规范化的
`users[]` 名称、顺序和 uPSK 与受控源逐项完全一致；成功输出不含密钥或密钥摘要。

以 [`config/server.example.json`](config/server.example.json) 为结构模板，在部署系统中生成
并注入新的 iPSK/uPSK。不要把替换后的配置提交到公开仓库。最小结构如下：

```json
{
  "user_stats": {
    "node_id": "node-example-01",
    "socket_path": "/run/shadowsocks-rust-plus/user-stats.sock"
  },
  "servers": [
    {
      "id": "ss-entry-01",
      "server": "127.0.0.1",
      "server_port": 19999,
      "method": "2022-blake3-aes-128-gcm",
      "password": "<运行时注入的 16 字节 Base64 iPSK>",
      "mode": "tcp_and_udp",
      "users": [
        {
          "name": "u_000123",
          "password": "<运行时注入的 16 字节 Base64 uPSK>"
        }
      ]
    }
  ]
}
```

底层 overlay 仍兼容上游两种 EIH AES method，但当前五节点部署采用失败关闭策略并固定选择
AES-128-GCM。每个 `servers[]` 都必须满足：

- 显式提供节点内唯一的 `id`；
- 本部署 method 必须是 `2022-blake3-aes-128-gcm`，iPSK/uPSK 均为 16 字节标准 Base64；
- 至少配置一个 `users[]`，不接受仅用主身份凭据产生的无归属流量；
- `id`、`user_stats.node_id` 和 `users[].name` 均为非空、最多 128 字节的 ASCII 可显示非空白字符；
  `users[].name` 在服务内唯一。

只要有一个服务不满足这些条件，配置完整性检查就会失败，统计不会以部分覆盖或零归属模式启动。
`user_stats` 配置不接受未知字段，`socket_mode` 只接受精确字符串 `"0600"` 或 `"0660"`。
`ConfigType::Manager` 的 builtin 和 standalone 模式均不得与 `user_stats` 同时启用；需要统计时必须使用静态
`servers[]` 配置，manager 动态 `add` 不在支持范围内。
全部配置字段、默认值和硬上限见 [`docs/API.md`](docs/API.md) 与示例配置；部署权限和 systemd
步骤见 [`docs/OPERATIONS.md`](docs/OPERATIONS.md)。

### 4. 读取快照

```bash
curl --fail-with-body --silent --show-error \
  --unix-socket /run/shadowsocks-rust-plus/user-stats.sock \
  http://localhost/v1/snapshot
```

也可用会额外校验 schema 与健康状态的仓库客户端：

```bash
./scripts/user-stats-client.py \
  /run/shadowsocks-rust-plus/user-stats.sock \
  --require-healthy --compact
```

客户端的 `--timeout` 是从连接开始到完整响应读取结束的整体 deadline，必须是有限正数；慢速
滴灌响应不会在每次收到字节后重新获得一整段超时。

exporter 每条 Unix stream 连接只处理一个严格 HTTP/1.1 请求；除 `GET /v1/snapshot` 与
`GET /healthz` 外的方法、路径、绝对形式 URI 和带请求体的请求均被拒绝。成功响应、固定错误
对象、字段和排序契约见 [`docs/API.md`](docs/API.md)。

Unix socket 不得直接映射为公网监听。远程读取必须经过节点上的独立反向代理，并由该代理提供
HTTPS、mTLS、来源限制和审计；`ssserver` 本身仍只监听本机 Unix socket。

## 统计与结算语义

计数口径是认证并解密后、成功进入转发边界的应用负载，不包括 Shadowsocks/EIH、TCP、UDP、
IP 或隧道封装，不包括 TCP 重传，也不记录目标地址、客户端地址或连接明细。精确的 TCP/UDP
唯一计数点和失败语义见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。

exporter 只输出当前进程生命周期内的累计值，不持久化账单，也不决定新运行周期首个快照是否
入账。控制面计算差值时必须使用完整基线键：

```text
node_id + server_id + server_generation + identity_name
        + identity_generation + runtime_id
```

完整键中的 generation 不得省略，以保持 schema 和未来兼容性。当前 exporter 在同一 runtime 内把
`server_id` 和用户 `name` 视为稳定逻辑身份：删除后保留记录且标记为 inactive，同名重激活会复用
`generation=1` 和原累计计数器。因此不得在同一 runtime 内把已用名称重分配给不同计费用户；需要
改变归属时应使用新名称，或通过受控重启进入新 `runtime_id`。幂等批次 ID 还必须包含快照序号和
本次增量；重复/倒退的
`sequence`、倒退的累计值或不健康快照都不得入账。新的 `runtime_id` 表示进程重启和计数归零，
首快照采用 `baseline` 还是 `include` 必须由控制面明确选择。

参考实现和契约测试位于 [`tests/settlement_model.py`](tests/settlement_model.py) 与
[`tests/test_settlement.py`](tests/test_settlement.py)。计划重启必须先停止接入、排空、采集最终
快照并确认入账，再停止服务；异常退出留下的未闭合窗口必须单独审计。

## 故障模型

exporter 是统计启用时的必要服务，而不是可选旁路：

1. 配置、父目录、lockfile、遗留 socket 或 bind 检查失败时，`ssserver` 启动失败。
2. 启动后，relay 和 exporter 一起受监督；exporter task 意外结束，或 `accept()` 连续失败 3 次，
   都会使 `ssserver` 以失败返回。
3. 使用仓库的 [`packaging/shadowsocks-rust-plus.service`](packaging/shadowsocks-rust-plus.service)
   时，systemd 在 3 秒后重启整个进程。应对连续重启告警，不能通过放宽目录权限或删除活动
   socket 绕过安全检查。
4. 单个客户端的畸形请求、超时、超限或写失败由 exporter 隔离，不会触发数据面重启。

这条链路保证 exporter 不会静默消失，但 systemd 重启也会产生新的 `runtime_id`；采集器必须按
前述结算屏障和异常窗口规则处理。

## 测试与性能

运行完整自动化验证：

```bash
./scripts/verify.sh
```

测试范围、单独运行方法和上游已知基线问题见 [`tests/README.md`](tests/README.md)。仓库提供两套
可复现性能工具，但参考结果不构成生产保证：

- [`tests/benchmark_snapshot.py`](tests/benchmark_snapshot.py)：100/500/1000 身份的快照延迟、
  响应大小和 RSS，1000 身份单次响应设置 `< 1s` 自动门槛。
- [`tests/benchmark_data_path.py`](tests/benchmark_data_path.py)：同一回环工作负载比较精确锁定的
  原始上游、已编译 `user-audit` 但运行时关闭统计/审计、运行时同时启用统计与审计三种 release
  配置，记录吞吐、CPU、RSS、真实 worker outcome、durable ingest 证据和构建产物哈希。

参考测量摘要、完整工作负载和结果解释见 [`docs/PERFORMANCE.md`](docs/PERFORMANCE.md)；原始
临时报告不提交，以免把单一环境的机器信息误当成发布基线。

生产候选版本仍须在目标机型按 [`tests/README.md`](tests/README.md) 的命令生成并归档报告，连同
并发数、TCP/UDP 比例、数据块大小和机器信息一起评审。

## 文档导航

- [用户统计接口 v1](docs/API.md)：HTTP/1.1-over-UDS 路由、成功/错误响应、字段与结算契约。
- [中控统计与 MySQL 设计](docs/CONTROL_PLANE_USAGE_STATISTICS.md)：主动上报、幂等结算、lineage 和存储/聚合设计。
- [架构决策与代码路径](docs/ARCHITECTURE.md)：manager 取舍、TCP/UDP 身份路径、计数点和生命周期。
- [安装、监控与回滚](docs/OPERATIONS.md)：生产前检查、systemd、采集、重启屏障和回滚。
- [固定上游基线](docs/UPSTREAM_BASELINE.md)：tag/commit、已知测试基线和升级规则。
- [参考性能基线](docs/PERFORMANCE.md)：快照规模与回环数据面对照结果、局限和复测要求。
- [用户成功访问审计规格](docs/USER_ACCESS_AUDIT.md)：审计事件、ingest/spool/export 协议、HMAC、保留和验收合同；
  该文件同时是第 1–8 轮审计与 Linux 实装验收的历史档案。
- [用户成功访问审计待办（V2）](docs/USER_ACCESS_AUDIT_V2.md)：仍未闭合的问题、待执行的验证与需决策事项。
- [测试与性能](tests/README.md)：自动化测试、集成测试和 benchmark 运行方式。

## 范围边界

本项目只负责数据面身份归属、累计计数和本机只读导出，不负责订阅生成、用户/套餐/余额管理、
账单存储、管理后台、配置分发、链路编排、硬配额、限速或实时断开。它统计用户实际代理的应用
负载，不能替代云厂商或 VPS 的网卡计费统计。

外部采集器、账务系统和生产发布系统必须各自实现持久化、幂等、告警、审批和密钥管理。公开
仓库中的示例只能使用占位值；真实域名、地址、证书、密码和密钥不得提交。

## 许可证与维护

本项目和锁定上游均按 MIT 许可证分发，见 [`LICENSE`](LICENSE) 与
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md)。升级上游必须先更新锁定信息、重新勘察
身份/计数路径、零 fuzz 重放补丁并完成全部测试；不得把“补丁仍可应用”当作语义兼容证明。
