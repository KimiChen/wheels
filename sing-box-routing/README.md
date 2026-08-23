# sing-box-routing

`sing-box-routing` 是部署在**海外优质网络 VPS** 上的 sing-box 分流控制面。网关接收本机/TUN 流量，加载业务 `.srs` 规则集，再把命中的流量转发到澳门、台湾、日本、美国等落地 VPS；未命中的流量使用网关自身网络直连。

仓库维护规则、节点清单、出口策略、配置渲染和验证，不保存真实节点凭据，也不在落地 VPS 上安装服务。

## 架构

```text
业务匹配条件（域名/IP/进程名）
    ↓
rules/*.txt → 临时 source JSON → *.srs   1. 规则层：命中什么
    ↓
policy/routes.json                       2. 策略层：规则走哪个数据出口
    ↓
policy/egress-groups.json                3. 出口层：逻辑出口选择哪些物理节点
    ↑
inventory/nodes.json
    ↓
config/gateway.base.json → config.json   4. 网关层：TUN、入站、DNS 与最终路由
```

四层相互解耦：

- TXT 规则及其生成的 SRS 只描述业务匹配条件（域名、IP 或进程名），不包含地区、节点地址或凭据。
- 路由策略只引用稳定的逻辑出口，例如 `egress-tw`，不直接引用物理 VPS，也不为每条业务规则重复配置 DNS。
- 节点清单只记录连接方式和环境变量名；真实地址、端口和密钥来自本地 `.env`。
- 出口组把同地区、用途等价的节点组成 `urltest + selector`。替换或增加落地 VPS 时无需修改 SRS。
- 基础配置只定义一个 `dns-direct`，使用 Cloudflare DoH 和 sing-box 默认 dialer 从海外网关自身发起；DNS 不参与地区数据出口选择。

## 当前分流

`policy/routes.json` 中的 `priority` 数字越小越先匹配：

| 优先级 | 规则集 | 数据出口 | 用途 |
| ---: | --- | --- | --- |
| 10 | `special` | `egress-mo` | 使用澳门出口的特定站点 |
| 100 | `tiktok` | `egress-us` | TikTok |
| 200 | `gemini` | `egress-tw` | Gemini |
| 300 | `ai` | `egress-jp` | 通用 AI 业务 |

上述规则只决定数据流量的出口；全部域名统一使用 `dns-direct` 解析，字面 IP 不触发 DNS 查询。未命中的数据流量使用 `direct`。`byteoversea.com` 同时存在于 `tiktok` 和 `ai`；由于 `tiktok` 的优先级更高，它会先命中 `tiktok` 并使用美国出口。

`tiktok` 还包含一条 `PROCESS-NAME` 规则。它会编译为 SRS 中的 `process_name`，但只有运行平台和入站场景能够识别流量所属进程时才可能命中；普通转发流量不会仅凭目标域名恢复客户端进程名。

`egress-mo` 和 `egress-us` 各有一个物理节点，`egress-tw` 有两个，`egress-jp` 有三个。路由只引用稳定的逻辑出口；同组的 `urltest` 自动选择当前可用且延迟合适的物理节点，替换或增加同用途节点时无需修改业务规则。

## 目录

```text
sing-box-routing/
├── config/
│   └── gateway.base.json       # 不含落地节点的网关基础配置
├── inventory/
│   └── nodes.json              # 物理节点及其环境变量引用
├── policy/
│   ├── egress-groups.json      # 逻辑出口和健康检查
│   └── routes.json             # SRS → 逻辑出口的唯一映射
├── rules/                      # Clash classical 风格 *.txt，规则唯一源码
│   └── README.md               # TXT 格式、支持类型和维护约束
├── tests/
│   ├── routing-cases.json      # 关键域名/IP 的预期命中结果
│   └── rule-list-syntax.txt    # TXT 转换器语法回归样例
├── scripts/
│   ├── convert-rule-list.sh    # TXT → 临时 sing-box source JSON
│   ├── build-rules.sh          # 按 routes.json 转换并编译 SRS
│   ├── render-config.sh        # 类型安全地生成完整配置
│   └── validate.sh             # staging、转换器与路由用例检查
├── .env.example                # 公开占位值；复制为本地 .env 后替换
├── .sing-box-version           # 构建和检查要求的 sing-box 版本
└── dist/                       # 生成物，不提交 Git
```

## 运行要求

- Bash、`jq`，以及 `sha256sum` 或 `shasum`；
- 与 `.sing-box-version` **完全一致**的 sing-box，当前为 `1.13.19`；
- 一份只保存在本地的 `.env`。可从公开占位模板创建：

```bash
cp .env.example .env
```

`.env` 中的值按以下方式使用：

