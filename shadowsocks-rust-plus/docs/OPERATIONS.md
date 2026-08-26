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
并只构建启用 `user-stats` feature 的 `x86_64-unknown-linux-musl` `ssserver`。`--repository`
既可指向本地镜像也可省略后使用锁定地址，但两者都必须解析到精确上游 commit。发布构建要求
overlay 工作树完全干净，并按 [`../packaging/release-toolchain.lock`](../packaging/release-toolchain.lock)
核对 rustc commit、Cargo、cargo-zigbuild、Zig、Python 和 zlib 版本。

脚本把同一准备源码复制到不同绝对路径，使用两个独立 Cargo target 构建；通过
`SOURCE_DATE_EPOCH`、路径 remap、关闭 incremental/build-id 和剥离符号消除已知不稳定输入。
上游 `build-time` 宏原本读取实时时钟，overlay 已将其替换为发布脚本从 commit epoch 派生的固定
UTC 字符串；其他构建若未显式提供该值则显示 `unknown`。
只有两个 ELF64 x86_64 二进制逐字节相同才会生成固定 gzip/tar mtime、属主、权限和成员顺序的
发布包。外部规范 manifest 和包内 manifest 完全相同，包含版本、上游 commit、overlay commit、
目标、构建时间基准、两次独立构建记录、完整工具链、二进制大小和 SHA-256；另有归档 SHA-256
文件。不要复制 `.cache/` 或临时构建目录作为发布物。

构建后由离线 RSA/ECDSA 私钥产生 detached SHA-256 签名，再用独立分发的公钥验签：

```bash
release_stem=dist/shadowsocks-rust-plus-v1.24.0-x86_64-unknown-linux-musl
./scripts/sign-release.sh \
  --archive "$release_stem.tar.gz" \
  --manifest "$release_stem.manifest.json" \
  --checksum "$release_stem.tar.gz.sha256" \
  --private-key /secure/offline/release-private.pem \
  --output "$release_stem.manifest.json.sig"
./scripts/verify-release.sh \
  --archive "$release_stem.tar.gz" \
  --manifest "$release_stem.manifest.json" \
  --checksum "$release_stem.tar.gz.sha256" \
  --signature "$release_stem.manifest.json.sig" \
  --public-key /secure/release-public.pem
```

验签会同时锁定 `upstream.lock` 版本/commit 和期望 overlay HEAD，校验 detached 签名、归档
SHA-256、规范 manifest、包内外 manifest、ELF 架构、二进制 SHA-256 及确定性归档元数据。私钥
不得进入仓库、构建目录或发布包；公钥应通过与发布包不同的可信渠道分发。`dist/`、Cargo target
和签名中间产物均已 ignore，仍不得使用 `git add -f` 提交。

推荐安装布局：

```text
/usr/local/bin/ssserver
/etc/shadowsocks-rust-plus/server.json
/run/shadowsocks-rust-plus/user-stats.sock
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

示例见 [`../config/server.example.json`](../config/server.example.json)，服务模板见
[`../packaging/shadowsocks-rust-plus.service`](../packaging/shadowsocks-rust-plus.service)。
当前五节点部署固定使用 `2022-blake3-aes-128-gcm`：全集群共用一个 16 字节随机 iPSK，每个
用户使用一个独立 16 字节随机 uPSK，全部采用带标准 padding 的 Base64。不得把替换后的示例
配置或受控凭据源提交到 Git。

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

## 计划重启与升级屏障

计划停止、升级或密钥轮换按以下顺序执行：

1. 停止把新连接调度到节点。
2. 等待现有 TCP 连接和 UDP 关联排空到约定上限。
3. 拉取健康的最终快照，使用唯一批次 ID 入账。
4. 等待存储端确认该批次已经持久化。
5. 停止服务，安装并校验新二进制/配置，再启动服务。
6. 确认出现新的 `runtime_id`、计数从零开始且测试身份四向流量正确。
7. 恢复调度。

异常退出无法执行最终屏障。采集器应保留最后成功快照，标记旧 runtime 的未闭合窗口并留下审计
记录，不能把新 runtime 的累计值直接与旧 runtime 相减。

## 禁用与回滚

快速禁用统计但保留 plus 二进制：

1. 按计划重启屏障取得最终快照。
2. 从配置删除顶层 `user_stats`。
3. 重启并确认不再创建 exporter socket。

运行时未配置 `user_stats` 时不会创建 registry/exporter；TCP/UDP 使用上游路径，EIH 线协议不变，
manager 命令也恢复为上游行为。启用 `user_stats` 的配置本身不能以 manager 模式运行。

回滚到锁定的原始上游二进制：

1. 完成最终快照屏障并停止服务。
2. 切换到由同一 `upstream.lock` 提交构建的已校验上游 `ssserver`。
3. 删除顶层 `user_stats` 配置；`servers[].id` 可以保留。
4. 启动后完成 TCP、UDP、manager 与 ACL 冒烟测试。
5. 把 exporter 停止视为预期维护窗口，防止监控误报为静默故障。

服务正常退出时会按 bind 后记录的设备号和 inode 清理自己的 socket。若进程被强制终止而留下
旧 socket，下次启动会在取得 `.lock` 独占锁、确认 socket 不可连接且路径未被替换后安全清理。
lockfile 可跨重启保留，由 exporter 重新打开并锁定；不要在服务仍运行时手工删除 socket 或
lockfile。

## 灰度和发布记录

单节点灰度、分批上线和上游提案属于生产或外部发布变更，必须在实际环境取得明确授权后执行。
每批至少归档：已验签的规范 manifest、detached 签名、发布包 SHA-256、签名公钥标识、节点范围、
配置版本、启动时刻、runtime ID、首末快照序号、TCP/UDP 测试增量、性能观测、回滚演练结果和
审批人。manifest 已记录构建版本、上游/overlay commit、工具链及二进制 SHA-256；不得人工另抄
一份可能漂移的来源字段。本仓库只提供模板和可复现验证，不包含真实节点记录。
