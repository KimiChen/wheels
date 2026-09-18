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

---

## 2026-09-19：本目录不再只有这份说明

六个文件已落地并在生产用过一次：

| 文件 | 去处 | 备注 |
| --- | --- | --- |
| `proxy-manager.{sysusers,tmpfiles,service}` | 入口机 `172.18.90.102` | 主控，绑 `127.0.0.1:8173` |
| `proxy-manager-agent.{sysusers,tmpfiles,service}` | 四台 SS 终端 | **agent 当前跑 root**（D31），`.sysusers` 是降权用的，暂未启用 |

三条不许违反的纪律在 unit 里都有对应的长注释，因为它们的违反症状**都是「服务起不来」**，
而那个症状指不向任何一条：

- **C18**：两个 socket 权限档次不许合并到同一个组。当前靠 root 绕过（D31），不是靠满足它。
- **C28 主组 + 补充组**：`proxy-manager-agent.sysusers` 里 `m proxy-manager-agent sbpd`
  那一行就是补充组。注意 `sbpd` 的 gid **四台各不相同**（999/994/995/996），
  所以只能加入、不能创建。
- **C28 `PrivateTmp=false`**：agent unit 里显式写了。开了它，私有 `/tmp`
  会把宿主机的 socket 藏起来，而那和权限不足长得一模一样。

还有一条不在 C 编号里、但同样会连坐数据面的：**agent unit 绝不能声明
`RuntimeDirectory=sing-box-plus-deploy`**。plus 的 unit 是 `RuntimeDirectoryPreserve=no`，
那个目录的生命周期绑在它身上；agent 声明同名目录会抢走属主，
并在 agent 停止时把目录删掉。

部署脚本在 `operations` 仓 `docs/proxy.paoyou.work/scripts/`：
`install-agent.py`（节点）、`deploy-manager.py`（主控）、`switch-site.py`（反代切换）。
三个分开不是洁癖——它们的失败模式完全不同，装错了是一个起不来的服务，切错了是整站 502。
