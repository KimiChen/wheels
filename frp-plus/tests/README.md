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

构建脚本准备固定上游和 Overlay，运行 FRP 配置、消息、工具、metrics、完整 `client/...`
（含 Proxy/Visitor）、`server/...` 及所有监控扩展测试。采集的 17 组脱敏向量保存在
[fixtures/collect](fixtures/collect/README.md)，入口自动设置其路径。
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

## 最小托管备份与真实恢复接管

`monitor/config_restore_native_test.go` 在真实配置联合测试后保留一个 prepared 事务，
强杀 Agent，通过原生 `managed-maintenance` 创建私有检查点并原地恢复，验证 Service
身份更换、启动日志恢复、写入门禁、Admin 接管、第二次离线确认和 TCP/STCP Visitor。
未接管不能离线确认；接管不会自动开放编辑。测试使用补丁 0008 的 Agent：

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

以下结果需要在实际环境另行取得，不由本地单元测试或旧部署数字代替：

- 真实 GitHub OAuth App、允许/拒绝账号、注销和反向代理部署。
- Linux 双架构采样、systemd TERM/SIGKILL、备份恢复及切换。
- 长时历史保留、慢盘、队列背压、故障时实时监控和隧道独立性。
- 按当前 schema 测量 100/500 节点持续 CPU、内存、磁盘及浏览器开销；这些是测试档位，
  不是容量承诺。容量工具应直接复用当前初始化与协议，不能查询已移除的历史表。

浏览器检查覆盖卡片、详情、管理编辑、浅/深主题、390px 窄屏、键盘及实时更新中的焦点保持。
测试方法在本文件维护，配置与架构细节分别见根 README 和相应模块说明。
