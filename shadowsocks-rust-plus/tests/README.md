# 测试与性能

仓库提供可重复的单元、编译、协议/结算、真实 TCP/UDP 与 auditd 集成和性能测试。测试通过只证明指定环境中的
实现与契约满足当前断言；生产灰度、目标节点性能验收和分批发布仍需独立授权与证据。

## 一键验证

从项目根目录执行：

```bash
./scripts/verify.sh
```

`verify.sh` 会校验 `upstream.lock` 中的 `v1.24.0` tag/commit、检查补丁序列、在临时
目录零 fuzz 重放 overlay、调用 `scripts/test.sh`，最后对未被 ignore 规则排除的文件扫描常见
私钥、`AKIA` access key ID 和 `PrivateKey`/`Passphrase` 赋值。扫描只用于提交前卫生检查，不覆盖
通用 `password`/`secret`/`token` 赋值，也不扫描被忽略的部署 `.env`。它需要访问锁定的上游
仓库，并要求预先安装 `ripgrep`（命令名 `rg`）。这是正式验证门禁而不是可选测试依赖：缺少
`rg` 或 OpenSSL 时 `verify.sh` 保持失败，不以 skip 制造全绿；全绿结果只适用于依赖完整的环境。

已经准备好补丁后源码时，可直接运行：

```bash
./scripts/test.sh --source /path/to/prepared-shadowsocks-rust
```

只运行 Rust/编译测试而跳过 Python 结算和真实进程集成测试：

```bash
./scripts/test.sh --source /path/to/prepared-shadowsocks-rust --no-integration
```

`scripts/test.sh` 的固定检查包括：

1. Linux 上运行 workspace lib/bin 的 `user-audit` Rust 单元测试；非 Linux 上改跑 `user-stats`
   feature-off 路径并排除 Linux-only auditd；
2. core 的 AEAD-2022 TCP EIH 认证用户透传测试；
3. service 只启用普通 `server`、不启用 `user-stats` 的独立编译，防止 Cargo workspace
   feature 合并掩盖 gating 错误；
4. HTTP response framing 与 snapshot schema 的 Python 单元测试；
5. 结算模型契约测试；
6. 私有凭据源生成、规范化和五配置一致性工具测试；
7. 确定性 Linux 双二进制发布目录、manifest、SHA-256 和 detached 签名验签测试；
8. 真实 `ssserver`/`sslocal` TCP+UDP 集成测试；
9. `shadowsocks-audit-protocol`/`shadowsocks-auditd` 单元测试与 mock collector 协议测试。

`shadowsocks-auditd` 是 Linux-only。在 macOS 等非 Linux 主机上，脚本会从宿主 workspace 测试中排除
auditd，并把 workspace feature 降为 `user-stats`；producer 的 `user-audit` 单元测试不会在宿主
运行。协议单元测试仍会执行，auditd 则对已安装的 `SHADOWSOCKS_AUDIT_CHECK_TARGET`（默认
`x86_64-unknown-linux-gnu`）做 `cargo check --all-targets`。先运行
`rustup target add x86_64-unknown-linux-gnu`（或安装所选 target）即可启用该检查；未安装时脚本继续
其余检查并明确打印“未验证”，该降级需显式设置 `SHADOWSOCKS_REQUIRE_AUDIT_TARGET=0`；默认缺失 target 即失败。auditd
运行时测试、producer feature-on 测试和完整 workspace 回归必须在 Linux 主机执行并作为发布硬门禁。

不准备上游源码、也不访问网络时，可单独运行新增的纯本地工具测试：

```bash
python3 tests/test_cluster_users.py
python3 tests/test_release_artifact.py
python3 tests/test_mock_collector.py
```

