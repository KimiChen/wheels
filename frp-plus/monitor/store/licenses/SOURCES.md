# SQLite 依赖许可来源

随发布包附带本目录完整内容。文件从 Go module 校验和锁定的源包逐字复制。SQLite 向量扩展未启用，仍附其许可；libc 的第三方许可集合必须一并保留。

| 模块 | 版本 | 文件 |
|---|---|---|
| [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite@v1.59.0) | `v1.59.0` | `modernc-sqlite/LICENSE`, `modernc-sqlite/LICENSE-SQLITE`, `modernc-sqlite/LICENSE-SQLITE_VEC`, `modernc-sqlite/AUTHORS` |
| [modernc.org/libc](https://pkg.go.dev/modernc.org/libc@v1.75.7) | `v1.75.7` | `modernc-libc/LICENSE`, `modernc-libc/LICENSE-3RD-PARTY.md`, `modernc-libc/AUTHORS` |
| [modernc.org/mathutil](https://pkg.go.dev/modernc.org/mathutil@v1.7.1) | `v1.7.1` | `modernc-mathutil/LICENSE`, `modernc-mathutil/AUTHORS` |
| [modernc.org/memory](https://pkg.go.dev/modernc.org/memory@v1.12.1) | `v1.12.1` | `modernc-memory/LICENSE`, `modernc-memory/AUTHORS` |
| [github.com/dustin/go-humanize](https://pkg.go.dev/github.com/dustin/go-humanize@v1.0.1) | `v1.0.1` | `dustin-go-humanize/LICENSE` |
| [github.com/mattn/go-isatty](https://pkg.go.dev/github.com/mattn/go-isatty@v0.0.24) | `v0.0.24` | `mattn-go-isatty/LICENSE` |
| [github.com/ncruces/go-strftime](https://pkg.go.dev/github.com/ncruces/go-strftime@v1.0.0) | `v1.0.0` | `ncruces-go-strftime/LICENSE` |
| [github.com/remyoudompheng/bigfft](https://pkg.go.dev/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec) | `v0.0.0-20230129092748-24d4a6f8daec` | `remyoudompheng-bigfft/LICENSE` |
| [github.com/google/pprof](https://pkg.go.dev/github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3) | `v0.0.0-20260802141513-ef3492d7dac3` | `google-pprof/LICENSE` |
| [golang.org/x/net](https://pkg.go.dev/golang.org/x/net@v0.57.0) | `v0.57.0` | `golang-net/LICENSE`, `golang-net/PATENTS` |
| [golang.org/x/tools](https://pkg.go.dev/golang.org/x/tools@v0.48.0) | `v0.48.0` | `golang-tools/LICENSE`, `golang-tools/PATENTS` |

sqlite 标签提交：`c96a4e6cb22254bf70026502a781a54a053c2cf0`。
go.mod 保持 sqlite 所要求的 `modernc.org/libc v1.75.7`；FRP 的既有 UUID 继续遵循原许可；TSDB 引起的 x/sys 等版本更新见 TSDB-SOURCES.md。
