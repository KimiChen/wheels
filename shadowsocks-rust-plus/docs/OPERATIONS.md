# 安装、监控与回滚

本文只描述数据面组件。生产密钥、节点清单、域名和地址由部署系统管理，不应写入本仓库。

## 构建与安装

先完成本地测试；`scripts/build.sh` 产生的是当前宿主平台开发产物，macOS 二进制不得进入 Linux
部署。Linux x86_64 生产候选必须使用专用发布构建：

```bash
./scripts/verify.sh
./scripts/build-linux-release.sh --repository /absolute/path/to/upstream-mirror
```

`build-linux-release.sh` 复用 `prepare-source.sh`，从 `upstream.lock` 固定的 `v1.24.0` 提交
`7ee1aa9223ed8f4d34734aac919036c8ad4502c2` 重新准备源码、按 `patches/series` 应用补丁，
并构建启用 `user-audit` feature 的 `x86_64-unknown-linux-musl` `ssserver` 与
`shadowsocks-auditd`。`--repository`
既可指向本地镜像也可省略后使用锁定地址，但两者都必须解析到精确上游 commit。发布构建要求
overlay 工作树完全干净，并按 [`../packaging/release-toolchain.lock`](../packaging/release-toolchain.lock)
核对 rustc commit、Cargo、cargo-zigbuild、Zig 和 Python 版本。宿主 Python 链接的 zlib 不参与
双二进制发布内容，不作为跨发行版发布门禁。

脚本把同一准备源码复制到不同绝对路径，使用两个独立 Cargo target 构建；通过
`SOURCE_DATE_EPOCH`、路径 remap、关闭 incremental/build-id 和剥离符号消除已知不稳定输入。
上游 `build-time` 宏原本读取实时时钟，overlay 已将其替换为发布脚本从 commit epoch 派生的固定
UTC 字符串；其他构建若未显式提供该值则显示 `unknown`。
四次构建（两个 binary 各两次）必须逐字节相同，才会生成固定权限和顺序的双二进制发布目录。
`release-manifest.json` 记录版本、上游 commit、overlay commit、目标、构建时间基准、两次独立
构建记录、完整工具链以及两个 ELF 的大小和 SHA-256；每个 binary 另有独立 checksum 文件。
不要复制 `.cache/` 或临时构建目录作为发布物。

构建后由离线 RSA/ECDSA 私钥产生 detached SHA-256 签名，再用独立分发的公钥验签：