凭据工具测试会实际生成 205 个正式账号和 4 个测试账号，确认 kind 区分及剥离、每个 iPSK/uPSK
都是 16 字节标准 Base64、用户名和 uPSK 唯一、输出精确 `0600`、禁止覆盖、拒绝仓库内未 ignore 目标，并覆盖规范排序、五配置完全
一致、顺序漂移与 ID 冲突。随机凭据只存在于权限受限的临时目录，测试输出和失败消息不得包含
它们。发布测试使用无密钥的最小 ELF x86_64 fixture 两次打包并比较全部字节，校验双二进制
manifest、两个二进制及 checksum 防篡改；package 写入测试覆盖额外成员插入、输出目录替换、原目录
清理和 stat/open 身份竞态。本机有 OpenSSL 时还会临时生成测试专用密钥，覆盖 detached 签名成功、
拒绝覆盖、manifest/signature/artifact A/B 路径替换、额外目录成员、仓库末检漂移、签名清理和篡改
验签失败。构建 fixture 还会检查 source-root cwd、独立空 `CARGO_HOME`、Cargo config 搜索路径拒绝、
Cargo/Rust 环境污染隔离，以及 Python 元数据必须绑定实际运行的解释器而非 `PATH` 中同名程序。
临时密钥与产物不会写入仓库。

## 覆盖范围

### 配置与安全边界

Rust 配置测试覆盖默认值和 JSON round-trip、未知 `user_stats` 字段拒绝、只接受精确
`"0600"`/`"0660"` 的 socket mode、未编译 feature 却配置统计、server ID 和用户名重复、用户密码
重复、ASCII 可显示非空白识别符规则（包括 Cf 类不可见 Unicode 拒绝）、规范绝对路径、逐级祖先
符号链接/属主/权限、
所有软上限与硬上限。启用统计时还验证：

- 数据面兼容性只允许 `2022-blake3-aes-128-gcm` 与 `2022-blake3-aes-256-gcm`；五节点部署工具
  进一步固定 AES-128-GCM 与 16 字节 Base64 iPSK/uPSK；
- 每个服务至少有一个 EIH 用户；
- 非 EIH method、无用户服务和仅主身份服务均失败关闭。

测试使用运行时随机 PSK，不在源码或报告中保存可复用密钥。

### Registry、exporter 与监督生命周期

Rust 单元/Unix 测试覆盖：

- 四向 `u64` 饱和与 `counter_overflow`、序号饱和与 `sequence_overflow`；
- UDP 统计开启时缺失认证用户或活动计数器会在关联创建前返回 `PermissionDenied`；合法用户正常
  建立会话，统计关闭时保持上游身份行为；
- 稳定逻辑服务/用户的 generation/active，同名重激活复用 `generation=1` 与累计计数器，过期内部
  lifecycle 句柄拒绝、查询/停用遵循 `state -> users` 锁顺序，以及稳定排序；
- 连续 10,001 次同名重激活后记录数仍有界、累计值单调；逻辑用户名和 server ID 的独立上限，
  以及超限失败不污染 registry 映射；
- 锁外 JSON 序列化、body 末尾换行和最大响应限制；
- HTTP/1.1 `GET /v1/snapshot` 与不推进序号的 `GET /healthz`，以及固定状态码、headers 和错误
  DTO；
- HTTP 标准要求的 HEAD 405、`Allow: GET`、representation `Content-Length` 与空 wire body；
- HTTP/1.0、absolute-form、query、缺少/重复 Host、任意 `Content-Length`/
  `Transfer-Encoding`、其他方法/路径和 preflight 已缓冲的尾随字节均严格拒绝；响应开始后
  迟到且没有 body framing 的字节会随连接关闭被丢弃，永远不会作为第二个请求处理；
- 原始 request-line/headers/终止 CRLF 与 JSON body 的大小上限、64-header 硬上限、读写超时和
  最大并发；
- 用确定性快照门闩验证：连接写超时后，未结束的阻塞序列化仍持有并发许可，后续请求返回 429；
  后台任务完成后许可恢复且没有泄漏；
- 当第一个 429 客户端挂起写入时 accept 循环仍能处理后续连接；独立 busy-response worker 上限 32、
  超限立即关闭且任务数有界；
