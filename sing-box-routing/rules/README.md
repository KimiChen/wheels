# 规则源码

本目录的四个 `.txt` 文件是业务规则的唯一权威源码。构建工具当前严格锁定 sing-box `1.13.19`（见 `../.sing-box-version`）；此前使用的 `1.13.4` 已不再受当前构建流程接受。转换产生的临时 JSON、二进制 `.srs` 与渲染配置均为生成物，不应手工维护或提交 Git。

TXT 及其生成的 SRS 只描述域名、IP、进程名等业务匹配条件，不包含地区、落地节点、DNS 或代理凭据。规则到逻辑出口的唯一映射位于 `../policy/routes.json`；物理节点和出口组分别位于 `../inventory/nodes.json` 与 `../policy/egress-groups.json`。所有由 sing-box 处理的域名查询统一使用海外网关上的 Cloudflare DoH `dns-direct`，路由规则只决定数据出口；字面 IP 不触发 DNS 查询。

## 当前映射

路由按 `priority` 从小到大匹配，先命中的规则生效：

| 源文件 | 生成文件 | 规则标签 | 优先级 | 目的地区 | 逻辑出口 |
| --- | --- | --- | ---: | --- | --- |
| `special.txt` | `special.srs` | `special` | 10 | 澳门 | `egress-mo` |
| `tiktok.txt` | `tiktok.srs` | `tiktok` | 100 | 美国 | `egress-us` |
| `gemini.txt` | `gemini.srs` | `gemini` | 200 | 台湾 | `egress-tw` |
| `ai.txt` | `ai.srs` | `ai` | 300 | 日本 | `egress-jp` |

`byteoversea.com` 同时存在于 `tiktok.txt` 和 `ai.txt`。由于 `tiktok` 的优先级 100 高于 `ai` 的 300，该域名会先命中 `tiktok` 并使用美国出口。

约定如下：

- `tag`、`.txt` 规则文件名和生成的 `.srs` 文件名必须一致。
- 具体业务规则应排在通用业务规则之前；跨规则重复项必须通过 `priority` 明确顺序，并由路由测试固定预期。
- 新增、重命名或删除规则时必须同步修改 `../policy/routes.json`。构建脚本会拒绝孤立的 `.txt`、失效引用、重名标签或优先级，以及旧的 `rules/*.json` 源文件。

## TXT 格式与转换

文件使用 UTF-8，一行一条 Clash classical 风格规则。空行以及去除首尾空白后以 `#` 开头的整行注释会被忽略；转换器会对每类规则排序去重。

| TXT 类型 | 临时 source rule-set 字段 | 限制 |
| --- | --- | --- |
| `DOMAIN` | `domain` | 不接受额外参数；去除首尾空白后，值内不能含空白 |
| `DOMAIN-SUFFIX` | `domain_suffix` | 不接受额外参数；去除首尾空白后，值内不能含空白 |
| `DOMAIN-KEYWORD` | `domain_keyword` | 不接受额外参数；去除首尾空白后，值内不能含空白 |
| `DOMAIN-REGEX` | `domain_regex` | 第一个逗号后的完整内容作为正则表达式 |
| `IP-CIDR`、`IP-CIDR6` | `ip_cidr` | 仅可附加 `no-resolve`；该兼容修饰符不会写入 JSON |
| `PROCESS-NAME` | `process_name` | 不接受额外参数；转换成独立的 headless rule |
| `IP-ASN` | 不生成字段 | 仅校验十进制 ASN，可附加 `no-resolve`，随后输出警告并忽略 |

转换结果使用 sing-box source rule-set `version: 2`。域名和 CIDR 条件写入一个 headless rule，所有 `PROCESS-NAME` 条件写入另一个独立 rule，使进程名与域名/IP 可以分别命中，而不会被组合成必须同时满足的条件。未知类型、未知参数、非法 ASN，以及只包含被忽略 `IP-ASN` 的文件都会导致转换失败。

`process_name` 仅受 sing-box 的 Linux、Windows 和 macOS 实现支持，并且必须能从当前主机的连接识别出所属进程。海外网关无法获知另一台设备上发起连接的应用进程，因此远程客户端或转发流量通常不会命中 `PROCESS-NAME`；这类场景仍应以域名/IP 规则为主。当前 `tiktok.txt` 中的进程规则会作为补充条件独立匹配，不会限制同文件内的域名规则。

## 构建与验证

在子项目根目录执行：

```bash
SINGBOX_BIN=/path/to/sing-box-1.13.19 ./scripts/build-rules.sh
SINGBOX_BIN=/path/to/sing-box-1.13.19 ./scripts/validate.sh --env-file .env.example
```

`build-rules.sh` 会依照路由优先级将 TXT 转为临时 JSON，再用锁定版本编译到 `dist/ruleset/*.srs`，同时生成 `SHA256SUMS`；临时 JSON 在构建后删除。`validate.sh` 还会覆盖转换器的支持格式与失败场景、编译全部四个 SRS、渲染配置、执行 `sing-box check`，并验证 `tests/routing-cases.json` 中的路由与 DNS 预期。

脚本要求实际使用的 sing-box 版本与 `../.sing-box-version` 完全一致，因此 `1.13.4` 等旧版本会在构建或验证开始时被明确拒绝。