```bash
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

验签会同时锁定 `upstream.lock` 版本/commit 和期望 overlay HEAD，校验 detached
签名、规范 manifest、两个 ELF 架构与 SHA-256、两个 checksum 和输出文件结构。私钥
不得进入仓库、构建目录或发布包；公钥应通过与发布包不同的可信渠道分发。`dist/`、Cargo target
和签名中间产物均已 ignore，仍不得使用 `git add -f` 提交。

推荐安装布局：

```text
/usr/local/bin/ssserver
/usr/local/bin/shadowsocks-auditd
/etc/shadowsocks-rust-plus/server.json
/etc/shadowsocks-audit/auditd.json
/etc/shadowsocks-audit/export-hmac
/run/shadowsocks-rust-plus/user-stats.sock
/run/shadowsocks-audit/ingest/ingest.sock
/run/shadowsocks-audit/export/export.sock
/var/lib/shadowsocks-audit/{open,sealed,acked,quarantine}
```

配置目录应为 `0750`、配置文件应为 `0640` 或更严。socket 路径必须是规范绝对路径，不能含
`.`、`..` 或任何符号链接祖先；每一级祖先都必须是真实目录，并由 root 或服务最终 euid
持有。直接父目录的 `mode & 0022` 必须为 0；更高层级只有 root 持有且带 sticky bit 的目录
可以让 group/other 写入。默认 socket 模式为 `0600`。只有采集器确实通过专用组访问时才使用
`0660`；组写权限只授予 socket 本身，不能通过把父目录改为 group-writable 绕过完整性检查。
某些系统的 `/var/run` 是指向 `/run` 的符号链接，因此配置应直接使用 `/run/...`；在 macOS
等系统用临时目录测试时，应先取得 `/tmp` 的规范路径再拼接 socket 路径。
Linux/Android 还要求服务进程可访问已挂载的 `/proc/self/fd`；exporter 用它只对已经通过
`O_PATH|O_NOFOLLOW` 固定并核验的 socket inode 设置权限，缺失或不可访问时会安全地启动失败。

平台覆盖必须按以下口径进入发布清单：Linux/Android 使用上述固定 fd 路径；macOS、FreeBSD、
NetBSD 及其他非 Linux Unix 构建使用 `fchmodat(..., AT_SYMLINK_NOFOLLOW)` 路径。目前 macOS 已
运行 socket mode 与符号链接替换回归，Linux musl 和 FreeBSD 已交叉编译；Android 仅验证了该
平台权限 helper 的编译，NetBSD 及其余 Unix 尚未完成本项目的运行时认证。发布到任一目标系统前必须
执行完整 `verify.sh`，并实际检查 `0600`/`0660`、替换符号链接和并发 lockfile 场景。目标 libc/
内核若不支持 no-follow chmod，exporter 应保持启动失败；禁止退回会跟随符号链接的 `chmod`。

示例见 [`../config/server.example.json`](../config/server.example.json) 与
[`../config/auditd.example.json`](../config/auditd.example.json)，服务模板见
[`../packaging/shadowsocks-rust-plus.service`](../packaging/shadowsocks-rust-plus.service) 和
[`../packaging/shadowsocks-auditd.service`](../packaging/shadowsocks-auditd.service)。
当前五节点部署固定使用 `2022-blake3-aes-128-gcm`：全集群共用一个 16 字节随机 iPSK，每个
用户使用一个独立 16 字节随机 uPSK，全部采用带标准 padding 的 Base64。不得把替换后的示例
配置或受控凭据源提交到 Git。

### auditd 安装与权限

先安装 `shadowsocks-auditd` 二进制、[`../packaging/shadowsocks-auditd.sysusers`](../packaging/shadowsocks-auditd.sysusers)
和 [`../packaging/shadowsocks-auditd.tmpfiles`](../packaging/shadowsocks-auditd.tmpfiles)，再创建
`/etc/shadowsocks-audit/auditd.json`。推荐使用发行版的 `systemd-sysusers`/`systemd-tmpfiles`，
不要手工把目录设成可写给 group/other：

```bash
install -D -m 0644 packaging/shadowsocks-auditd.sysusers \
  /usr/lib/sysusers.d/shadowsocks-audit.conf
install -D -m 0644 packaging/shadowsocks-auditd.tmpfiles \
  /usr/lib/tmpfiles.d/shadowsocks-audit.conf
systemd-sysusers /usr/lib/sysusers.d/shadowsocks-audit.conf
systemd-tmpfiles --create /usr/lib/tmpfiles.d/shadowsocks-audit.conf
install -m 0640 -o root -g shadowsocks-audit config/auditd.example.json \
  /etc/shadowsocks-audit/auditd.json
# 在启用服务前，将 node_id、socket 路径和 spool 参数替换为本节点值；示例值不可直接用于生产。
install -m 0600 -o shadowsocks-audit -g shadowsocks-audit /secure/node-export-hmac \
  /etc/shadowsocks-audit/export-hmac