- socket mode、活动 socket/普通文件拒绝、遗留 socket 替换；Linux/Android 固定 `O_PATH`
  socket inode 后通过 `/proc/self/fd` 改 mode，其他 Unix 使用 no-follow `fchmodat`；符号链接替换
  不改动目标文件，以及 fd/path 设备号、inode、mode 重验与安全清理；
- 模式 `0600` 的 `.lock` 文件和非阻塞独占 `flock`，加锁后 lockfile 路径/inode 替换检测，第二个并发
  bind 稳定失败，释放后可重启；
- 同一源 IP 更换端口不能绕过 60 秒认证/统计拒绝日志限频，不同 IP 独立，LRU 容量为 4096；
- exporter 辅助 task 的失败以及连续 3 次注入的 `accept()` 失败由 relay 同一 supervisor 传播，使
  `ssserver` 失败退出。

最后一项验证的是 task 级故障模型。单个 exporter 请求错误由连接处理层隔离，不应让
`ssserver` 退出；exporter task 自身任意异常退出或 panic 则不能静默忽略，生产模板交给
systemd `Restart=on-failure` 恢复整个进程。

### 数据面集成

[`integration_user_stats.py`](integration_user_stats.py) 构建并启动真实
`ssserver`/`sslocal`，使用本机 echo 服务验证两个 EIH 用户的并发 TCP/UDP 归属、四个计数
方向、100 用户配置、认证失败不入账、HTTP/1.1-over-Unix-socket 接口和进程重启后的新
`runtime_id`/归零。
测试不访问公网服务，iPSK/uPSK 在每次运行时随机生成，并只写入权限受限的临时目录。

### 结算契约

[`test_settlement.py`](test_settlement.py) 与
[`settlement_model.py`](settlement_model.py) 覆盖累计差值、`baseline`/`include` 首快照
策略、陈旧/重复序号、计数回退、未知 `identity_kind`、同一 runtime 的固定启动时间、已观察
服务/身份 lineage 消失、原子拒绝和幂等批次。不健康快照以及多服务中任一计数回退都不会推进
序号或部分提交基线。持久化基线键固定为：

```text
node_id + server_id + server_generation + identity_name
        + identity_generation + runtime_id
```

契约模型还使用显式保留多个 generation lineage 的 fixture，确认完整键分别结算且任一已观察 lineage
消失都会使整份快照原子拒绝。当前 exporter 的同名重激活会复用 `generation=1` 和原累计计数器，但控制面
仍必须保留 generation 维度以遵循 v1 schema 并兼容未来实现。

### 审计协议 mock collector

[`mock_collector.py`](mock_collector.py) 是仅用于开发和协议互通测试的无第三方依赖 collector。它在
解析 lease body 前验证响应 body SHA-256、节点标识和 HMAC，逐行校验 wrapper 的 spool epoch/sequence、
事件 payload digest 与连续范围，再把 `(node_id, event_id)` 和 `(node_id, batch_id)` 的幂等状态写入可选
的 `0600` JSON 状态文件。冲突只被记录到隔离列表，不覆盖已有事件。固定请求/响应 HMAC vectors 与
重复 key、非有限数字、BOM、NDJSON 缺行等拒绝用例见 [`test_mock_collector.py`](test_mock_collector.py)。

对运行中的 auditd 做一次本机采集（不会输出 event body 或 key）：

```bash
python3 tests/mock_collector.py \
  --socket /run/shadowsocks-audit/export/export.sock \
  --node node-example-01 \
  --key-file /etc/shadowsocks-audit/export-hmac \
  --state /var/lib/shadowsocks-audit/mock-collector.json
```

该工具不是生产 collector；真实 controller 仍需按 `docs/USER_ACCESS_AUDIT.md` 的 durable commit、
跨节点隔离和 retention 合同实现。

## 快照性能

先准备 plus 源码，再运行 release exporter benchmark：

```bash
./scripts/prepare-source.sh /tmp/shadowsocks-rust-plus-source
./tests/benchmark_snapshot.py \
  --source /tmp/shadowsocks-rust-plus-source \
  --output /tmp/shadowsocks-rust-plus-snapshot.json
```

