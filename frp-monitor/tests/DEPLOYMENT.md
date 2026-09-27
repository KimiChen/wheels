# Linux amd64 测试部署记录

日期：2026-09-27。部署内容为 `0.3.0-p3`，运行二进制来自提交 `e7d22cb` 的
Linux amd64 发布包，上游 FRP `v0.71.0`，包内外 SHA256 均已核验。
测试机为 Ubuntu 26.04.1 LTS、systemd 259、18 个逻辑 CPU、约 31 GiB 内存，
同时承载其他服务。地址、SSH信息、节点身份、证书、凭据及机器原始数据只留在
ignored 的本地 `.env` / `data/` 和远端私有目录，本文只记录脱敏结果。

## 持续运行的测试服务

server 与 agent 分别使用独立无登录系统用户，安装在 root 持有的发布目录，运行
目录 0700、私有文件 0600。systemd 两个 unit 均 enabled/active；agent 无法读取
server 管理凭据。管理/采集走校验证书的 HTTPS/WSS，独立随机管理员、监控与 FRP token。
测试证书为自签名且只由本次测试信任，未改变系统信任库。服务绑定测试机指定的局域网IP，
代理出口限制回环，不改变原有防火墙、Web站点或其他服务配置。

节点在线、新鲜、FRP可信绑定匹配，SQLite storage ready、dropped=0；启用每5秒一次的
FRP控制端口TCP探测。没有持久开启示例Proxy；真实转发由临时回环回归覆盖。
agent指标范围仍遵循采集器保守的 `unknown` 语义；独立检查确认 agent 的 mount/net
namespace 与PID 1相同，未通过增加root权限来改变此字段。

## 功能与恢复验收

- 15秒同机Linux采样取得10个不同样本，16个公开指标通过；`free` 内存/交换总量、
  按同一挂载筛选规则的 `df` 总量/used 对照通过。时间容差见 `linux_acceptance.py`。
- P0 wire v1/v2、P1 WSS/认证/故障隔离、P2探测/历史/降级、P3管理/绑定/凭据生命周期
  的真实二进制 smoke 全部通过。
- 从固定上游commit独立构建未加补丁的原版 frpc/frps，增强→增强、原版→增强、
  增强→原版各跑 wire v1/v2，六组 TCP/UDP/HTTP/STCP payload 矩阵全部通过。
- 正常 TERM 停 server 退出码0，尾批历史持久化；启动后同一agent自动重新上报并对账匹配。
- 分别 SIGKILL server 与 agent，systemd 自动重启，PID变化、重启计数递增；节点重连，
  已提交历史保留，SQLite `integrity_check` 通过。崩溃前未提交的当前分钟不承诺保留。
- 对活跃SQLite在线备份并恢复到新的0700目录；凭据/绑定/证书保持一致，路径重写、权限
  和原生配置verify通过。停原服务后切到恢复目录，真实启动且历史可查；演练后切回原目录，
  最终服务在线，storage ready。原目录和私有备份保留，恢复实例已经停止。

独立功能回归用 transient systemd service 限制 CPU、内存与运行时间，并已回收；
没有遗留测试进程或临时转发端口。恢复演练仅操作本次新建的两个测试unit。

## 短时 synthetic 容量

见 [运行方法与边界](CAPACITY.md)。每组全新monitor进程/数据库/回环端口，按真实协议
以每节点1 report/s上报60秒；另有1个SSE消费者、每秒1次公开GET、每5秒1次历史GET。
建连受20/s握手限速，建连时间不计入60秒测量；退出后逐项核对发送和持久化总量。
整个临时cgroup限制2个CPU核与1GiB内存，server/harness分别 `GOMAXPROCS=2`。

| 节点数 | 报告持久化 | API p95 | API最大延迟 | server平均CPU（单核） | server采样RSS峰值 | 最终DB |
|---|---|---|---|---|---|---|
| 100 | 6,000/6,000 | 4.680 ms | 5.387 ms | 10.2% | 50.6 MiB | 0.98 MiB |
| 500 | 30,000/30,000 | 11.735 ms | 15.749 ms | 31.4% | 90.8 MiB | 4.66 MiB |

两组节点均全部在线/新鲜，socket写入、连接、API与SSE错误均为0，storage ready、
dropped=0，SQLite integrity为ok。500节点case耗时约94秒（含建连/结束），两组总耗时
约165秒，整个临时cgroup记账内存峰值106.4 MiB；执行后测试unit和子进程已退出。

这些结果只证明当前测试机上的短时 synthetic 场景；无TLS、真实采集、隧道转发或探测负载，
也不是多浏览器、慢盘、长时保留期或生产容量承诺。队列深度未直接暴露，以dropped、
API/SSE错误及最终数据库样本数共同检查丢样。CPU以单个核100%表示；RSS为每秒采样峰值，
不等同于cgroup记账峰值。容量工具和真实采样验收工具无新增第三方依赖。

## 尚未覆盖

Linux arm64实机、多节点真实TLS/探测/转发混合负载、长时运行及retention清理、
慢盘和更多认证/Proxy类型；现有单台amd64验收不能替代这些范围。保留测试部署供后续使用，
本轮没有安排周期任务、自动升级或自动更改其他服务。