install -m 0644 packaging/shadowsocks-auditd.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now shadowsocks-auditd.service
```

`export-hmac` 必须由密钥管理系统生成每节点独立的 32-byte 随机值，文件内容为 64 个小写 hex
字符，可选一个末尾 LF；命令输出、shell history、日志和仓库不得出现 key。`auditd` 使用不可
登录的 `shadowsocks-audit` 账号，只写 `/var/lib/shadowsocks-audit` 和 `/run/shadowsocks-audit`；
ingest/export socket 分别由 `shadowsocks-audit-ingest`/`shadowsocks-audit-export` 组隔离。服务
单元启用 `ProtectSystem=strict`、`ProtectHome=true`、`NoNewPrivileges=true` 和 `AF_UNIX` 限制，
不能读取 ssserver 配置或密钥。ssserver 单元对 auditd 使用 `Wants=`/`After=` 而不是 `Requires=`：
auditd 离线时数据面仍启动，producer 只保留有界 queue 并后台重连。

配置校验发生在服务启动时（未知字段、重复参数、相对路径、symlink parent、UID/mode、范围
错误都必须在创建 socket 或文件前失败）。不要以 root 直接运行 daemon；由 systemd 使用
`shadowsocks-audit` 身份启动并从 journal 检查失败原因：

```bash
systemctl start shadowsocks-auditd.service
journalctl -u shadowsocks-auditd.service -n 50 --no-pager
```

不要同时使用环境变量或备用配置路径。修改 spool 上限、segment 或导出 HMAC 后必须先停止接受
新采集、完成最终 lease/ACK 屏障，再重启 auditd；不要删除 `open/`、`acked/` 或
`tombstones.json` 来“修复”状态。

首次生成 200 个正式账号和 4 个测试账号的受控源时，先准备权限 `0700` 且已 ignore 的目录；工具只会创建显式目标，模式
固定为 `0600`，已存在时拒绝覆盖，也不会把任何密钥写到终端：

```bash
install -d -m 0700 .artifacts/credentials
./scripts/cluster-users.py generate \
  --formal-count 200 \
  --test-count 4 \
  --test-prefix test_ \
  --output .artifacts/credentials/cluster-users.json
```

从旧受控源导入时，先确保输入文件 group/other 无权限，再使用 `normalize --input ... --output ...`
写到新的目标。部署系统必须从唯一规范源把同一个 `shared_i_psk` 和按 name 排序的同一份
`users[]` 原样注入 5 个最终配置，不得由人员或五份模板分别生成。凭据源 schema v2 使用
`kind: formal|test` 区分账号，并按 formal/name、test/name 排序；写入 ssserver 配置时剥离
`kind`，最终每个用户对象仍只能包含 `name` 和 `password`。使用下列安全投影，不要用临时
`jq`/文本脚本重新实现：

```bash
./scripts/cluster-users.py render-users \
  --source .artifacts/credentials/cluster-users.json \
  --output .artifacts/credentials/ssserver-users.json
