# 第三方组件声明

本项目是 [sing-box](https://github.com/SagerNet/sing-box) 的衍生作品，以 **GPL-3.0-or-later** 授权。
许可证全文与上游附加条款原文见本目录的 [LICENSE](LICENSE)。

## 上游

| 项 | 值 |
| --- | --- |
| 项目 | `github.com/SagerNet/sing-box` |
| 版本 | `v1.14.0` |
| commit | `0b8995879f29a9b98ee027bc17b75e101445b238` |
| 许可证 | GPL-3.0-or-later，附「衍生作品未经同意不得使用该应用名称或暗示关联」 |
| 版权 | Copyright (C) 2022 by nekohasekai <contact-sagernet@sekai.icu> |

上游同一作者的支撑库（`sing`、`sing-mux`、`sing-vmess`、`sing-shadowsocks2`、`sing-tun` 等）
以 Go module 依赖形式引入，未修改，版本由 `go.sum` 固定，许可证以各自仓库为准。

## 复制并修改的文件（GPLv3 §5(a)）

下列文件复制自上游 `cmd/sing-box/`，因为 `github.com/sagernet/sing-box/cmd/sing-box` 是
`package main`、Go 禁止导入，而本项目必须保持 CLI 的 argv 与退出码兼容。
每个文件顶部都有来源 commit、复制日期与逐条修改说明；双向哈希登记在
`cmd/sing-box-plus/copied-files.lock`。

| 文件 | 上游行数 | 复制日期 | 本项目修改 |
| --- | --- | --- | --- |
| `cmd.go` | 74 | 2026-09-06 | 命令名改为 `sing-box-plus`；`include.Context()` 换成 `box.Context()` + 自有最小 registry 与不含 ssmapi 的 service registry |
| `main.go` | 11 | 2026-09-06 | 仅加注释头 |
| `cmd_run.go` | 232 | 2026-09-06 | 解码前预扫描原始配置；进入 run 循环前建立进程级 registry；每次建 Box 前对账；`box.New()` 之后、`Start()` 之前注入 tracker |
| `cmd_check.go` | 43 | 2026-09-06 | 在 `box.New()` 之前独立校验；比对重载不变量；为独立 `check` 注入只用于校验的 registry |
| `cmd_format.go` | 77 | 2026-09-06 | 仅加注释头 |
| `cmd_version.go` | 64 | 2026-09-06 | 输出改为固定四行，上游 commit 由自有 `-X` 变量携带 |
| `cmd_netns_holder.go` | 20 | 2026-09-06 | 仅加注释头 |
| `cmd_run_userns_linux.go` | 78 | 2026-09-06 | 仅加注释头 |
| `cmd_run_userns_other.go` | 9 | 2026-09-06 | 仅加注释头 |

合计 608 行。

## 命名

`sing-box-plus` 是内部代号，不作为可发布产品名。上游附加条款明确禁止衍生作品使用该应用名称或
暗示关联，对外发布必须使用中性名称或先取得许可。

## 义务提示

- 向公开仓库提交本 overlay 的源码即构成 GPLv3 意义上的 convey，因此本目录必须包含
  许可证全文、上游附加条款原文、本文件，以及每个修改文件的修改内容与日期。
- 仅在自有主机部署、不向第三方交付二进制，不产生额外源码义务；GPLv3 没有 AGPL 式的网络使用条款。
- 向第三方交付二进制时须随附对应完整源码或书面要约。
- 通过快照接口消费本项目的下游系统是独立进程，不因该接口而受 GPL 传染；
  但不得链接或内嵌本项目的 Go 代码。
