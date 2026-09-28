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

构建脚本准备固定上游和 Overlay，运行 FRP 配置、消息、工具、metrics、client、server、
registry、proxy 及所有监控扩展测试。采集的 17 组脱敏向量保存在
[fixtures/collect](fixtures/collect/README.md)，入口自动设置其路径。
对并发/生命周期改动，在准备目录运行相关包的 `go test -race`；扩展包同时运行 `go vet`。
`nginx_test.py` 另需本机 nginx 与 openssl；缺少时明确跳过。它在临时回环端口启动隔离反代，
检查随包模板的来源限流、SSE 配额、OAuth 回调和 WSS 握手，不接触现有服务。

| 范围 | 关键验证 |
|---|---|
| 采集与会话 | 质量标记、大整数、scope/boot/接口变化、TLS、hello、序号/连接所有权、心跳与新鲜度分离 |
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
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
python3 tests/monitor_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server
python3 tests/monitor_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server --history
```

`smoke.py` 验证原生 TOML、Dashboard 认证及嵌入资源、无 Proxy 登录、wire v1/v2
二进制 TCP echo、服务端重启后同一 agent 重连。该文件同时提供有限的进程、网络和
公开 API 测试工具，避免测试入口相互依赖。

`monitor_smoke.py` 验证当前 SQLite、数字节点、公开 JSON/SSE、四种流量边界、历史目录
开关和重启恢复；并覆盖 FRP 与监控连接的故障隔离、节点凭据变更以及真实探测任务生命周期。
运行时业务变更使用临时库与主控刷新，不绕过或替代 Go 测试中的管理端认证。

所有子进程使用隔离环境，回环 TLS 校验名称和 CA；超时强制结束会判失败。仅本机 Linux
会产生完整内核指标，Darwin 合法返回 unsupported。退出后清理测试进程与临时文件。

## 原版 FRP 互通矩阵

```sh
python3 tests/frp_smoke.py \
  --agent dist/darwin-arm64/frp-monitor-agent \
  --server dist/darwin-arm64/frp-monitor-server \
  --original-agent /private/test/original-frpc \
  --original-server /private/test/original-frps
```

原版二进制必须来自 `upstream.lock` 固定 commit，未经 Overlay 或监控补丁，并保留
原生 Dashboard。矩阵覆盖增强/增强、原版/增强、增强/原版三种组合，各测 wire v1/v2
的 TCP、UDP、HTTP 虚拟主机和 STCP visitor；关闭监控的增强版不能冒充原版。
可信归属冲突、不同 user 的同名 clientID、适配器忙锁和快照过期另有 Go 回归。

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