```

投影文件仍含真实 uPSK，因此同样只会以 `0600` 写入显式、未存在且已 ignore 的目标。

注入后、启动前必须校验恰好五份单服务配置；下列路径仅为私有 staging 示例：

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

该命令要求受控源和五份最终配置都不授予 group/other 任何权限，并验证 node/service ID 全集群唯一、AES-128-GCM、16 字节 Base64 iPSK/uPSK、共享 iPSK
一致，以及 200 个正式/4 个测试账号在剥离 `kind` 后的 `users[]` 名称、顺序和 uPSK 逐项一致；
错误信息和成功摘要都不包含密钥。

## 启动前检查

每次升级先在隔离临时目录完成以下检查：

1. `scripts/verify.sh` 从固定上游重放全部补丁并通过测试。
2. 配置中的 `user_stats.node_id`、每个 `servers[].id` 和 `users[].name` 均为非空、最多 128 字节的
   ASCII 可显示非空白字符；server ID 在节点内唯一，用户名和用户密码在服务内各自唯一。
3. 当前五节点的每个服务都使用 `2022-blake3-aes-128-gcm`，共享 iPSK 与每个 uPSK 都能规范
   Base64 解码为恰好 16 字节，并至少有一个 `users[]`。底层虽兼容 AES-256 EIH，但不属于本次
   五节点配置 profile；非 EIH method、空用户列表和仅主身份服务都会使配置完整性检查失败。
4. `user_stats` 中没有拼错或未支持字段，`socket_mode` 只写精确的 `"0600"` 或 `"0660"`。
5. `user_stats.node_id` 与控制面节点 ID 一致，socket 路径按上文逐级检查其规范路径、属主和权限。
   统计模式不能使用 builtin/standalone `ssmanager` 或运行时 manager `add`；要统计的服务必须全部写在
   静态 `servers[]` 中。
6. `cluster-users.py verify-five` 对最终 5 份配置通过；共享 iPSK、规范化后的 200 个正式账号与
   4 个测试账号 `users[]` 内容和顺序完全一致，5 个 node ID 和 service ID 各自唯一。监听地址、固定生产端口
   `19999`、ACL 和用户数量符合变更单，配置中没有示例占位值。新逻辑用户记录
   （`server_id + name`）数量不得超过 `max_identities`，不同逻辑 server ID 数量也不得超过相同
   数值的独立上限。
7. 部署系统和控制面必须把同一 runtime 中已经出现的 `(server_id, name)` 视为不可转让的计费
   身份；不得通过修改 route、密码或业务用户关系把它重新归给另一计费主体。改变归属必须使用
   新名称，或在最终快照屏障后启动新 runtime。当前静态数据面没有热转让入口，该约束仍须由
   生产持久化层落实并验收。
8. 以实际服务用户启动灰度实例，确认 exporter socket 及其 `.lock` 文件的类型、属主和权限。
9. 用本地测试身份分别完成一次 TCP 与 UDP 回显，核对四个计数字段的方向和增量。
10. 确认 `user_audit` 的 ingest socket 祖先目录不可被非特权账号替换，且 `SO_PEERCRED`/socket inode
    owner 校验使用配置中的 `auditd_user`；auditd 不可用时 ssserver 仍能代理但 health 标记 degraded。
11. 通过 mock collector 完成一次 lease、响应 HMAC/Body-SHA256 校验和 ACK，再验证相同 batch 重试
    幂等；不要把 event body 或 HMAC key 写入 shell 输出。
12. 以容量、最小可用空间、open tail 截断和进程重启故障注入检查 spool recovery；确认未 ACK 删除会
    生成 `spool_gap`，acked 副本仍按 86400 秒保留。

exporter 无法安全绑定时，启用了统计的 `ssserver` 会启动失败；不要通过删除 socket 检查或放宽
目录权限来绕过错误。exporter 先以 `O_NOFOLLOW` 打开模式 `0600` 的同路径 `.lock` 文件，取得
非阻塞独占 `flock` 后重验已打开 fd 与当前路径的设备号/inode，然后才探测、清理和绑定，并在整个生命
周期持有该锁。socket mode 使用不跟随符号链接的操作设置，随后重验设备号、inode 和 mode。第二个并发
实例会稳定
失败，不会删除第一个实例新绑定的 socket。活动 socket、普通文件和符号链接都不会被覆盖；
只有无法连接且 inode 未变化的旧 socket 才会被清理。

exporter task 与 TCP/UDP relay 一起受监督。它在启动后任意异常退出、panic 或连续 3 次
`accept()` 失败都会使 `ssserver`
失败退出；仓库 systemd 模板的 `Restart=on-failure` 会在 `RestartSec=3s` 后重启整个服务。
这会产生新的 `runtime_id`，因此必须监控重启循环并按异常运行周期处理结算。只有单个 exporter
客户端的协议错误、超时、超限或断开会被隔离，不会终止数据面。

## 采集与健康检查

先直接通过本机 Unix socket 检查 exporter 健康状态：

```bash
curl --fail-with-body --silent --show-error \
  --unix-socket /run/shadowsocks-rust-plus/user-stats.sock \
  http://localhost/healthz
```

健康时返回 HTTP 200 与 `{"schema_version":1,"status":"ok"}`；计数器或序号饱和时返回
HTTP 503 与 `{"schema_version":1,"status":"unhealthy"}`。健康检查不生成快照，也不推进
`sequence`。

使用标准 curl 读取一次严格快照：

```bash
curl --fail-with-body --silent --show-error \
  --unix-socket /run/shadowsocks-rust-plus/user-stats.sock \
  http://localhost/v1/snapshot
```

需要同时校验 schema、完整性和健康字段时，使用仓库客户端：

```bash
./scripts/user-stats-client.py \
  /run/shadowsocks-rust-plus/user-stats.sock \
  --require-healthy --compact
