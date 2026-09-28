# 构建入口

`scripts/frp.py` 是 Python 3.11+ 标准库 CLI；运行平台为 macOS/Linux，需要 Git、
本机 Go 1.26.6+、Node.js 与 npm。输出包含 SQLite 节点存储、可选 TSDB 与 GitHub 管理登录。
原生 frpc/frps 功能和
Dashboard 保留；通过 `[telemetry]` / `[monitor]` 显式启用独立采集、探测、存储与网页。

从 `frp-monitor/` 运行：

```sh
python3 scripts/frp.py prepare
python3 scripts/frp.py test
python3 scripts/frp.py build --native
python3 tests/smoke.py --agent dist/darwin-arm64/frp-monitor-agent --server dist/darwin-arm64/frp-monitor-server
python3 tests/monitor_smoke.py --agent dist/darwin-arm64/frp-monitor-agent --server dist/darwin-arm64/frp-monitor-server
python3 scripts/frp.py build
python3 scripts/frp.py package
```

上例 smoke 路径适用于 Apple Silicon；其他主机使用 `build --native` 返回的目录。
命令成功后 stdout 输出 JSON，进度和编译日志写入 stderr。可从任意工作目录调用脚本。
同时只允许一个 CLI 使用同一缓存，避免准备/构建互相覆盖。

| 命令 | 行为 |
| --- | --- |
| `prepare` | 严格校验锁文件，获取并核验上游 annotated tag 和 Commit，在临时目录应用补丁和映射扩展，成功后发布源码树 |
| `test` | 重新 prepare，运行 `pkg/config/...`、`pkg/msg/...`、`pkg/util/...`、`pkg/metrics/...`、`client`、`server`、`server/registry`、`server/proxy` 和所有 `extension/frpmonitor/...` 本机 Go 测试；不运行 Docker/e2e |
| `build` | 重新 prepare，构建原生 frpc/frps Dashboard，再构建 Linux amd64 和 arm64 两套二进制 |
| `build --native` | 使用本机 OS/架构，供本地 smoke；支持 macOS/Linux amd64/arm64 |
| `build --target linux/amd64` | 只构建指定目标；可重复 `--target`，不能与 `--native` 同用 |
| `package` | 重新完成两个 Linux 目标的构建，生成两个 tar.gz 及外部 `SHA256SUMS` |

## 配置与路径

默认安全读取子项目根 `.env`，仅接受以下三个键；同名进程环境变量优先：

| 键 | 默认值 | 约束 |
| --- | --- | --- |
| `FRP_MONITOR_CACHE_DIR` | `.cache` | 位于本子项目 `.cache/` 内，可为绝对路径或相对路径 |
| `FRP_MONITOR_OUTPUT_DIR` | `dist` | 位于本子项目 `dist/` 内，可为绝对路径或相对路径 |
| `FRP_MONITOR_UPSTREAM_MIRROR` | `upstream.lock` 的官方仓库 | 可省略或留空；也可指定本地 Git 镜像路径或不含凭据的 HTTPS URL |

`.env` 只按 UTF-8 读取字面量赋值，不运行 shell、不执行文件、不展开变量。
接受简单单/双引号和未引用值后的 ` # 注释`；白名单键中的 `$`、反引号及控制字符拒绝。
其他键忽略，因此 Git 凭据或业务配置不会加入构建参数。生成路径不允许 `..`、
项目外路径或符号链接，确保产物留在已忽略的目录内。

默认目录：

```text
.cache/upstream/repository.git   固定上游对象缓存
.cache/upstream/worktree/        本次准备的源码，无 .git
.cache/go-build/                 Go 编译缓存
.cache/go-mod/                   Go module 缓存
.cache/npm/                      npm 缓存
.cache/web-assets/               原生 Dashboard 编译结果缓存
dist/linux-amd64/               frp-monitor-agent、frp-monitor-server、BUILD.json
dist/linux-arm64/               同上
dist/<本机OS>-<本机架构>/         --native 产物
```

## 固定来源与构建边界

- `upstream.lock` 必须完整且只有当前 schema 字段。仓库固定为官方 FRP，校验
  `tag_object` 的对象类型、完整 SHA 和 annotated tag 解引用后的 Commit。
  Git replace refs 被禁用；镜像只能提供锁文件指定的身份，不能改变版本。
- 缓存源码使用 `git archive`；路径穿越、外部链接和特殊文件拒绝。上游内部文档
  符号链接物化成普通文件。每次 prepare 都重新组装，并先移除上一份带工具元数据
  的源码树；组装/补丁失败不会发布半成品。初始化时锁文件或配置解析失败可能保留
  旧的已验证树，但命令仍失败。未带工具元数据的目录不会被删除。
- `patches/series` 按列出的顺序应用补丁，先 `git apply --check` 再应用。
  文件名必须为 `0001-name.patch` 形式，不允许重复项、路径穿越或补丁 symlink。
- 仅将 `agent/`、`monitor/`、`shared/`、`web/` 映射到
  `extension/frpmonitor/`；使用上游 `github.com/fatedier/frp` module。
  扩展中的 symlink、`.env*`、依赖目录和嵌套 Go module 会被拒绝。
- 原生 Dashboard 来自同一固定上游源码的 `web/frpc` 与 `web/frps`，使用该树
  的 `web/package-lock.json` 执行 `npm ci` 及两个 workspace 的 `npm run build`。
  不使用 `noweb`。缓存按全部网页源文件摘要、Node/npm 版本分组；使用前重新
  计算两个完整 dist 树的摘要，缺文件、额外文件或内容变化会触发重建。