默认测试 100、500、1000 个身份，每组 25 次，通过 `GET /v1/snapshot` 输出 JSON body 大小、
HTTP/1.1-over-UDS 往返延迟分布、进程 RSS 和工具链信息；任一采样达到 1 秒会失败。可用
`--samples`、`--identities` 和 `--target` 调整。benchmark 必须校验 200 状态、响应 headers、
完整 body 和 schema，不能只从流中寻找 JSON。

## 数据面性能

[`benchmark_data_path.py`](benchmark_data_path.py) 在同一回环 echo 工作负载下比较：

1. 精确锁定 commit 的原始上游 release；
2. 编译 `user-audit` feature、但运行时没有 `user_stats`/`user_audit` 配置的 plus release；
3. 同一 plus 二进制、运行时同时启用统计与审计。

准备一个 HEAD 为锁定 commit 且工作树完全干净的原始上游 Git checkout，以及一个从该 commit
按 `patches/series` 顺序逐个执行 `git am`、工作树同样干净的 plus Git checkout：

```bash
project_root="$(pwd -P)"
benchmark_root=/tmp/shadowsocks-rust-plus-benchmark
mkdir "$benchmark_root"

git clone --no-checkout \
  https://github.com/shadowsocks/shadowsocks-rust.git \
  "$benchmark_root/upstream"
git -C "$benchmark_root/upstream" checkout --detach \
  7ee1aa9223ed8f4d34734aac919036c8ad4502c2

git clone --no-checkout \
  https://github.com/shadowsocks/shadowsocks-rust.git \
  "$benchmark_root/plus"
git -C "$benchmark_root/plus" checkout --detach \
  7ee1aa9223ed8f4d34734aac919036c8ad4502c2
while IFS= read -r patch_name; do
  [[ -z "$patch_name" || "$patch_name" == \#* ]] && continue
  git -C "$benchmark_root/plus" \
    -c user.name=shadowsocks-rust-plus \
    -c user.email=noreply@shadowsocks-rust-plus.invalid \
    am "$project_root/patches/$patch_name"
done < "$project_root/patches/series"
```

上述 `benchmark_root` 必须事先不存在；命令不会覆盖已有目录。该 benchmark 只接受 Linux，且要求
目标 auditd 已经运行。取得 systemd 主进程 PID 后运行：

```bash
auditd_pid="$(systemctl show \
  --property MainPID --value shadowsocks-auditd.service)"
test "$auditd_pid" -gt 0

sudo ./tests/benchmark_data_path.py \
  --upstream-source /tmp/shadowsocks-rust-plus-benchmark/upstream \
  --plus-source /tmp/shadowsocks-rust-plus-benchmark/plus \
  --audit-ingest-socket /run/shadowsocks-audit/ingest/ingest.sock \
  --auditd-user shadowsocks-audit \
  --producer-user shadowsocks-benchmark \
  --audit-export-socket /run/shadowsocks-audit/export/export.sock \
  --audit-export-user shadowsocks-audit-export \
  --audit-hmac-key-file /etc/shadowsocks-audit/export-hmac \
  --audit-node-id node-example-01 \
  --auditd-pid "$auditd_pid" \
  --output /tmp/shadowsocks-rust-plus-data-path.json
```

脚本必须以 root 运行，且 `shadowsocks-audit`、`shadowsocks-benchmark`、
`shadowsocks-audit-export` 必须是三个不同 UID；producer 必须获准连接 ingest socket，export peer 必须
获准连接 export socket。HMAC key 文件须由 auditd UID 持有且 mode 为 `0600`，内容不会写入报告。
脚本会在运行前后验证 PID、有效 UID、可执行文件 inode/SHA-256、ingest/export socket 身份，并通过
签名 health 的前后 `stored_records` 增量绑定 producer runtime；启用审计的测量样本会直接采集 auditd
RSS。默认运行 5 个测量样本和 1 个 warm-up，使用 4 个 TCP 与 4 个
UDP worker；每个 UDP worker 轮流打 `--udp-targets`（默认 16）个互不相同的 tunnel 目标，
否则单一目标只会占用一条审计窗口缓存条目，RSS 门禁结构上观察不到该缓存。
独立累加 worker attempt/success/error，同时记录吞吐、`ssserver`/`sslocal`/auditd 的
CPU/RSS、环境、工作负载参数和二进制哈希。短冒烟参数可通过 `--help` 查看；依赖已经缓存时可加
`--offline-build`。`--plus-source` 不能使用 `prepare-source.sh` 生成的无 `.git` 导出树；脚本
必须验证锁定提交确实是 plus HEAD 的祖先，避免把不同上游误作对照。脚本不硬编码“吞吐下降/
CPU 增幅”的通过阈值，生产候选报告必须在目标
机型人工比较三种配置，并连同并发、TCP/UDP 比例、payload 大小和机器信息一起归档。