```

`--timeout` 必须是有限正数，并作为连接、发送、响应头和完整响应体读取共享的整体 deadline；即使
对端持续慢速滴灌字节，也不会逐次重置超时。

exporter 每条 Unix stream 连接只处理一个 HTTP/1.1 请求并主动关闭连接。采集请求必须是带唯一
合法 `Host` authority 的 origin-form `GET /v1/snapshot`；禁止 query、`Content-Length`、
`Transfer-Encoding`、HTTP/1.0 和 absolute-form。服务端不保持连接，也不会处理第二个请求；
客户端不得发送 body 或 pipeline。仓库客户端与上述 curl 已满足契约；自行实现的采集器必须
遵守同一规则。HTTP 状态码、固定错误 body 和限制详见 [`API.md`](API.md)。

采集器必须验证 `schema_version`、`runtime_id`、`sequence`、两个 health 标志和响应完整性后再入账。
同一 `runtime_id` 中拒绝变化的 `started_at_unix_ms`、重复或倒退的序号、倒退的累计值、未知
`identity_kind`、字段缺失，以及已观察服务/身份 lineage 的无故消失。新 `runtime_id` 的首个快照
按控制面预先选定的 `baseline` 或 `include` 策略处理，具体契约见 [`API.md`](API.md) 与
[`../tests/settlement_model.py`](../tests/settlement_model.py)。

每项累计值的持久化基线键必须完整包含
`node_id + server_id + server_generation + identity_name + identity_generation + runtime_id`；
幂等批次 ID 还应包含快照序号和本次增量。当前 exporter 在同一 runtime 内对同名服务/用户复用
`generation=1` 和原累计计数器；删除只切换 `active`，已观察的 lineage 不得从后续快照消失。这两个
generation 仍是 v1 完整键的强制维度，不得因当前实现固定为 1 而省略。同一 runtime 内也不得把稳定用户名
重分配给不同计费用户；改变归属应使用新名称，或执行重启屏障并进入新 `runtime_id`。

建议告警：

- 连续两个采集周期连接失败、超时、解析失败或收到 exporter 错误；
- `health.counter_overflow` 或 `health.sequence_overflow` 为 `true`；
- 运行中的服务突然出现新 `runtime_id`；
- 同一运行周期内序号或任一累计值不单调；
- 快照接近 `max_response_bytes`，逻辑用户记录数接近 `max_identities`，或逻辑 server ID 数接近相同数值的
  独立上限；
- 用户累计总和与服务级/主机级趋势出现无法由协议开销解释的持续偏差。

主机网卡字节包含加密、协议头、重传和隧道开销，不能与本接口的应用负载字节要求完全相等。

### 远程访问边界

`ssserver` exporter 永远只监听本机 Unix socket，不增加 TCP 或公网监听。控制面确需从节点外
发起 HTTP 请求时，应部署独立 Nginx、Caddy 或同类反向代理，以该 socket 作为 HTTP upstream；
公网一侧至少启用 HTTPS、mTLS、采集网络来源限制、速率限制和不含快照正文的审计日志。代理
必须保持 `Cache-Control: no-store`，不得缓存快照，也不得暴露正向代理、manager API 或其他
`ssserver` 端口。其 upstream 请求必须保持 HTTP/1.1 origin-form、单个合法 Host，且不能自动
注入 `Content-Length: 0` 或 `Transfer-Encoding`；上线前应分别通过代理验证 snapshot 的 200、
健康检查的 200/503 和未知路由的拒绝状态。

下面是最小 Nginx upstream 片段；它只展示 exporter 相关指令，外层 `server` 仍必须配置 TLS、
mTLS、采集网段 allowlist、速率限制和不记录响应 body 的审计。显式写 `proxy_http_version 1.1`
是为了兼容 Nginx 1.29.7 之前默认使用 HTTP/1.0 的版本；Unix socket URL 与该版本差异见
[Nginx proxy module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)。

```nginx
location ~ ^/(?:v1/snapshot|healthz)$ {
    if ($http_content_length != "") { return 400; }
    if ($http_transfer_encoding != "") { return 400; }

    proxy_pass http://unix:/run/shadowsocks-rust-plus/user-stats.sock:;
    proxy_http_version 1.1;

    proxy_set_header Host localhost;
    proxy_set_header Connection close;
    proxy_set_header Content-Length "";
    proxy_set_header Transfer-Encoding "";
    proxy_set_header Expect "";
    proxy_set_header Upgrade "";
    proxy_pass_request_body off;

    proxy_cache off;
    proxy_no_cache 1;
    proxy_cache_bypass 1;
    proxy_next_upstream off;
    proxy_intercept_errors off;
}
```

两个 `if ... return` 只做安全的提前终止，使声明了 body 的外部请求在代理边界即被拒绝；随后清空
framing headers 和关闭 upstream body 转发仍作为纵深防护。该 location 之外应默认拒绝；不能设置
`proxy_method GET`，否则外部 POST 会被静默改写为可推进 `sequence` 的 snapshot GET。配置完成后
先执行 `nginx -t`，再用实际 mTLS 客户端验证两个精确路由、query/非 GET/body 拒绝以及上游不可用
时不会重试。

`GET /v1/snapshot` 会推进 `sequence`，因此反向代理应禁用该路由的自动 upstream retry；重试
会产生可审计但不可据此推导流量的序号缺口。代理到 exporter 的并发也应限制在
`max_concurrent_clients` 以内，避免请求突发长期返回 429。`/healthz` 不推进序号，可以独立用于
高频存活检查。

若快照连接达到 `write_timeout_ms`，已启动的阻塞序列化不会被强制取消，并会继续占用原连接的
并发名额直到工作和响应缓冲释放；这段时间的 429 表示资源仍真实繁忙。持续出现该现象时应检查
CPU 饥饿、身份数量、响应大小和采集频率，而不是降低超时或直接扩大并发上限。
繁忙响应在最多 32 个独立 worker 中写入，每个最长 100ms；该上限也满时新连接会被直接关闭。
采集器应把无响应关闭和 429 都当作可重试的繁忙错误，不得入账。

反向代理 worker 应使用不可登录的独立用户，通过专用组和 `socket_mode: "0660"` 获得最小连接
权限；socket 父目录仍保持 `0750` 且禁止组写。不能为了让代理连接而改成 `0666`、放宽父目录，
或让代理以 root 身份常驻。

反向代理是独立的控制面组件，不应与 `ssserver` 数据面监督树绑定。代理或 TLS 配置失败时，
本机 exporter 仍按原故障模型运行；远程采集器则必须告警并依靠持久化基线、outbox 和幂等重试
恢复。任何将 Unix socket 直接转成无认证 TCP 的字节转发都不构成安全的远程访问方案。

## 审计采集与健康检查

审计采集器只连接 auditd 的 export UDS，不连接 ingest UDS。每次请求都用节点独立 HMAC 签名，
并在解析 body 前验证 response digest、response MAC、node ID 和 request nonce。请求必须严格使用
HTTP/1.1 origin-form：`POST /v1/audit/lease` 的 body 固定为
`{"schema_version":1}`，`POST /v1/audit/ack` 的字段顺序固定为
`schema_version,batch_id,body_sha256`；每连接只发一个请求并关闭。

一个最小采集循环如下，实际实现应使用安全的 HTTP/UDS 库或仓库 mock collector，不要自行拼接
未校验的字符串：

```text
GET /v1/audit/healthz
  -> 验证签名的完整 health；503/degraded 只告警，不入账
