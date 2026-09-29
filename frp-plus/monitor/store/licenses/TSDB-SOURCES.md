# 内嵌 VictoriaMetrics 依赖许可证

下列模块用于 `CGO_ENABLED=0` 的内嵌历史存储；原文取自相应版本的官方 Go module。

| 模块 | 版本 | 许可证目录 |
|---|---|---|
| [github.com/VictoriaMetrics/VictoriaMetrics](https://pkg.go.dev/github.com/VictoriaMetrics/VictoriaMetrics@v1.151.0) | `v1.151.0` | `tsdb-victoriametrics-victoriametrics` |
| [github.com/VictoriaMetrics/easyproto](https://pkg.go.dev/github.com/VictoriaMetrics/easyproto@v1.2.0) | `v1.2.0` | `tsdb-victoriametrics-easyproto` |
| [github.com/VictoriaMetrics/fastcache](https://pkg.go.dev/github.com/VictoriaMetrics/fastcache@v1.13.3) | `v1.13.3` | `tsdb-victoriametrics-fastcache` |
| [github.com/VictoriaMetrics/metrics](https://pkg.go.dev/github.com/VictoriaMetrics/metrics@v1.44.0) | `v1.44.0` | `tsdb-victoriametrics-metrics` |
| [github.com/VictoriaMetrics/metricsql](https://pkg.go.dev/github.com/VictoriaMetrics/metricsql@v0.87.3) | `v0.87.3` | `tsdb-victoriametrics-metricsql` |
| [github.com/cespare/xxhash/v2](https://pkg.go.dev/github.com/cespare/xxhash/v2@v2.3.0) | `v2.3.0` | `tsdb-cespare-xxhash-v2` |
| [github.com/golang/snappy](https://pkg.go.dev/github.com/golang/snappy@v1.0.0) | `v1.0.0` | `tsdb-golang-snappy` |
| [github.com/klauspost/compress](https://pkg.go.dev/github.com/klauspost/compress@v1.19.1) | `v1.19.1` | `tsdb-klauspost-compress` |
| [github.com/valyala/bytebufferpool](https://pkg.go.dev/github.com/valyala/bytebufferpool@v1.0.0) | `v1.0.0` | `tsdb-valyala-bytebufferpool` |
| [github.com/valyala/fastrand](https://pkg.go.dev/github.com/valyala/fastrand@v1.1.0) | `v1.1.0` | `tsdb-valyala-fastrand` |
| [github.com/valyala/histogram](https://pkg.go.dev/github.com/valyala/histogram@v1.2.0) | `v1.2.0` | `tsdb-valyala-histogram` |
| [github.com/valyala/quicktemplate](https://pkg.go.dev/github.com/valyala/quicktemplate@v1.8.0) | `v1.8.0` | `tsdb-valyala-quicktemplate` |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys@v0.47.0) | `v0.47.0` | `tsdb-golang-sys` |

## 模块版本联动更新

以下依赖随 VictoriaMetrics 的模块版本约束更新，供完整 FRP 二进制使用。

| 模块 | 版本 | 许可证目录 |
|---|---|---|
| [github.com/gorilla/websocket](https://pkg.go.dev/github.com/gorilla/websocket@v1.5.4-0.20250319132907-e064f32e3674) | `v1.5.4-0.20250319132907-e064f32e3674` | `tsdb-gorilla-websocket` |
| [github.com/prometheus/client_golang](https://pkg.go.dev/github.com/prometheus/client_golang@v1.24.0) | `v1.24.0` | `tsdb-prometheus-client_golang` |
| [github.com/spf13/pflag](https://pkg.go.dev/github.com/spf13/pflag@v1.0.9) | `v1.0.9` | `tsdb-spf13-pflag` |
| [golang.org/x/time](https://pkg.go.dev/golang.org/x/time@v0.15.0) | `v0.15.0` | `tsdb-golang-time` |
| [k8s.io/apimachinery](https://pkg.go.dev/k8s.io/apimachinery@v0.36.3) | `v0.36.3` | `tsdb-k8s.io-apimachinery` |
| [k8s.io/client-go](https://pkg.go.dev/k8s.io/client-go@v0.36.3) | `v0.36.3` | `tsdb-k8s.io-client-go` |
| [k8s.io/utils](https://pkg.go.dev/k8s.io/utils@v0.0.0-20260707023825-cf1189d6abe3) | `v0.0.0-20260707023825-cf1189d6abe3` | `tsdb-k8s.io-utils` |
| [github.com/davecgh/go-spew](https://pkg.go.dev/github.com/davecgh/go-spew@v1.1.2-0.20180830191138-d8f796af33cc) | `v1.1.2-0.20180830191138-d8f796af33cc` | `tsdb-davecgh-go-spew` |
| [github.com/go-logr/logr](https://pkg.go.dev/github.com/go-logr/logr@v1.4.4) | `v1.4.4` | `tsdb-go-logr-logr` |
| [github.com/pmezard/go-difflib](https://pkg.go.dev/github.com/pmezard/go-difflib@v1.0.1-0.20181226105442-5d4384ee4fb2) | `v1.0.1-0.20181226105442-5d4384ee4fb2` | `tsdb-pmezard-go-difflib` |
| [github.com/prometheus/client_model](https://pkg.go.dev/github.com/prometheus/client_model@v0.6.2) | `v0.6.2` | `tsdb-prometheus-client_model` |
| [github.com/prometheus/common](https://pkg.go.dev/github.com/prometheus/common@v0.70.1) | `v0.70.1` | `tsdb-prometheus-common` |
| [github.com/prometheus/procfs](https://pkg.go.dev/github.com/prometheus/procfs@v0.21.1) | `v0.21.1` | `tsdb-prometheus-procfs` |
| [github.com/valyala/gozstd](https://pkg.go.dev/github.com/valyala/gozstd@v1.25.0) | `v1.25.0` | `tsdb-valyala-gozstd` |
| [google.golang.org/protobuf](https://pkg.go.dev/google.golang.org/protobuf@v1.36.12-0.20260120151049-f2248ac996af) | `v1.36.12-0.20260120151049-f2248ac996af` | `tsdb-google.golang.org-protobuf` |
| [sigs.k8s.io/json](https://pkg.go.dev/sigs.k8s.io/json@v0.0.0-20250730193827-2d320260d730) | `v0.0.0-20250730193827-2d320260d730` | `tsdb-sigs.k8s.io-json` |
| [sigs.k8s.io/yaml](https://pkg.go.dev/sigs.k8s.io/yaml@v1.6.0) | `v1.6.0` | `tsdb-sigs.k8s.io-yaml` |
| [github.com/munnerz/goautoneg](https://pkg.go.dev/github.com/munnerz/goautoneg@v0.0.0-20191010083416-a7dc8b61c822) | `v0.0.0-20191010083416-a7dc8b61c822` | `tsdb-munnerz-goautoneg` |
| [go.yaml.in/yaml/v2](https://pkg.go.dev/go.yaml.in/yaml/v2@v2.4.4) | `v2.4.4` | `tsdb-go.yaml.in-yaml-v2` |
| [k8s.io/klog/v2](https://pkg.go.dev/k8s.io/klog/v2@v2.140.0) | `v2.140.0` | `tsdb-k8s.io-klog-v2` |