仓库内的参考测量摘要见 [`../docs/PERFORMANCE.md`](../docs/PERFORMANCE.md)。

两套 benchmark 都只使用 loopback 和运行时随机密钥，不产生公网代理流量。输出路径应放在临时
或发布证据目录，不要把含机器信息的临时报告无审查提交到公开仓库。

## 性能门禁

[`benchmark_audit.py`](benchmark_audit.py) 提供跨平台的故障场景预检，并可消费
[`benchmark_data_path.py`](benchmark_data_path.py) 生成的真实数据面报告。没有真实报告时，输出中的
`gate` 必须为 `null`，合成队列吞吐只作诊断，不能作为发布通过依据：

```bash
python3 tests/benchmark_audit.py \
  --output /tmp/shadowsocks-rust-plus-audit-preflight.json
```

在 Linux 目标机上完成 upstream、runtime-off plus 和 user-audit runtime-on plus 三组回环测量后，
同一份报告会用共同 `run_id` 绑定 auditd 进程 RSS 与真实 worker outcome；再执行真实门禁：

```bash
sudo python3 tests/benchmark_data_path.py \
  --upstream-source /tmp/shadowsocks-rust-plus-benchmark/upstream \
  --plus-source /tmp/shadowsocks-rust-plus-benchmark/plus \
  --audit-ingest-socket /run/shadowsocks-audit/ingest/ingest.sock \
  --auditd-user shadowsocks-audit \
  --producer-user shadowsocks-benchmark \
  --audit-export-socket /run/shadowsocks-audit/export/export.sock \
  --audit-export-user shadowsocks-audit-export \
  --audit-hmac-key-file /etc/shadowsocks-audit/export-hmac \
  --audit-node-id node-example-01 \
  --auditd-pid "$auditd_pid" \
  --output /tmp/shadowsocks-rust-plus-data-path.json
python3 tests/benchmark_audit.py \
  --data-path-report /tmp/shadowsocks-rust-plus-data-path.json \
  --require-linux --enforce \
  --output /tmp/shadowsocks-rust-plus-audit-gate.json
```

门禁只接受 `evidence_kind=native_user_audit_data_path` 的自包含报告：必须是 Linux、plus 构建包含
`user-audit`、三种 case 的 runtime 状态完整、三个服务 UID 各自独立、两个 socket 的 device/inode
已绑定、auditd RSS 与 enabled case 采样一致、签名 health 证明 durable ingest 增量，且代理计数明确
来自 `worker_outcomes`。性能阈值以锁定 upstream 为主基线，CPU/RSS 只比较 `ssserver`；分离的
auditd JSON 不能证明属于同一次运行，因而不被接受。`--enforce` 会在缺少任一证据或阈值失败时
退出非零；目标机实跑、Linux auditd runtime、sanitizer fuzz 和生产并发压测的证据需单独归档，
不能用合成预检替代。

## 已知上游基线问题

锁定上游的 `crates/shadowsocks/tests/tcp.rs` 依赖公网 `www.example.com`，固定基线当前会因
上游断言 HTTP/1.0、实际返回 HTTP/1.1 而失败。该问题在原始 `v1.24.0` 已可复现，本 overlay
不修改或掩盖它；本项目的集成测试使用本机 echo 服务，不依赖公网响应格式。
