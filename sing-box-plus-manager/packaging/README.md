# packaging

主控与 `proxy-manager-agent` 各一套 systemd / sysusers / tmpfiles（README §5）。

**属于 M1（agent 与 PKI）与 M6（打包签名与运维手册），本目录目前只有这份说明。**
按 §8 的措辞纪律，没做的事不留一个看起来做好了的空壳。

## 落地时必须满足的三条

这三条都不是「部署细节」，它们是 §2.3 里编号约束在磁盘上的形态：

1. **两个 socket 权限档次不同，不许合并**（C18）。快照 socket 给 agent 只读组权限，
   配额 socket 给 agent 写权限。配到同一个组上等于抹掉这条分界线。
2. **主组 + 补充组缺一不可**（C28）。agent 要同时穿过两个不同权限档次的目录，
   缺任一项表现都是「服务起不来」。目录权限用 `0751` 父目录 + `0750` 分组隔离子目录——
   `0750` 的父目录会破坏穿越，`shadowsocks-rust-plus/packaging/*.tmpfiles` 的注释里
   写明了为什么。
3. **只访问本机 socket 的辅助 unit 必须 `PrivateTmp=false`**（C28）。
   私有 `/tmp` 会隐藏宿主机 socket，症状同样是「服务起不来」。

## 可直接参照的现成资产

| 资产 | 路径 | 用途 |
| --- | --- | --- |
| 权限分层 | `shadowsocks-rust-plus/packaging/*.sysusers`、`*.tmpfiles` | 分组隔离的目录权限，直接对应 C18 / C28 |
| 硬化 unit | `shadowsocks-rust-plus/packaging/*.service` | `ProtectSystem=strict`、`RestrictAddressFamilies=AF_UNIX` 等 |
| 工具链锁 + 离线签名 | `shadowsocks-rust-plus/packaging/release-toolchain.lock`、`scripts/sign-release.sh` / `verify-release.sh` | 锁到编译器 commit 哈希、fail-closed；detached 签名 + 独立公钥验签（M6） |

## 两条容易被当成「已经好了」的事

- **退出码不是健康证据**（C22）。服务是否在跑要靠 PID 文件 + 进程身份核验，
  不靠包装器的 `status`。unit 里写了 `Restart=` 不等于它真的在提供服务。
- **私钥不进 Git 必须是可执行门禁**，不是文档里的一句嘱咐（README §4.2 PKI 纪律 3）。
  本项目的那道门禁在 `sing-box-plus-manager/.gitignore`——同类系统正是在这里栽过：
  README 写明「密钥不得进 Git」，实际 CA 私钥已被跟踪。
