# 测试与验收

测试围绕当前协议和数据结构组织，不按开发阶段保留重复入口。临时配置使用回环地址、
独立随机凭据和私有目录；测试不读取生产凭据、不依赖真实 GitHub 账号。
运行日志和机器采样只保存在 ignored 的 `.cache/`、`data/`。

## 自动测试

在子项目根目录执行，要求 Go 1.26.6+、Python 3.11+、Node.js/npm：

```sh
python3 scripts/frp.py test
python3 -m unittest discover -s tests -p '*test*.py'
node --test web/tests/*.test.mjs
python3 scripts/frp.py build --native
```

构建脚本准备固定上游和 Overlay，运行 FRP 配置、消息、工具、metrics、auth、proto、
transport、插件、原生 frpc 子命令、完整 `client/...`（含 Proxy/Visitor）、`server/...`
及所有监控扩展测试。采集的 17 组脱敏向量保存在
[fixtures/collect](fixtures/collect/README.md)，入口自动设置其路径。
当前候选使用 `patches/series` 的完整 12 条补丁构建；下文单独提及补丁编号是能力来源，
不表示只应用该补丁。控制库当前为 schema12，升级与恢复验证覆盖受支持的旧库范围。
对并发/生命周期改动，在准备目录运行相关包的 `go test -race`；扩展包同时运行 `go vet`。
`nginx_test.py` 另需本机 nginx 与 openssl；缺少时明确跳过。它在临时回环端口启动隔离反代，
检查随包模板的来源限流、SSE 配额、OAuth 回调和 WSS 握手，不接触现有服务。

| 范围 | 关键验证 |
|---|---|
| 采集与会话 | 质量标记、大整数、boot/接口变化、TLS、hello、序号/连接所有权、心跳与新鲜度分离 |
| SQLite | 自增 ID、可信绑定与凭据唯一性、探测删除事务、配置修订、1024 节点上限和私有文件 |
| 分组 | 多对多成员、名称唯一/修订冲突、删除关联清理、v4 原位升级保留数据、公开隐私边界与组合筛选 |
| 流量 | 整周期 Max、手工校准、切换类型保留已用、主控日历/DST、月末和手动重置 |
| 恢复与取消 | 原子账本/基线、回滚不重计、写结果不确定时撤销缓存、重启不恢复在线 |
| TSDB | 可选开关、质量/缺口、聚合、真实存储重启与保留期、背压、低内存启动、并发关闭 |
| OAuth | GitHub state/PKCE、允许列表、会话期限、安全 Cookie、同源/CSRF、注销及未配置状态 |
| Web | 数字 ID、四种流量、表单/校准、公开字段裁剪、取消迟到响应、大整数和布局 |
| 工具 | 配置渲染、私有文件、备份恢复/路径重写、补丁/发布包边界、许可证与校验和 |

SQLite 使用真实临时数据库，故障触发器验证事务回滚；TSDB 测试使用真实内嵌
VictoriaMetrics。GitHub 通过固定生产端点的受控 HTTP 客户端替身测试，不增加测试登录接口。

## 真实二进制

下例适用于 macOS arm64，其他主机使用 `dist/<GOOS>-<GOARCH>/` 的本机二进制：

```sh
python3 tests/smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server
python3 tests/monitor_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server
python3 tests/monitor_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server --history
python3 tests/frp_detail_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server
```

`smoke.py` 验证原生 TOML、Dashboard 认证及嵌入资源、无 Proxy 登录、wire v1/v2
二进制 TCP echo、服务端重启后同一 agent 重连。该文件同时提供有限的进程、网络和
公开 API 测试工具，避免测试入口相互依赖。

`monitor_smoke.py` 验证当前 SQLite、数字节点、公开 JSON/SSE、四种流量边界、历史目录
开关和重启恢复；并覆盖 FRP 与监控连接的故障隔离、节点凭据变更以及真实探测任务生命周期。
运行时业务变更使用临时库与主控刷新，不绕过或替代 Go 测试中的管理端认证。