- Go 使用本机工具链，显式 `GOTOOLCHAIN=local`、`GOENV=off`、`GOWORK=off`、
  `CGO_ENABLED=0`、`-mod=readonly`、`-trimpath`、`-buildvcs=false` 和空 build ID。
  清除影响架构、实验功能、默认调试行为及模块下载/校验来源（`GOPATH`、`GOPROXY`、
  `GOSUMDB`、`GONOPROXY`、`GONOSUMDB`、`GOPRIVATE`、`GOVCS`）的 Go 环境选项，
  不自动下载 Go 工具链。
  工具链必须满足补丁后的 `go.mod`，内嵌 VictoriaMetrics 要求 Go 1.26.6+；`BUILD.json` 记录本次 Go/Node/npm 版本、上游身份、
  构建脚本/补丁/扩展摘要、原生网页资源摘要以及二进制摘要。
- 重现构建要求相同源码、依赖、操作系统、Go/Node/npm 工具链及构建条件；不承诺
  不同工具链版本逐字节相同。tar.gz 文件排序、权限、所有者和时间戳固定。
- 每个架构的两个二进制均成功后才替换其目录。构建失败时旧架构产物可能保留，
  多架构构建也可能只完成前一个目标；应以命令退出码和最终 JSON 为成功依据，
  不将失败后残留目录当作本轮成功产物。

发布包只包含两个二进制、`BUILD.json`、`LICENSE`、`LICENSE.monitor-probe`、`THIRD_PARTY_NOTICES.md`、
`upstream.lock`、SQLite/TSDB 依赖许可证 `licenses/`、`scripts/ops.py`、`scripts/local.py`、
`monitor/control/schema.sql`、
`packaging/README.md`、`packaging/nginx.conf.example`、`.env.example`、说明及包内 `SHA256SUMS`。许可证使用
文件名白名单并保留来源目录；不会打包 `.env`、本地运行数据、
上游临时树、npm 依赖目录或源码。`package` 失败时同样不能沿用残留发布包。

## 离线脚本测试

```sh
python3 -m unittest discover -s tests -p pipeline_test.py -v
```

测试用临时本地 Git 仓库构造 annotated tag，不联网或运行 Go/npm；覆盖锁文件与
Tag/Commit 不匹配、安全配置、路径与 symlink、重复 prepare、成功和失败补丁、
归档路径、Git replace refs 隔离、原生网页缓存完整性、固定发布包内容及校验和。
FRP 实际转发与重连验收另见 `tests/README.md`。

## 本地演示

`python3 scripts/local.py init` 读取以下 `.env` 配置，进程环境变量优先：

| 配置 | 默认值 / 用途 |
|---|---|
| `FRP_MONITOR_PORT` / `FRP_SERVER_PORT` | 17401 / 17000，两个端口必须不同 |
| `FRP_MONITOR_INTERVAL_SECONDS` | 1，范围 1–3600 |
| `FRP_MONITOR_HISTORY_DATA_PATH` | 空为关闭；目录非空启用，相对路径按本次运行数据目录解析 |
| `FRP_MONITOR_RETENTION_DAYS` | 7，范围 1–365 |
| `FRP_AGENT_NAME` / `FRP_AGENT_IFACE` | 本地演示节点 / 空接口筛选 |
| `FRP_GITHUB_CLIENT_ID` | GitHub OAuth App 的客户端 ID |
| `FRP_GITHUB_CLIENT_SECRET_FILE` | 外部私有密钥文件路径 |
| `FRP_GITHUB_CALLBACK_URL` | 管理入口 HTTPS callback |
| `FRP_GITHUB_ADMIN_USERS` | 逗号分隔的 GitHub 个人账号用户名允许列表 |

OAuth 四项一起配置；全部留空时管理登录不可用，公开监控和节点上报仍可运行。
工具只按 UTF-8 读取字面量，不执行 shell 或展开凭据。其他 `.env` 键不传入运行进程。
读取时拒绝符号链接和特殊文件，打开后核验常规文件类型，最多读取 1 MiB。

初始化在 `data/local/` 创建 `control.sqlite`，预置自增数字 ID 的本地节点、节点令牌
摘要和可信 FRP 绑定；生成独立 FRP/节点 Token、TOML 与带回环 SAN 的 30 天自签证书。
目录 0700，文件 0600；不覆盖已有目录，不导入旧结构。可用
`--directory data/another-demo` 新建安装；`--http` 显式选择回环明文开发。
证书不加入系统信任，也不跳过 TLS 验证。

历史默认关闭；初始化配置将非空历史目录写到 `monitor.historyDataPath`，不再生成单独开关。
目录不是 SQLite 文件，不需要独立服务。
`init --probes` 在 `settings.probe_json` 中生成本机 FRP 端口探测，同时明确授权该 agent
访问私网/回环。后续探测修改由管理 API 写入控制库；目标和 Token 不进入公开网页。
管理员访问 `/admin/` 后跳转 GitHub 登录，初始化不生成管理员登录令牌。

`python3 scripts/local.py run` 先检查演示目录中 `local.crt` 的有效期，再使用 native `verify` 校验配置，最后启动当前平台
`dist/<OS>-<ARCH>/` 的两个程序。自定义构建输出目录需直接使用相应二进制。
过期或无效证书会在启动前报错，应在新目录重新初始化演示；HTTP 演示无需证书检查。
Ctrl-C/TERM 清理子进程，日志留在私有目录。已有安装不会随 `.env` 自动更新；
TOML、OAuth 允许列表与启动密钥修改后重启，节点与探测业务通过管理界面即时更新。

`ops.py` 提供 server/agent 私有配置初始化、控制库在线备份、恢复验证和 systemd 模板。
控制库快照覆盖当前配置及账本；可选 TSDB 历史的备份范围单独说明，不将活动历史目录
当作普通文件直接打包。工具不安装服务、不执行远程命令，完整命令见
[发布与恢复](../packaging/README.md)。