POST /v1/audit/lease
  -> 先验证 raw NDJSON/body digest，再校验 wrapper epoch/sequence/event digest
写入 controller durable outbox（按 node_id,event_id 幂等）
POST /v1/audit/ack
  -> 仅在 durable commit 后发送；超时重试同一 batch_id/body_sha256
```

采集器必须保留最后处理的 `(spool_epoch, spool_sequence)`、每个 event ID 和 batch ID 的幂等状态；
lease/ACK 超时、连接断开、未知 NACK 或 response MAC 失败都只能重试，不能把未确认数据标为丢失。
`producer_gap`、`udp_window_contention` 和 `spool_gap` 是诊断记录，不得并入用户访问数；收到 gap
应告警并保存原始诊断。auditd health 中 `status=degraded` 的触发包括 producer 断开超过 5 秒、
存储不可写、recovery/quarantine 未处置、未 ACK gap 或计数饱和；累计计数不因恢复而清零。

导出响应的 `X-Shadowsocks-Audit-Body-SHA256` 必须等于实际 raw NDJSON body 和
`X-Shadowsocks-Audit-Response-SHA256`。collector 应保留 event 的原始 JSON bytes，按
`event_payload_sha256` 复算，不得 parse 后重新序列化再 ACK。仓库提供的
[`../tests/mock_collector.py`](../tests/mock_collector.py) 只用于协议互通和故障测试，不是生产
controller；生产 controller 仍需实现 durable commit、跨节点隔离、retention 和管理员审计。

### 审计 export 的 HTTP 中介

远程 collector 不能直接获得 export UDS。需要 HTTP 中介时，必须运行独立的反向代理实例，并让其
worker 使用 auditd 配置中 `export_peer_user` 对应的专用 UID（模板默认 `audit-exporter`）。该账号
只能加入 `shadowsocks-audit-export` 组；不得加入 ingest 组，也不得复用 ssserver、auditd 或普通
Web worker 账号。`systemd-sysusers` 后应同时验证：worker 的实际 UID 与 `export_peer_user` 解析结果
一致、它能遍历 `0750 shadowsocks-audit:shadowsocks-audit-export` 的父目录并连接
`0660 shadowsocks-audit:shadowsocks-audit-export` 的 socket。auditd 的 `SO_PEERCRED` 看到的是中介
worker，不是远端 collector；仅把某个账号加入 export 组而不匹配配置 UID 仍会被拒绝。

公网侧必须使用 HTTPS、mTLS、collector 来源 allowlist、限速和不记录请求/响应正文的审计日志。
中介只能转发三个精确的 method/path 组合，不得重写 method/path、规范 JSON body、
`Authorization` 或 `X-Shadowsocks-Audit-*` 请求头，不得解压/压缩正文、缓存响应、自动重试 upstream
或把 204 改写为带 body 的响应。下面片段放在专用 Nginx `http`/`server` 上下文；TLS 和 allowlist
仍需由部署系统补齐：

```nginx
map "$request_method:$uri" $audit_route_allowed {
    default                         0;
    "GET:/v1/audit/healthz"        1;
    "POST:/v1/audit/lease"        1;
    "POST:/v1/audit/ack"          1;
}