历史存储要求所在文件系统至少保留 1 GiB 可用空间。Linux 的 `/tmp` 若是较小的
tmpfs，`--history` 会正确返回存储降级，不能据此认定二进制不兼容。此时先在磁盘上
创建由测试用户拥有的 0700 私有临时目录，通过 `TMPDIR` 指定给测试进程；
仍保留存储空间保护，不修改线上历史目录或降低测试断言。

`frp_detail_smoke.py` 对 wire v1/v2 使用真实增强二进制和透明回环 WebSocket 中继，
观察实际 `frp.detail` 帧：file/include/Store 来源与覆盖顺序、动态 TCP 请求和真实分配端口、
HTTP 注册入口与实际转发、原生 reload 后配置修订/读取时间，以及 STCP Visitor 的
未连接、peer 失败、实际连接、恢复时间和连接关闭。SUDP 验证真实 datagram 转发及远端
关闭后本地仍监听；XTCP 使用本机 STUN 占位地址及不存在的目标，验证确定失败后的
STCP fallback 实际使用与释放。本机场景不证明跨公网 NAT 穿透成功。
验证期间主机报告保持同一会话及递增序号，
公开 JSON/SSE 不包含私有详情，未配置 OAuth 时详情入口返回 404。配置和帧只存在于临时目录或内存，
不输出原始子进程日志。该脚本仅捕获二进制适配器实际发出的私有帧；管理员会话、OAuth、
详情 API 授权和陈旧状态另由 Go 测试验收，不能将此组合描述为真实 GitHub 登录端到端测试。
本地中继显式开启 `allowInsecureLoopback`，只连接动态回环端口；FRP 控制连接仍启用 TLS。
帧捕获自身的分片、容量、序号和隐私检测由 `frp_detail_smoke_test.py` 验证。

所有子进程使用隔离环境；TLS 场景校验名称和 CA，超时强制结束会判失败。仅本机 Linux
会产生完整内核指标，Darwin 合法返回 unsupported。退出后清理测试进程与临时文件。

## 控制传输、TLS 与 OIDC

```sh
python3 tests/frp_transport_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server
python3 tests/frp_oidc_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server
```

传输脚本在 wire v1/v2 上分别验证 TCP/KCP/QUIC/WS/WSS，每例建立四条 TCP 业务连接，
逐字节比较约 64 KiB 的加密/压缩载荷；另测关闭复用、双向 TLS、不信任 CA、名称不匹配、
Token 错误和缺少客户端证书。TLS 拒绝例关闭复用以保留明确证书校验错误；不会将“连接关闭”
或无关进程退出当作通过。已登记代理后的载荷超时、截断或改变直接失败，不自动重试。

锁定原生 frps 只接收 WS。WSS 按上游测试拓扑，由第二个原生 frpc 的 `https2http` 插件
终止 TLS，再经仅回环 WS 到 frps；这一场景的 frps `transport.tls.force=false`，其余场景
均为 `true`。客户端验证终止端证书；这不是 frps 直接支持 WSS 或公网明文跳转的证明。

OIDC 脚本启动仅回环 HTTP 的一次性身份服务，真实执行 discovery、client credentials、
JWKS 和 RS256 签名验证。两种 wire 均检查正常业务，以及 audience、issuer、过期、签名、
客户端凭据错误的明确拒绝；FRP 控制连接仍校验证书并使用 TLS。身份服务不是生产 IdP，
该测试不覆盖外部 IdP 可用性、复杂 NAT 或长时间稳定性。脚本首先记录二进制 SHA256，
不读取生产账号，临时秘密保持私有并在退出时清理。

锁定上游的分组与限速用例可直接使用增强版产物运行：

```sh
go -C .cache/upstream/worktree test -count=1 -timeout=5m ./test/e2e -args \
  -frpc-path="$PWD/dist/darwin-arm64/frp-plus-agent" \
  -frps-path="$PWD/dist/darwin-arm64/frp-plus-server" \
  '-ginkgo.focus=\[Feature: (Group|Bandwidth Limit)\]' -ginkgo.no-color
```

共 12 项覆盖 TOML/旧 INI 下的负载均衡、健康检查摘除/恢复及客户端/服务端限速；
包含服务端 NewProxy 插件。2026-10-03 使用 G2 九补丁实际产物验收：首次 11 项通过，
一项 fixture 初始化遇本机其他应用端口占用，定向运行客户端限速两项通过。
该结果是功能回归，不作为容量或长期稳定性承诺。

