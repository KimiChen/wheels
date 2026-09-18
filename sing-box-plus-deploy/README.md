# sing-box-plus-deploy

用 Python 标准库把私有主机清单渲染为独立的 GOST、nftables 和 systemd 配置。用于让新单跳转发、转发链与现有代理并行运行。

当前只提供本地配置生成，不连接服务器，不下载二进制，不创建证书，不管理 sing-box 用户、凭据或订阅。示例只使用文档保留地址和 example 域名；真实 inventory、证书、密钥和输出应留在私有运维仓库。

## 使用

要求 Python 3.9 或更高版本，无第三方 Python 依赖。

~~~bash
python3 sbpd.py render \
  --inventory examples/inventory.example.json \
  --output output/example
python3 -m unittest discover -s tests -v
~~~

输出目录必须不存在或为空。渲染前校验整个清单，发现错误时不写入文件。生成目录按节点分组：

~~~text
<node>/
  etc/sing-box-plus-deploy/
    gost.json
    sbpd_nat.nft
    apply-nat.sh
  etc/systemd/system/
    sing-box-plus-deploy-gost.service
    sing-box-plus-deploy-nat.service
~~~

每个节点只生成它需要的文件。渲染产物默认仅当前用户可读，shell 脚本仅当前用户可执行。执行器安装到远端时需另行设置服务账户的读取权限。

## 清单 v1

参考 [完整示例](examples/inventory.example.json)。顶层包含 version、nodes，可选 metadata。nodes 是节点名称到配置对象的映射，每个节点支持 listeners、reserved_listeners、nat 和 metadata。metadata 不参与渲染，可供私有执行器保存 SSH 地址等信息。

除 metadata 外，未知字段会被拒绝，避免拼错配置字段或误传旧服务名称。

### GOST 监听器

listeners 中每项包含：

| 字段 | 含义 |
| --- | --- |
| name | 本节点内唯一的名称，只允许字母、数字、下划线及连字符 |
| mode | forward 或 relay |
| listen | host 为 IPv4 字面量；port 为 1–65535 的整数 |
| protocols | forward 使用 tcp、udp 或两者；relay 必须为仅含 tcp 的列表 |
| allowed_sources | 来源 IPv4 地址或 CIDR 的白名单；公网 forward 及所有 relay 必填 |
| target | 固定 host、port；relay 必填，forward 有 chain 时可省略 |
| chain | 下一跳 host、port，以及 tls.ca_file、tls.server_name |
| tls | relay 的 cert_file、key_file；forward 不设置此字段 |

TLS 文件必须位于 /etc/sing-box-plus-deploy/ 内。下一跳固定启用服务器证书验证，CA 和服务器名称由 inventory 明确指定。不会生成关闭校验的配置。

公网链首使用 forward，分别生成 TCP 和 UDP 服务；两者共用 relay+mtls 链。远端 relay 的固定 target 决定实际目标，客户端入口可省略 target。MTLS 在 GOST 中表示多路复用 TLS，底层 TCP 同时承载 Relay 的 TCP 和 UDP 数据；它不表示强制使用客户端证书的双向 TLS。

中间节点包含两类监听器：

1. relay+mtls 监听器的 target 指向本机独立的回环桥接端口。
2. 同一回环端口上各有 TCP、UDP forward 监听器，经 chain 连接下一跳 relay。

终端 relay 的 target 指向本机 sing-box 监听器。需要同时处理直接访问和回环访问时，sing-box 应绑定 0.0.0.0；其配置和权限由外部执行器管理。

reserved_listeners 与 listen 类似，但字段直接为 host、port、protocols。把已有服务和不由本工具生成的 sing-box 监听放入其中，可以检测新监听与它们的重叠。检查区分 TCP/UDP，但将 0.0.0.0 与同端口的任何 IPv4 地址视为重叠。该检查只覆盖 inventory 中声明的情况，部署前仍需检查服务器实际监听和已有 NAT 规则。

### 单跳 NAT

nat 包含：

| 字段 | 含义 |
| --- | --- |
| ingress_interface | 收到用户连接的实际网卡，例如 eth0 |
| source_nat | mode 为 masquerade，或 mode 为 snat 并提供 address |
| forwards | 每项包含 name、listen_port、protocols，以及 target.host、target.port |

入口规则要求数据包从指定网卡进入，且目的地址属于本机，再按协议和入口端口 DNAT。回程 SNAT 同时匹配入口网卡、DNAT 状态、协议、原始入口端口及转换后的目标地址/端口；复用已有目标服务不会因此匹配旧入口的回程。

规则固定使用独立的 ip sbpd_nat 表。prerouting/postrouting 优先级为 -101/99，先于常规 -100/100 NAT 链运行，但仅匹配清单声明的新入口。规则不改变过滤链策略；主机及云防火墙需允许相关流量，控制机需要 net.ipv4.ip_forward=1。

apply-nat.sh 使用 flock 串行化更新，先对事务执行 nft --check，再用一次 nft 调用原子删除并重建自己的表。它从不执行 flush ruleset，也不修改其他表。加载器依赖 Linux 的 nft、flock、mktemp 和 unlink。

## 与旧系统并行

本工具固定使用以下独立资源：

| 资源 | 名称或路径 |
| --- | --- |
| GOST 二进制 | /opt/sing-box-plus-deploy/bin/gost |
| 配置与 TLS | /etc/sing-box-plus-deploy/ |
| GOST 服务 | sing-box-plus-deploy-gost.service |
| NAT 服务 | sing-box-plus-deploy-nat.service |
| nftables 表 | ip sbpd_nat |
| GOST 运行账户 | sbpd:sbpd |

GOST 服务有只读文件系统等 systemd 限制，MemoryHigh=96M、MemoryMax=160M。超过硬限制可能被停止并自动重启，实际流量需要更大预算时应先评估主机内存。端口低于 1024 在默认非特权账户下通常不可绑定，生产清单应使用高端口。

安装执行器应先创建 sbpd 系统账户，安装并校验 GOST 二进制；配置及证书可设为 root:sbpd 0640、目录 0750，私钥只给该账户所需的读取权限。NAT 加载器必须由 root 执行。新配置、服务、端口和回环桥接均需独立，不替换旧系统文件或二进制。

NAT 更新应执行新服务的 reload。停止 NAT 的 oneshot 服务会保留规则；有意撤销时只删除 ip sbpd_nat，并单独禁用新 NAT 服务。停止新 GOST 服务也只影响新链。服务不包含操作旧服务的依赖、停止命令或清理命令。

此工具不会检查证书有效期、服务器端口实际占用、云防火墙、既有 NAT 端口或端到端连通性。部署执行器必须在应用前检查这些状态，并保存备份；应用后分别验证新链 TCP/UDP 和旧入口仍可用。

## 验证与参考

单元测试覆盖端口重叠、旧监听保留、未知字段、TLS 验证、固定目标 Relay、NAT 规则范围、输出目录隔离和失败前不写入文件。示例清单包含单跳和带回环桥接的多节点转发链。

GOST 官方文档确认支持 [JSON 配置](https://gost.run/en/getting-started/configuration-overview/)，并提供 [Relay 固定目标转发](https://gost.run/en/tutorials/protocols/relay/)、[来源白名单](https://gost.run/en/concepts/admission/) 和 [MTLS 通道](https://gost.run/en/reference/dialers/mtls/) 说明。