location ~ ^/v1/audit/(?:healthz|lease|ack)$ {
    if ($audit_route_allowed = 0) { return 405; }
    if ($is_args != "") { return 400; }
    if ($http_transfer_encoding != "") { return 400; }
    if ($http_content_encoding != "") { return 400; }

    proxy_pass http://unix:/run/shadowsocks-audit/export/export.sock:;
    proxy_http_version 1.1;
    proxy_set_header Host localhost;
    proxy_set_header Connection close;
    proxy_set_header Transfer-Encoding "";
    proxy_set_header Content-Encoding "";
    proxy_set_header Expect "";
    proxy_set_header Upgrade "";
    proxy_set_header Content-Length $content_length;
    proxy_set_header Authorization $http_authorization;
    proxy_set_header X-Shadowsocks-Audit-Node $http_x_shadowsocks_audit_node;
    proxy_set_header X-Shadowsocks-Audit-Timestamp $http_x_shadowsocks_audit_timestamp;
    proxy_set_header X-Shadowsocks-Audit-Nonce $http_x_shadowsocks_audit_nonce;
    proxy_set_header X-Shadowsocks-Audit-Content-SHA256 $http_x_shadowsocks_audit_content_sha256;
    proxy_pass_request_body on;
    proxy_request_buffering on;
    proxy_buffering off;
    proxy_cache off;
    proxy_next_upstream off;
    proxy_intercept_errors off;
}
```

该正则 location 之外必须默认拒绝。上线前使用真实 mTLS collector 逐一验证三条合法路由、错误
method/query/body framing 的拒绝、200/204/503 的逐字 body 与所有 response HMAC headers，以及
upstream 断开时不会自动重试 lease/ACK。任何会重新序列化 ACK JSON 或 lease body 的 API gateway
都不兼容本协议。

建议额外告警：

- auditd 进程退出、ingest/export socket 不存在、连续 5 秒无 producer 或 health 503；
- `storage_rejected_attempts`、`evicted_unacked_records`、任一 gap 或 `udp_window_contention` 增长；
- lease digest/MAC、epoch/sequence 连续性或 ACK 幂等校验失败；
- spool bytes 接近 5 GiB、文件系统可用空间低于 1 GiB、acked retention 超期未清理；
- ssserver `shutdown_skipped_observations`、`sequence_exhausted` 或 producer health counter 饱和。

## 计划重启与升级屏障

计划停止、升级或密钥轮换按以下顺序执行：

1. 停止把新连接调度到节点。
2. 等待现有 TCP 连接和 UDP 关联排空到约定上限。
3. 让 ssserver 停止接受新数据、关闭 `AuditEmitter`，在 2 秒 drain 窗口内排空 queue/in-flight；
   join 全部 relay 后记录最终 `shutdown_skipped_observations` health counter。
4. 从 auditd 拉取健康的最终 lease，先 durable commit，再发送 ACK；同时按需拉取一次 user-stats
   最终快照，使用唯一 batch ID 入账。
5. 等待 controller 确认审计 batch 和统计快照都已持久化。
6. 停止 `shadowsocks-rust-plus` 与 `shadowsocks-auditd`，安装并验签新双二进制/配置，再先启动
   auditd、后启动 ssserver。
7. 确认出现新的 `runtime_id`，审计 `spool_epoch`/producer hello 正常，计数从零开始且测试身份
   TCP/UDP 四向流量正确。
8. 恢复调度。

异常退出无法执行最终屏障。采集器应保留最后成功快照，标记旧 runtime 的未闭合窗口并留下审计
记录，不能把新 runtime 的累计值直接与旧 runtime 相减。

## 禁用与回滚

快速禁用统计和审计但保留 plus 二进制：

1. 按计划重启屏障取得最终快照。
2. 从配置删除顶层 `user_audit` 与 `user_stats`。
3. 停止并禁用 `shadowsocks-auditd.service`，确认 auditd export/ingest socket 不再创建。
4. 重启并确认 ssserver 不再创建 user-stats exporter socket，manager 行为恢复为上游默认。

运行时未配置 `user_stats` 时不会创建 registry/exporter；TCP/UDP 使用上游路径，EIH 线协议不变，
manager 命令也恢复为上游行为。启用 `user_stats` 的配置本身不能以 manager 模式运行。

仅回滚审计功能、保留用户统计：

1. 完成上面的最终审计 lease/ACK 屏障并停止 ssserver。
2. 从配置删除顶层 `user_audit`，保持 `user_stats` 配置，停止并禁用 auditd。
3. 启动同一 plus 版本的 `ssserver`，确认统计 exporter 仍健康，且不存在 ingest socket。

回滚到锁定的原始上游二进制：

1. 完成最终审计 lease/ACK 与统计快照屏障并停止两个服务。
2. 切换到由同一 `upstream.lock` 提交构建的已校验上游 `ssserver`，不安装/启动 auditd。
3. 删除顶层 `user_audit` 和 `user_stats` 配置；`servers[].id` 可以保留。
4. 启动后完成 TCP、UDP、manager 与 ACL 冒烟测试。
5. 把 exporter/auditd 停止视为预期维护窗口，防止监控误报为静默故障。

服务正常退出时会按 bind 后记录的设备号和 inode 清理自己的 socket。若进程被强制终止而留下
旧 socket，下次启动会在取得 `.lock` 独占锁、确认 socket 不可连接且路径未被替换后安全清理。
lockfile 可跨重启保留，由 exporter 重新打开并锁定；不要在服务仍运行时手工删除 socket 或
lockfile。

## 灰度和发布记录

单节点灰度、分批上线和上游提案属于生产或外部发布变更，必须在实际环境取得明确授权后执行。
每批至少归档：已验签的 `release-manifest.json`、detached 签名、两个 binary 的 SHA-256、签名
公钥标识、节点范围、配置版本、启动时刻、ssserver runtime ID、auditd spool epoch、producer
hello 时间、首末 user-stats 快照序号、首末审计 spool sequence、TCP/UDP 测试增量、gap/health
计数、性能观测、回滚演练结果和审批人。manifest 已记录构建版本、上游/overlay commit、工具链及
两个 ELF 的 SHA-256；不得人工另抄一份可能漂移的来源字段。本仓库只提供模板和可复现验证，不包含
真实节点记录。