## 配置管理的真实联合验收

`monitor/config_native_test.go` 用真实 Admin HTTP、SQLite、管理 WebSocket 与原生
frps/frpc 验证校验、秘密引用、预览、应用回复丢失后的只查询恢复、实际 TCP/STCP Visitor
转发及逐字节回退。只有外部 GitHub 身份提供者由测试替身响应，不需要真实账号。

可选浏览器用例进一步加载同一个实际 Handler，所有配置写入经正常表单、预览和确认按钮。
它不拦截或模拟 Admin API：移动端深色页面创建 TCP、STCP Proxy/Visitor，验证真实载荷，
逆序恢复三次操作，并检查秘密未进入 DOM、浏览器存储或审计数据库。
迁移用例先启动原文件代理验证转发，停机后执行真实 `config-migrate plan`，由测试显式
完成源文件维护、安装候选并运行 `check --offline`；重新启动后完成同一浏览器闭环。
删除迁移后的 Store 对象必须撤下监听，不能重新激活原文件同名对象；回退后恢复载荷转发。

在已安装 Playwright 和 Chromium 的环境中，从子项目根目录设置绝对路径后运行：

```sh
export FRP_CONFIG_E2E_AGENT="$PWD/dist/darwin-arm64/frp-plus-agent"
export FRP_CONFIG_E2E_SERVER="$PWD/dist/darwin-arm64/frp-plus-server"
export FRP_CONFIG_E2E_BROWSER_HELPER="$PWD/tests/config_browser_e2e.mjs"
export FRP_CONFIG_E2E_NODE="$(command -v node)"
export PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs
export BROWSER_EXECUTABLE=/absolute/path/to/chromium
cd .cache/upstream/worktree
go test -race -mod=readonly ./extension/frpmonitor/monitor -run '^TestConfigNative' -count=1 -v
```

这些变量仅控制测试。未提供二进制或浏览器参数时，用例明确跳过；跳过不能计为联合验收通过。
`TestConfigNativeUnmanagedAPIsEndToEnd` 还验证关闭托管时的原生 Dashboard 鉴权、
Store 创建/读取/删除、文件 PUT 只落盘以及显式 reload 后文件/Store 独立转发。
原生进程、管理端和浏览器使用临时回环端口及一次性凭据。浏览器凭据由 stdin 传递，
失败只输出固定阶段名，不打印异常 DOM、请求体或私有日志。

审计页面使用 `monitor/audit_browser_test.go` 的 `TestAuditBrowserEndToEnd`；设置同一组
Node/Playwright/Chromium 参数，并将 `FRP_AUDIT_BROWSER_HELPER` 指向
`tests/audit_browser_e2e.mjs`。它连接实际 Handler、认证、审计 API 和 SQLite，验证分页、
未知操作者、旧 Service ID、筛选、脱敏 JSONL 下载及会话清理；事件由测试种入，
不将该测试描述为原生配置执行流程的验收。

隧道历史页面使用 `monitor/tunnel_browser_test.go` 的 `TestTunnelBrowserEndToEnd`；
`FRP_TUNNEL_BROWSER_HELPER` 指向 `tests/tunnel_browser_e2e.mjs`，其余浏览器参数相同。
测试连接实际管理员认证、SQLite 和 VictoriaMetrics，验证一个逻辑对象的两代实例、
生命周期事件、真实历史曲线、7 天窗口、Visitor 筛选与退出清理；原生事件和累计值由
测试种入，原生转发由互通测试独立验证。页面单元测试还覆盖连接数可用但字节不受支持、
采样缺口、超过 uint64 的历史汇总及操作审计关联，避免把未知观察当成零值。
2026-10-03 静态测试树运行上述两个真实浏览器用例，`go test -v -race -count=1`
通过（8.674 秒）；前端 129 项测试通过，1366/390/320 宽度的明暗主题无横向溢出。

## 最小托管备份与真实恢复接管