| 变量 | 当前用途 |
| --- | --- |
| `TARGET_CONFIG_DIR` | `render-config.sh` 未显式传入 `--ruleset-dir` 时，用于生成 `<目录>/ruleset/*.srs` 路径；必须是非根绝对路径 |
| `PROXY_MO_AKILE_*` | 澳门 Shadowsocks 节点的地址、端口、方法和密码 |
| `PROXY_US_VIRCS_*` | 美国 Shadowsocks 节点的地址、端口、方法和密码 |
| `PROXY_TW_AKILE_*`、`PROXY_TW_DATAWAVE_*` | 两个台湾 Shadowsocks 节点的地址、端口、方法和密码 |
| `PROXY_JP_DMIT_*`、`PROXY_JP_AKILE_*`、`PROXY_JP_ZOUTER_*` | 三个日本 Shadowsocks 节点的地址、端口、方法和密码 |
| `TARGET_SSH_HOST`、`TARGET_SSH_PORT`、`TARGET_SSH_USER`、`TARGET_SSH_KEY` | 为人工部署预留；当前仓库脚本不使用这些变量，也不会自动 SSH 发布 |

每个节点的完整变量名由 `inventory/nodes.json` 的 `server_env`、`server_port_env`、`method_env` 和 `password_env` 声明，以 `.env.example` 为填写模板。渲染器把 dotenv 当作数据解析，不执行其中的 shell 代码；同名进程环境变量优先于 `.env`。本地 `.env` 已被 Git 忽略，禁止提交。

## 构建、渲染与验证

安装 `.sing-box-version` 指定版本的 sing-box（当前锁定 `1.13.19`），然后编译全部规则：

```bash
./scripts/build-rules.sh
```

sing-box 不在 `PATH` 时显式指定二进制：

```bash
SINGBOX_BIN=/absolute/path/to/sing-box ./scripts/build-rules.sh
```

`SINGBOX_BIN` 是执行脚本时传入的进程环境变量，不从 `.env` 读取。`build-rules.sh --policy FILE` 可在测试时覆盖路由策略；正常构建使用仓库内的 `policy/routes.json`。

规则源码默认使用 `rules/<tag>.txt`。每行采用 `类型,值`：

```text
DOMAIN,example.com
DOMAIN-SUFFIX,example.org
DOMAIN-KEYWORD,example-keyword
DOMAIN-REGEX,^api-[a-z]+\.example$
IP-CIDR,192.0.2.0/24,no-resolve
IP-CIDR6,2001:db8::/32,no-resolve
PROCESS-NAME,com.example.app
```

支持 `DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`DOMAIN-REGEX`、`IP-CIDR`、`IP-CIDR6` 和 `PROCESS-NAME`。`PROCESS-NAME` 会转换为 sing-box source rule-set 的 `process_name`；实际能否命中取决于运行平台和入站场景是否能识别流量所属进程。空行及以 `#` 开头的整行注释会保留在人工源码中；重复规则在转换时去重。CIDR 的 `no-resolve` 作为 Clash 兼容修饰符接受，但 sing-box source rule-set 没有对应字段，因此不会写入临时 JSON。

`IP-ASN` 默认忽略：转换器会在标准错误中报告文件、行号和值，并在结尾给出忽略数量。其他未知类型、未知参数和空值会直接让构建失败；如果文件除被忽略的 ASN 外没有有效规则，也会失败。

构建脚本只处理 `policy/routes.json` 声明的 TXT，先在临时目录生成 sing-box source JSON，再使用固定版本编译 SRS。全部成功后才替换 `dist/ruleset/` 并生成 `SHA256SUMS`；临时 JSON 不提交也不部署。不要手工编辑 `.srs`。

使用本地 `.env` 渲染完整配置：

```bash
./scripts/render-config.sh
```

默认生成 `dist/config.json`，规则路径使用 `.env` 中的 `TARGET_CONFIG_DIR/ruleset/<tag>.srs`。可通过 `--env-file`、`--output` 和 `--ruleset-dir` 为独立 staging 指定输入和输出；脚本将 dotenv 当作数据解析而不是 shell 代码，并以 `0600` 权限写入配置。运行依赖 `jq`。

测试其他配置组合时，还可用 `--base`、`--nodes`、`--groups` 和 `--routes` 分别覆盖基础配置、节点清单、出口策略和路由策略；这些参数不改变仓库文件。完整参数以各脚本的 `--help` 为准。

使用真实本地配置执行完整验证：

```bash
SINGBOX_BIN=/absolute/path/to/sing-box ./scripts/validate.sh
```

sing-box 已在 `PATH` 时可省略 `SINGBOX_BIN`。只需用公开示例数据做冒烟检查时使用：

```bash
./scripts/validate.sh --env-file .env.example
```

`validate.sh --tests FILE` 可覆盖路由用例文件；默认使用 `tests/routing-cases.json`。验证会重新构建 `dist/ruleset/`，因此需要与锁定版本一致的 sing-box，而不只是 `jq`。

验证脚本会：

- 配置、策略、节点和测试文件的结构检查；
- 检查 TXT 转换器的注释、去重、正则逗号、BOM/CRLF、CIDR、`no-resolve` 和忽略 ASN 行为；
- 按 `routes.json` 转换并构建当前 4 个 SRS，检查规则源码与路由标签的对应关系；
- 在临时 staging 中从指定 dotenv 类型安全地渲染配置；
- 使用 `.sing-box-version` 指定的准确版本执行真实 `sing-box check`；
- 使用编译后的 SRS 执行当前 8 个 `tests/routing-cases.json` 域名/IP 用例，验证规则集和数据出口；域名用例还验证统一使用 `dns-direct`，字面 IP 用例检查 DNS 不适用。

