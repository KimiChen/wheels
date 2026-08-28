# 安装模板

1. 使用 `scripts/build-linux-release.sh` 完成两次独立的固定工具链
   `x86_64-unknown-linux-musl` 构建，生成 `ssserver` 和 `shadowsocks-auditd` 两个 ELF、各自
   SHA-256、规范 `release-manifest.json` 和 detached-signature 输入。
2. 使用离线私钥运行 `scripts/sign-release.sh`，再用独立分发的公钥运行
   `scripts/verify-release.sh`；只有 detached 签名、版本/commit、ELF 架构、规范 release manifest、
   两个二进制及 SHA-256 全部通过后，才从发布目录取出 `ssserver` 和 `shadowsocks-auditd`，分别安装为
   `/usr/local/bin/ssserver` 与 `/usr/local/bin/shadowsocks-auditd`。`scripts/build.sh` 的宿主平台开发产物不得用于 Linux 部署。
3. 将 `packaging/shadowsocks-auditd.sysusers` 安装到 `/usr/lib/sysusers.d/`，将
   `packaging/shadowsocks-auditd.tmpfiles` 安装到 `/usr/lib/tmpfiles.d/`，执行
   `systemd-sysusers`/`systemd-tmpfiles --create`，创建不可登录的 `shadowsocks`、
   `shadowsocks-audit`、`audit-exporter` 用户及专用 ingest/export 组，然后安装
   `packaging/shadowsocks-auditd.service`。这样 `/run` 重建后两个 socket 父目录仍会自动恢复。
4. 从 `scripts/cluster-users.py` 生成并规范化的唯一私有源注入配置；五节点 profile 固定
   `2022-blake3-aes-128-gcm`，共享 iPSK 与每用户 uPSK 都是 16 字节随机值的标准 Base64。
   受控源用 `kind: formal|test` 区分默认 200 个正式账号和 4 个测试账号；注入配置时剥离
   `kind`。对最终五份配置执行 `verify-five --expected-formal-users 200
   --expected-test-users 4` 并通过后，把实际配置写入
   `/etc/shadowsocks-rust-plus/server.json`，目录 `0750`、文件 `0640`。受控源和实际配置不得
   加入 Git，密钥只存在于部署密钥系统及批准的私有文件中。
5. 安装 `shadowsocks-rust-plus.service` 并执行 `systemctl daemon-reload`；只有取得明确的
   生产变更授权后，才可在批准范围内的非关键节点启动。

`shadowsocks-auditd.service` 仅允许 `AF_UNIX`，以 `ProtectSystem=strict`、`ProtectHome=true`、
`NoNewPrivileges=true` 运行；它只写 `/var/lib/shadowsocks-audit` 与 `/run/shadowsocks-audit`，
不能读取 ssserver 配置或密钥。ssserver 对 auditd 使用 `Wants`/`After`，不是 `Requires`，因此
auditd 离线时数据面仍应启动并继续代理。生产配置样例见 [`../config/auditd.example.json`](../config/auditd.example.json)。

模板使用 systemd `RuntimeDirectory` 创建模式 `0750` 的可信 socket 父目录，并限制服务只写
运行目录。socket 路径的每一级祖先都必须是真实目录、不得经过符号链接，并由 root 或最终
服务 euid 持有；直接父目录不能给 group/other 写权限。若使用
`socket_mode: "0660"` 给采集器读取，应让采集器加入 socket 所属的专用组，并验证目录的组执行
权限和 socket 的实际属主、组、模式；不要把父目录改成 group-writable。

exporter task 与 relay 一起受监督，任意异常退出都会使 `ssserver` 失败退出。模板的
`Restart=on-failure` 会重启整个进程；只有单个 exporter 客户端请求错误在进程内隔离。部署时
应监控重启循环和新 `runtime_id`，具体见
[`../docs/OPERATIONS.md`](../docs/OPERATIONS.md)。

exporter 在上述 socket 上提供严格 HTTP/1.1，只接受 `GET /v1/snapshot` 和 `GET /healthz`。
本机可直接检查：

```bash
curl --unix-socket /run/shadowsocks-rust-plus/user-stats.sock \
  http://localhost/healthz
```

不得为 `ssserver` 增加 TCP 或公网 exporter 监听。远程采集必须部署独立反向代理，并在公网一侧
配置 HTTPS、mTLS、来源限制和禁用缓存；反向代理不属于本 systemd 模板的监督范围。

auditd export 的远程 HTTP 中介必须使用 `audit-exporter` 专用 worker UID，并仅通过
`shadowsocks-audit-export` 组连接 export socket；auditd 会用 `SO_PEERCRED` 要求该 UID 与
`export_peer_user` 精确匹配。它还必须原样转发 HMAC 请求头、POST body 与响应签名，不能照抄上面的
user-stats GET-only 配方。精确路由和 Nginx 示例见 [`../docs/OPERATIONS.md`](../docs/OPERATIONS.md#审计-export-的-http-中介)。

停止或升级前不能直接依赖 systemd 的终止信号完成结算。控制面应先停止新接入并排空，
调用 `scripts/user-stats-client.py --require-healthy` 拉取最终快照，等待存储端确认批次，
再执行 `systemctl stop/restart`。