`monitor/config_restore_native_test.go` 在真实配置联合测试后保留一个 prepared 事务，
强杀 Agent，通过原生 `managed-maintenance` 创建私有检查点并原地恢复，验证 Service
身份更换、启动日志恢复、写入门禁、Admin 接管、第二次离线确认和 TCP/STCP Visitor。
未接管不能离线确认；接管不会自动开放编辑。恢复能力自补丁 0008 接入，
以下测试使用当前完整 12 条补丁构建的 Agent：

```sh
export FRP_CONFIG_RESTORE_E2E_AGENT="$PWD/dist/darwin-arm64/frp-plus-agent"
export FRP_CONFIG_E2E_SERVER="$PWD/dist/darwin-arm64/frp-plus-server"
cd .cache/upstream/worktree
go test -race -mod=readonly ./extension/frpmonitor/monitor \
  -run '^TestConfigRestoreNativeEndToEnd$' -count=1 -v
```

如同时设置前文的 Node、Playwright、Chromium 参数，并将
`FRP_CONFIG_RESTORE_BROWSER_HELPER` 设为 `tests/config_restore_browser_e2e.mjs` 的绝对路径，
同一测试会经真实页面执行接管确认；校验明确勾选、仅一次接管请求、配置写入仍关闭和
离线确认说明，其后的真实数据库核对、停机确认、重启和业务转发断言仍执行。
未提供浏览器 helper 只算 API/native 恢复验收，不算浏览器恢复验收。

`ops_managed_test.py` 单独验证归档清单、路径/大小/解压预算、损坏拒绝、helper 调用
及格式边界；Go 的 managed/configuration 测试负责文件锁、写入中断、上下文漂移和
恢复状态机。模拟 helper 的 Python 单测不替代上述真实原生流程。

## 依赖检查点与停机完整归档

format2 保留旧生成安装范围；无 Store、无托管根的旧 Agent 使用新版 Python 工具即可
备份，不要求旧二进制提供 helper。format3 保留单 Agent 检查点 v1；format4 使用固定
编号 USTAR 成员封装检查点 v2，或保存停机主控的 SQLite 与完整 TSDB。

- `ops_checkpoint_test.py` 验证 format4 编号、摘要、长 UTF-8 路径、闭合清单、容量、
  链接/穿越/PAX 拒绝及原生 helper 边界。
- `ops_history_test.py` 验证主控 DB/WAL 一致副本、TSDB 文件和空目录、运行中锁拒绝、
  来源漂移、同路径恢复、重算摘要后仍缺配置引用的拒绝，以及旧 Agent 的 format2 分派。
  恢复父目录覆盖可信所有者的 0755 权限；staging/目标仍私有，拒绝不可信所有者、可写父目录、
  链接和目录替换。此测试不代表普通用户可以写入 root 持有的 `/var/lib`。
- `agent/configuration` 的 BackupGraph/Checkpoint 测试和 `agent/managed` 的
  Checkpoint/Restore 测试验证 sealed 来源、includes、旧/新 Store 的依赖闭包、安装恢复计划、
  门禁与 CAS；过期 journal 只归档幂等记录，不重新带回残余快照或回退能力。

```sh
python3 -m unittest discover -s tests -p 'ops_*test.py'
# 在 scripts/frp.py 已准备的固定源码目录执行：
cd .cache/upstream/worktree
go test -race -mod=readonly ./extension/frpmonitor/agent/managed \
  ./extension/frpmonitor/agent/configuration -run 'Checkpoint|Restore|BackupGraph' -count=1
```

2026-10-03 的最终 12 补丁本机验收使用真实 Agent/frps：wire v1/v2 均完成 mTLS、
Token 文件、include 和 `https2http` 插件证书的 format4 备份、依赖破坏、同路径恢复与
再次 HTTPS 转发；核对精确字节/纳秒 mtime、原锁 inode、身份轮换、幂等恢复 epoch，
未接管时离线确认仍被拒绝。主控真实恢复核对旧 v6 及新 schema12，后者包含恢复完成记录；
SQLite 校准、host/probe 历史保留，重启 Agent 后新样本继续增加。最终候选另通过 0755
原父目录下的私有目标恢复；46 项 ops 测试通过。这些是隔离恢复证据，不是生产组备份完成证明。

目前完整依赖恢复限同安装根、同路径；模板、外部映射和跨目录仍未开放。摘要校验不提供
归档来源签名，真实归档、凭据与路径必须保存在私有环境。

