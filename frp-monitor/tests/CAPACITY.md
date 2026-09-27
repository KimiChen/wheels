# 有界 synthetic 容量验收

`capacity.go` 启动指定的真实 `frp-monitor-server`，每个 case 使用独立随机 loopback 端口、全新 SQLite、全新节点凭据和独立子进程；不连接已有 monitor 服务，也不读取生产配置。全部运行文件写在显式指定的 0700 目录下面，私密文件为 0600，路径不得包含 symlink。输出目录应放在 ignored 的 `.cache/`、`data/` 或仓库外；不要提交运行目录。

默认顺序测 100 和 500 个 **synthetic** 节点，每节点 1 report/s，全部握手完成后各测 60 秒，再留 5 秒处理尾部报告。握手以约 18/s 建立，遵循服务端 20/s admission 限速，500 节点建连约 28 秒。每个 case 同时运行一个 public SSE 消费者、每秒一次 public snapshot GET、每 5 秒一次 history GET。Synthetic metrics 中所有资源字段完整有效，但值由测试生成；这不代表 500 个真实 Linux 采集器，也不包含 TLS、FRP 隧道转发、TCP 探测、管理页面并发或长时间 retention 压力。

输出是无凭据的 JSON Lines：`measuring` 进度和每个 case 的结果。结果包括 socket 成功写入次数、退出后 SQLite 聚合中实际持久化报告总数和节点数、public API p50/p95/max、SSE 快照数、最终在线且 fresh 节点数、history storage 状态与 dropped 计数、Linux 被测子进程 CPU 秒和每秒采样的 RSS 峰值、harness 自身 RSS、SQLite/WAL/SHM 大小和 `integrity_check`。CPU 百分比以单个核为 100%；CPU 时间只测 60 秒负载阶段，RSS 为采样值。没有暴露内部 queue depth；`storage.dropped` 和最终写入/持久化总量差用于判断丢样，不能把 socket 写入数直接当作服务端确认数。

成功条件：预期报告数 = 成功 socket 写入数 = 实际持久化报告数，全部节点最终 fresh/online，连接/API/SSE 无错误，storage ready 且 dropped=0，SSE 至少收到测量秒数的一半快照，SQLite integrity ok。延迟和 CPU/RSS 输出用于比较，不宣称固定性能 SLA。服务端以 SIGTERM 优雅退出，5 秒仍未退出则强制清理并判失败。

## 编译

先用现有 pipeline 准备 P3 upstream module；不要与另一个 `prepare`/`test`/`build` 同时操作 `.cache/upstream/worktree`。在子项目根目录执行：

```bash
python3 scripts/frp.py prepare
project_root="$PWD"
cd .cache/upstream/worktree
env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=local GOWORK=off GOENV=off GOFLAGS= \
  GOCACHE="$project_root/.cache/go-build" GOMODCACHE="$project_root/.cache/go-mod" \
  go build -mod=readonly -trimpath -o "$project_root/.cache/capacity-linux-amd64" \
  "$project_root/tests/capacity.go"
```

`GOARCH=arm64` 可生成对应架构工具。构建依赖沿用锁定的 upstream module，不新增依赖。普通本机小规模验证可省略 GOOS/GOARCH；macOS 不提供 `/proc`，其 CPU/RSS 显式标为 unavailable。Linux 缺少可读取的进程数据则失败。

## 在隔离测试机运行

下面路径均为示例；`/opt/frp-monitor/bin/frp-monitor-server` 应是已核验的发行二进制。输出目录必须为专用测试目录，不能指定正式服务目录。主机需为 systemd+cgroup 环境：

```bash
install -d -m 0700 /var/lib/frp-monitor-capacity
systemd-run --unit=frp-monitor-capacity --wait --collect --pipe \
  --property=CPUQuota=200% --property=MemoryMax=1G --property=TasksMax=256 \
  --property=RuntimeMaxSec=330 --property=TimeoutStopSec=10 --property=KillMode=control-group \
  --property=LimitNOFILE=4096 --property=UMask=0077 \
  /opt/frp-monitor-test-tools/capacity-linux-amd64 \
  -server /opt/frp-monitor/bin/frp-monitor-server \
  -directory /var/lib/frp-monitor-capacity \
  -nodes 100,500 -seconds 60 -grace 5 -max-seconds 300 \
  -gomaxprocs 2 -max-rss-mib 768 -max-cpu-seconds 180
```

`MemoryMax` 和 `CPUQuota` 限制整个临时测试 cgroup（harness 与独立 server 合计）；`GOMAXPROCS` 只控制 Go 并行度，不是硬 CPU 上限。harness 每秒检查 server RSS 和累计 CPU 时间，超过预算即结束；总超时取消测试并清理子进程，清理可再耗时约 10 秒，systemd 的 RuntimeMaxSec 为外部硬期限。100/500 两组通常约 165–190 秒；在繁忙测试机上不要通过提高限额掩盖失败，应保留报告并说明环境。

先运行 `-nodes 3 -seconds 3 -grace 2 -max-seconds 30` 可检查工具和发行包。默认 case 通过仅说明该机器上这一短时、受控 synthetic 负载通过；长期容量、多个浏览器 SSE、真实采集、TLS 与转发并发应分别验收。