临时 staging 在验证完成后删除；可部署产物仍由 `build-rules.sh` 和默认的 `render-config.sh` 分别写入 `dist/ruleset/` 与 `dist/config.json`。

## 添加落地节点

1. 在 `inventory/nodes.json` 增加唯一节点 tag，例如 `node-tw-02`，只填写环境变量名，不填写真实地址或密钥。
2. 在本地 `.env` 增加这些变量的真实值；不要把值输出到终端或提交 Git。
3. 把节点 tag 加入 `policy/egress-groups.json` 对应地区的 `members`。
4. 运行完整验证，确认配置能渲染、节点引用有效且 sing-box 检查通过。

同一 `urltest` 组只应包含地区、用途和访问能力等价的节点。它按探测结果选择节点，不是跨地区灾备；不要把 `direct` 或其他地区作为地区敏感业务的隐式兜底。单节点组也保留相同的 `urltest + selector` 结构，方便以后无须改路由即可扩容。当前通用探测地址只验证 HTTPS 连通性和延迟，新增或替换成员时仍需逐个实测该组对应的真实业务。

## 添加或调整分流规则

1. 在 `rules/<tag>.txt` 新增或修改文本规则；TXT 文件名、SRS 文件名和规则 tag 保持一致。
2. 在 `policy/routes.json` 增加对应条目，指定 `priority` 和逻辑 `outbound`；不要为单条路由增加 DNS 字段。
3. 在 `tests/routing-cases.json` 增加代表性域名/IP 及预期规则集和出口；域名用例同时指定预期 DNS，字面 IP 用例不设置 DNS。每个规则集至少保留一个用例。
4. 运行完整验证。

具体业务规则应排在通用规则之前。跨规则重复项由 `routes.json` 的显式 `priority` 决定，调整规则时应同步检查优先级和测试用例；例如同时出现在 `tiktok` 与 `ai` 中的 `byteoversea.com` 会优先命中 `tiktok`。调整地区时只需修改数据出口；DNS 始终由海外网关上的 `dns-direct` 统一解析。

## DNS 策略

DNS 与地区数据出口解耦：

- 只保留 `dns-direct` 一个解析器，使用 Cloudflare DoH，并通过默认 dialer 从海外网关自身发起；
- `dns.final` 固定为 `dns-direct`，所有由 sing-box 处理的域名查询都自然落到这一默认解析器；
- 由 sing-box 处理的业务域名、普通直连域名和节点域名都统一使用 `dns-direct`；字面 IP 不触发 DNS 查询。

`dns-direct` 的服务器是字面 IP，因此不需要额外的 `domain_resolver`，也不要把它 `detour` 到完全空的 `direct` outbound。当前锁定版本会在服务启动阶段拒绝这种冗余 detour，而 `sing-box check` 不一定能发现该启动期错误。

澳门、台湾、日本、美国等地区仍只决定数据流量走哪个落地 VPS；DNS 的网络地域统一为海外网关，不经由这些落地出口。因此，`routes.json` 不包含逐路由 DNS 字段，`egress-groups.json` 也不定义地区 DNS。落地节点优先使用 IP；若节点地址是域名，则由 `dns-direct` 解析，不能通过该节点自身形成解析环路。

## 部署顺序

1. 在本地完成规则构建、配置渲染和全部验证。
2. 核对 `SHA256SUMS`，把 `config.json` 与整套 `ruleset/` 作为同一个版本上传到远端 staging 目录。
3. 在远端使用目标 sing-box 版本检查 staging 中的实际待部署文件，并用去掉 TUN 的临时回环入站真实启动一次；`sing-box check` 不覆盖所有启动期初始化错误。
4. 备份当前可运行版本，再原子切换配置和规则；不要只替换其中一部分。
5. 重启 sing-box，检查 systemd、TUN、策略路由、DNS，以及测试域名的实际出口。
6. 任一健康检查失败时，恢复完整旧版本并再次检查服务状态。

仓库不负责远端 SSH 发布或落地 VPS 的安装，具体服务器信息按工作区约定从私有文档读取，不得写入本仓库。

## 安全

这是公开 Git 仓库。不得提交真实节点域名/IP、节点端口、密码、证书、SSH 目标、私钥、本地 `.env`、渲染后的 `config.json`、staging 目录或 `dist/` 生成物。

- 每个落地节点使用独立凭据，便于单独轮换和吊销。
- 落地 VPS 防火墙只允许网关来源访问代理端口。
- 渲染和部署过程使用限制性文件权限，不在命令行、日志或错误信息中打印凭据。
- 公网入站必须单独设计认证；不要把当前仅监听 `127.0.0.1:7890` 的 mixed 入站直接暴露到公网。