## 明确失败与回退故障修复

补丁 0009 的 `TestConfigEarlyFailureNativeEndToEnd` 使用
`FRP_CONFIG_EARLY_FAILURE_E2E_AGENT` 与 `FRP_CONFIG_E2E_SERVER`：真实 frps 端口策略拒绝、
真实 Visitor 本地端口冲突都会在 60 秒操作期限前回退，恢复精确 Store 字节；资源仍未就绪
或控制连接正常等待的单元反例不提前失败。没有新增任意网络探测或业务成功声明。

`TestConfigNativeRollbackFailureEndToEnd` 使用普通配置 E2E 参数：先成功运行 Visitor，
删除后占用其原监听端口，手工回退实际失败。断言旧 Store 已恢复而运行确认位不能沿用旧成功，
未完成操作继续阻止新草案；停机排除冲突再启动，本地日志恢复且主控只查询即可转为 rolled_back，
TCP/Visitor 恢复转发，再准备/取消新候选证明操作名额已释放。

## 配置变更下的有限持续负载

```sh
python3 tests/frp_config_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server \
  --wire v1 --wire v2 --soak-seconds 60 --timeout 40
```

可选 `--soak-seconds` 接受 30–3600 秒，默认关闭；同一 TCP socket 每秒连续回显，
并发反复修改 HTTP 压缩配置、验证全部已开放类型并精确回退。脚本核对指标/会话持续
推进、下行待处理数量和采样 RSS 上限；所有轮次完成后继续 CAS、断链看门狗与强杀恢复。
等待期限和强杀用例暂停本机测试 frps 并确认 `wait start`，与明确的登记拒绝分开；
暂停期间独立监控必须继续推进，正常路径与异常清理都会恢复进程。
这只证明指定时间内的有界流程，不能作为吞吐、CPU、长时泄漏或生产容量结论。

## 原版 FRP 互通矩阵

```sh
python3 tests/build_original.py
python3 tests/frp_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server \
  --original-agent .cache/original/bin/darwin-arm64/frpc \
  --original-server .cache/original/bin/darwin-arm64/frps
```

原版二进制必须来自 `upstream.lock` 固定 commit，未经 Overlay 或监控补丁，并保留
原生 Dashboard。矩阵覆盖增强/增强、原版/增强、增强/原版三种组合，各测 wire v1/v2
的 TCP、UDP、HTTP 虚拟主机和 STCP visitor；关闭监控的增强版不能冒充原版。
`build_original.py` 从固定 Git archive 独立构建，记录源码、工具链、双 Dashboard 与产物
摘要，构建前后检查源码未变更。互通脚本默认读取原版二进制同目录 `BUILD.json`，
强制匹配锁定身份、空补丁/Overlay 清单和实际二进制 SHA；分离存放时显式指定
`--original-manifest`。`--monitor-version` 仅作额外检查。本地清单校验不是签名信任证明。
可信归属冲突、不同 user 的同名 clientID、适配器忙锁和快照过期另有 Go 回归。

## 旧版 Plus 滚动升级兼容

原版 FRP 互通不能代替旧监控协议兼容。另从新增详情能力前的固定 Git 提交
`23851a2298eac85e384c8ba61cb9ba89710d24e5` 归档构建旧 Plus，保留旧补丁与 Overlay，
不能将新版本关闭详情冒充旧版本：

```sh
python3 tests/build_compat_baseline.py
python3 tests/frp_compat_smoke.py \
  --agent dist/darwin-arm64/frp-plus-agent \
  --server dist/darwin-arm64/frp-plus-server \
  --old-agent .cache/compat-baseline/project/dist/darwin-arm64/frp-plus-agent \
  --old-server .cache/compat-baseline/project/dist/darwin-arm64/frp-plus-server
```

执行前核对固定归档、旧输入和实际二进制摘要。新/新、新/旧、旧/新、旧/旧分别在私有
回环 TLS 环境连续观察 12 秒：指标和 last_seen 持续推进、TCP 载荷正确，监控必须保持
一个连接且没有关闭/重连。监控桥只统计 TLS 连接，不截取凭据或解密流量。
此检查验证版本组合稳定性，不代替持续负载或真实公网升级演练。

