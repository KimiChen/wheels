# 基础访问审计（可选，默认关闭）

回答「哪个计费身份在什么时候访问了哪个域名或 IP 的哪个端口」。

**不**回答 URL、Header、证书链；**不**提供抗篡改与抗抵赖；**不**保证事件不丢。
未配置时不创建任何 goroutine、文件或包装，数据面与未启用完全一致。

启用它是产品与合规决策，不因计量已上线而自动成立：开启即意味着节点上存在「用户 → 域名」明细。
它与计费链路完全独立——不进快照、不走 UDS、不参与差分与结算，两条链路的保留期、访问控制与
对外声明必须分别制定。

## 配置

```json
{
  "services": [
    {
      "type": "user_stats",
      "node_id": "node-example-01",
      "listen_path": "/run/sing-box-plus/user-stats.sock",
      "inbounds": ["vless-entry-01"],
      "access_log": {
        "directory": "/var/lib/sing-box-plus/access",
        "max_bytes": 16777216,
        "max_total_bytes": 2147483648,
        "flush_interval_ms": 1000,
        "queue_size": 8192,
        "max_open_files": 256,
        "exclude_hosts": ["google.com", "github.com"],
        "exclude_ips": ["198.18.0.0/15"]
      }
    }
  ]
}
```

域名维度需要在计费 inbound 上配 `action: "sniff"`；未配置时 `host_src` 恒为 `fqdn` 或 `ip`，
这是正常降级而非故障。

## 排除规则（`exclude_hosts` / `exclude_ips`）

### 它不影响计费

这是使用这两项之前必须先确认的一件事：**排除只挡审计，挡不住计费**。

`connState.countUplink` / `countDownlink` 里，计数器推进与配额扣减是无条件的，
审计只是其中 `if s.audit != nil` 的一条旁路。排除判断发生在 `newConnState`——
命中的连接压根不挂 audit，于是连每个数据块两次 `connUp/connDown` 原子加都省掉了。
被排除的流量照样计入快照、照样扣额度、照样触发闸断。

`TestExcludedTrafficStillBilled` 钉住这条分界线：走一次命中排除的真实往返，
断言审计零记录**且**快照计数器照常增长。这个用例存在的理由是，
如果有人图省事把排除判断塞进计数回调，计费会跟着少，而这种少是静默的，只有对账时才会发现。

### 写法

| 项 | 匹配对象 | 写法 |
|---|---|---|
| `exclude_hosts` | `host_src` 为 `sniff` 或 `fqdn` 的记录 | 域名后缀。`github.com` 命中 `github.com` 与 `api.github.com`，不命中 `evilgithub.com` 与 `raw.githubusercontent.com` |
| `exclude_ips` | `host_src` 为 `ip` 的记录 | CIDR 或裸地址（裸地址按 /32、/128） |

两项都在启动时编译，非法写法硬失败而不是静默忽略：IP 字面量写进 `exclude_hosts`、
域名写进 `exclude_ips`、网段带主机位（`198.18.7.1/15`）、重复项、空项，一律拒绝启动。
带主机位的网段之所以不静默 `Masked()`，是因为那几乎总是位数写错了，而静默取整会让生效范围
远大于作者以为的。

**匹配的是将要落盘的 `host` 字段，不是路由最终解析出的地址。** 嗅探到域名的连接永远走
`exclude_hosts`，即使它解析出来的 IP 就在 `exclude_ips` 的网段里；反过来，`exclude_ips`
只能命中那些压根没有域名可写的记录。这样规则与文件内容一一对应，读日志的人不必去猜当时的 DNS 结果。

### 排除是静默的，所以规则要跟着数据走

被挡掉的连接不留任何痕迹——不写 gap 行，因为那是**有意**的省略而不是丢失，
每挡一条写一行反而会把日志撑回去。

代价是光看文件无法区分「没人访问过」与「访问过但被规则挡了」，而配置在另一台机器的另一个文件里，
日志交付出去之后更无从对照。因此每次打开物理文件时写一行留痕，轮转后的新文件、重启后续写的文件
各自带一份：

```json
{"seq":1,"ev":"filter","hosts":["google.com"],"ips":["198.18.0.0/15"]}
```

它占 `seq`。跳过不占会让下游的连续性校验失效。

### 什么该排、什么不该排

先明确这份日志是干什么的。它的用途是**言论溯源**——出事时回答「谁在什么时候访问了哪里」。
它不是计费对账的依据，计费有自己的链路（快照 → 账本），两者独立。

按这个用途，下面两类排掉是合理的：

- **纯 CDN**：`githubusercontent.com`、`googleusercontent.com`、`twimg.com`、`googlevideo.com`、
  `cdn-apple.com`、`vscode-cdn.net`。它们只分发静态资源，没有发布入口，记下来也指向不了任何人的言论。
  注意 `googleusercontent.com` 与 `googleapis.com` 都**不在** `google.com` 之下，要单列——
  漏了它，实测每周仍有约 68 MiB 的 Google CDN 记录留在日志里。