## Linux 实机对照

在运行 agent 的同一主机和命名空间执行：

```sh
python3 tests/linux_acceptance.py \
  --url https://127.0.0.1:17401 --ca /private/test/tls.crt --seconds 15
```

此工具只读公开 API 与本机 `/proc`，不需要管理/节点 token。多节点时用 `--node-id`，
显式网卡筛选使用相同 `--iface`。HTTPS 不跳过校验，HTTP 仅允许回环，不使用环境代理。
采样周期 1–10 秒、至少 3 个 API 样本；观察时长至少 `4 × 周期 + 4` 秒。

静态容量要求精确一致，网络累计计数须处于本地采样包络；CPU 容差 5 个百分点，
内存 used 容差为 0.5% 总量或 8 MiB，其他动态项使用工具输出的容差。
该检查验证取值与量级，不证明容器数据属于宿主机。报告仅输出脱敏固定字段；
`linux_acceptance_test.py` 验证算法、过滤与错误脱敏。

## 部署与持续负载

2026-10-03，当前完整 12 补丁 Linux amd64 候选已在主控宿主机以服务用户运行隔离测试：
`smoke.py` 的原生 wire v1/v2 和 `monitor_smoke.py --history` 通过。测试仅使用回环端口、
临时配置与随机凭据，并将 `TMPDIR` 指向具备足够空间的私有磁盘目录；未使用生产凭据。
隔离结果只证明该宿主机上的必要二进制流程，不替代生产数据或真实账号 OAuth 验收。

同日首批生产替换另行通过：1 主控与 10 Agent 均运行已核验候选；替换前 11 份停机归档
下载到本机并校验，旧程序/BUILD/unit 随组保留。独立验收核对 schema v6→v12、节点
身份/凭据/分组/套餐/账本保留、10 节点旧历史共同桶摘要相同及新增持久样本；3 个独立
原生 frps 的进程和程序未变。历史对比按相同 24h 桶起点读取，新样本使用 1h UTC 分钟桶。
私有部署证据与公开产物摘要见根 README 和 todo；此结果不包含后续检查点 v3。

以下范围仍需单独取得证据，不由本地单元测试或旧部署数字代替：

- 真实 GitHub OAuth App、允许/拒绝账号、注销和反向代理部署。
- Linux ARM64 运行、复杂 NAT/P2P，以及生产整组故障后的实际回退演练；本次停机、
  整组备份与替换已通过，同组恢复路径及真实备份恢复另在隔离环境验证。
- 长时历史保留、慢盘、队列背压、故障时实时监控和隧道独立性。
- 按当前 schema 测量 100/500 节点持续 CPU、内存、磁盘及浏览器开销；这些是测试档位，
  不是容量承诺。容量工具应直接复用当前初始化与协议，不能查询已移除的历史表。

本次授权范围是 1 个主控和 10 个 Agent 的 Linux amd64 一次性替换；ARM64、复杂 NAT、
长时容量与稳定性验证不作为此次上线前置。必要的功能验证、完整备份和失败回退仍须完成。

浏览器检查覆盖卡片、详情、管理编辑、浅/深主题、390px 窄屏、键盘及实时更新中的焦点保持。
测试方法在本文件维护，配置与架构细节分别见根 README 和相应模块说明。


Agent 检查点 v3 额外回归：

```sh
FRP_PLUS_TEST_AGENT=<本机新版Agent绝对路径> python3 -m unittest discover -s tests -p 'ops*_test.py'
# 在完整补丁组装树执行，二进制必须来自同一候选。
FRP_BACKUP_RELOCATION_E2E_AGENT=<新版Agent> FRP_BACKUP_RELOCATION_E2E_SERVER=<原生Server> \
  go test -race ./extension/frpmonitor/monitor -run TestBackupRelocationNativeEndToEnd -count=1
```

第二条覆盖 wire v1/v2、源与外部依赖删除、跨目录 mTLS/Token/include/HTTPS 插件/TCP 业务、
真实管理端接管与离线确认、旧终态材料的只读谱系查询及本地拒绝回退、再备份同路径恢复。
第一条未设置二进制时会明确跳过 3 项真实 CLI 测试；不能将纯包装通过等同原生恢复验收。