- **需要登录态才能发言的平台**：`google.com`、`microsoft.com`、`apple.com`、`github.com`、
  `gstatic.com`。在这些站点上发布内容必须先登录，平台自己就持有作者身份；
  代理这一层再记一条「某人连过 github.com」并不增加任何溯源能力。

不该排的是匿名发布面——无需登录即可发帖、留言、上传的站点，以及未知的小站。
那正是这份日志唯一能提供、别处拿不到的信息。生产语料里 `codewiki.zcode-ai.com`
就属于后者：27 条 186 MiB，量级不小但不是已知服务，因此保留。

**同一家公司的其他顶级域够不到。** 后缀匹配就是后缀匹配，`google.com` 命中不了
`googleapis.com`、`google.com.hk`、`gvt2.com`、`doubleclick.net`，`apple.com` 命中不了
`icloud.com`，`github.com` 命中不了 `githubassets.com`。生产语料实测，只排主域会剩下
5348 条 / 357 MiB 的同族记录，全是 CDN、遥测、配置流与广告统计，没有一个是发布面。
配的时候要把兄弟域逐个列出来，`config/server.example.json` 里那份就是按这个口径展开的。

按用途看，被排掉的**字节**占比无关紧要（生产实测这批规则遮蔽了 77% 的字节，
因为 CDN 本来就是大头），真正要看的是有没有漏掉发布面。

### 副作用

审计字节与快照计数器原本能对上（生产实测 `kimi-ss` 审计 2.090 GiB vs 计数器 2.086 GiB，
差值即未关闭的会话），这是一条「计数没漏也没重」的独立交叉验证。配了排除之后这条校验失效，
只能靠账本自身的四项判据。这是启用排除必然付出的代价，不是缺陷。

## 逐计费身份一个文件

文件名是 `access-<base64url(身份名)>.jsonl`，落在 `directory` 下。目录必须**预先存在**——
属主与权限是部署决策，交给 tmpfiles / `StateDirectory=`，进程不会自己 `mkdir`，
否则会掩盖「部署漏配了」。

分文件的理由是**数据主体分离**,不是抗篡改。文件仍与数据面同 uid，
被攻破的数据面可以改写其中任意一个——分成多少个文件都一样。

### 按人交付 / 按人删除

```bash
# 某个人的文件是哪一个
python3 -c "import base64,sys;print('access-'+base64.urlsafe_b64encode(sys.argv[1].encode()).rstrip(b'=').decode()+'.jsonl')" '<身份名>'

# 反过来：目录里这个文件是谁的
python3 -c "import base64,sys;n=sys.argv[1][len('access-'):].split('.jsonl')[0];print(base64.urlsafe_b64decode(n+'='*(-len(n)%4)).decode())" 'access-dTE.jsonl'

# 交付某人的全部记录（含已轮转）
tar czf /tmp/<身份名>.tar.gz -C /var/lib/sing-box-plus/access "$(...)"*

# 删除某人的全部记录
rm -f /var/lib/sing-box-plus/access/access-<b64>.jsonl*
```

删除正在被写入的文件是安全的：writer 每个 flush tick 会用 `os.SameFile` 检测 inode 变化
并重开新文件（纪律 9，2026-09-15 在生产同机上实测：静止时 `rm` 活动文件，一个 tick 内
自动重建，续写的首个 `seq` 恰为删除前末个 `seq`+1）。

但**活动文件优先用 `mv` 而不是 `rm`**：`reopen()` 先 `flush` 再换 fd，所以 `rm` 之后、
检出之前落在缓冲里的记录会写进已 unlink 的 inode，随即消失；上界是
`flush_interval_ms` × 出记录速率，且因为数据已销毁而无法事后计量。
`mv` 没有这个问题——同样 100 conn/s 在途时实测跨文件零丢失（旧 156~459、新 460~605）。
要按人删历史，删的是该身份的**已轮转**文件。

### 丢弃了怎么知道

审计的任何丢弃都会置位快照的 `health.audit_dropped`（粘滞，重启才清），并在进程日志里
按至多每分钟一条的频率打出「访问审计丢弃：<原因>：<细节>；累计 N 次」。覆盖的路径有六条：
打开文件失败、写失败复位、重开失败、轮转重命名失败、无法统计目录（此时总量上限根本没在执行）、
以及纪律 10 触底而无已轮转文件可删。

两件事要分清：

- **`audit_dropped` 不影响入账。** 计费计数器与审计是两条独立链路，审计丢记录不代表数字不准。
  `/healthz` 因此**不看**这一位，采集端也照常入账。把它算进入账判据等于磁盘写满时连账一起停。
- **队列满不走这一位。** 队列满的丢弃在文件内就地写成 `ev=gap` 行，是自描述的，
  查文件就能看到缺了多少条；`audit_dropped` 专门对付连 gap 行都写不出去的那些路径。但**删除之后该身份的新访问会重新建文件**——
如果目的是停止记录某个人，删文件不够，要把他从 `user_stats.inbounds` 覆盖的 inbound 里移走，
或整体关掉 `access_log`。

### 几条与单文件时不同的地方

- `seq` 与队列丢弃的 gap 行按**身份**计，不是按文件：轮转时计数器不复位
  （生产实测跨轮转是 69833→69834），进程重启才从 1 重新开始，于是同一个文件里可能出现
  两段各自递增的 `seq`。下游查缺要按 `(user, run)` 分段，别拿整个文件当一条序列
- 打开文件数有上限（`max_open_files`，默认 256），超出按 LRU 关闭。身份数上限是
  `max_identities`（默认 4096），没有这个上限时 fd 与写缓冲会同时失控
- **从未产生过成功访问的身份不创建文件**——目录里出现某人的文件本身就是一条信息
- `max_bytes` 是**逐文件**的。实测每个身份约 1.4 MB/天，所以默认从 256 MiB 降到 16 MiB；
  沿用 256 MiB 会让一个身份半年才轮转一次，等于纪律 9/10 在生产上从不触发
- `max_total_bytes` 仍是**全局**上限：它保护的是磁盘，而磁盘是共享的。
  逐身份的保留期属于数据主体策略，做法是删掉那个人的文件，不走这条路径

## 记录形态

每条成功访问一行 JSONL：

```json
{"seq":10241,"ts":1787587200123,"node":"node-example-01","run":"0123456789abcdef0123456789abcdef","user":"u_example_01","in":"vless-entry-01","net":"tcp","host":"example.com","host_src":"sniff","port":443,"up":5120,"down":81920,"ms":2317}
```

| 字段 | 含义 |
| --- | --- |
| `seq` | writer 侧单调递增序号，进程内唯一。与快照的 `sequence` 是两个独立命名空间 |
| `ts` | 连接关闭时刻，Unix 毫秒，**墙钟**。时钟回拨时 `seq` 仍严格递增，下游按 `seq` 排序 |
| `node` / `run` | 与快照同源的 `node_id` 与 `runtime_id` |
| `user` / `in` | 计费身份名与 inbound tag |
| `net` | `tcp` / `udp` |
| `host` | `metadata.Domain` → `Destination.Fqdn` → `Destination.Addr` 三级降级；已归一化 |
| `host_src` | `sniff` / `fqdn` / `ip`，标明 `host` 的来源与可信度 |
| `port` | 目标端口 |
| `up` / `down` | 该连接四向计数中对应方向的字节数，**原样落盘**，成功判据交由读取方复核 |
| `ms` | 连接时长 |

`host` 的归一化到此为止：只做小写 + 去掉一个尾点，并截断到 255 字节、转为合法 UTF-8。
**没有独立的 `host_norm` 键**，归一化结果直接写进 `host`。不引入 IDNA/UTS #46 转换。

缺口用同一条流表达，不另开通道：

```json
{"seq":10242,"ev":"gap","after":10241,"n":37,"reason":"queue_full"}
```

边界不确定时 `n` 写 `null`，**不合成一个不存在的区间**。关闭时另写一条 `{"ev":"stop"}`。

## 落盘判据

成功判据是 **AND 不是 OR**：TCP 要求 `up > 0 && down > 0`，UDP 只要求 `up > 0`。
用 `up + down > 0` 会把端口扫描和被 RST 的 ClientHello 逐条渲染成「成功访问」，
而那正是审计要排除的情形。

载体标记地址不写记录（真实目标在子流上）：`sp.mux.sing-box.arpa`、`sp.v2.udp-over-tcp.arpa`、
`sp.udp-over-tcp.arpa`、`sp.packet-addr.v2fly.arpa`。

## 运维

- **不要给该目录配 logrotate。** 文件由进程按 `max_bytes` 自轮转，并在每个 flush tick 用
  `os.SameFile` 比对 inode；外部轮转会与它抢同一个文件，产生难以归因的丢记录。
- **总量封顶时删最老的已轮转文件并写一条 gap；无对象可删时只累加计数、不写 gap**，
  以免自指 gap 把真事件放大。
- 启动 fail-hard、运行 fail-open：启动期权限或属主不符即启动失败；进入运行期后，
  审计的任何写入错误、队列满或 panic 都不会阻断转发或触发进程退出。

## 已知缺口

| 来源 | 性质 | 处置 |
| --- | --- | --- |
| 崩溃丢失缓冲区内的记录 | 不做 `fsync`，且该丢失**不产生 gap 行**，事后无法与「本就没有访问」区分 | 需要不丢即需另建 spool（当前决策为不做） |
| 审计文件与数据面同 uid | 数据面被非 root 攻破即可就地改写或删除自己的历史 | `0600` 可挡本机其他非 root 账号；**同 uid 改写无廉价缓解** |
| `host` 来自 sniff 时可能是伪装外层 | 启用 ECH 的客户端给出的是伪装 SNI，属**错误记录**而非缺失 | `host_src` 标明来源，下游按可信度分级 |
| 未启用 sniff 或客户端直接给 IP | 只能记 IP，无域名 | 正常降级，`host_src` 为 `ip` |
| UDP 只到 association 粒度 | 记首包目标，一条 QUIC session 内的多目标不可见 | 需要逐包目标即需另立设计 |
| 不经 router 的伪装中继（REALITY / ShadowTLS） | tracker 不可见 | 声明不支持 |
