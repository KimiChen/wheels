# packaging

部署模板。三点与上游不同，安装前请逐条确认：

1. **两个 UDS 都在 `/run/sing-box-plus/` 下**，由 `RuntimeDirectory=` 创建。
   快照 socket 是只读旁观者，配额 socket 能改变转发行为——两者权限档次不同，
   不要为了省事把它们放进同一个对外可达的目录，也不要把任何一个直接映射为公网监听。
2. **`Restart=on-failure` 是必需的**：exporter 意外退出、panic 或连续 accept 失败时，
   进程会主动失败退出（README §4.6 第 5 条），靠 systemd 拉起才能恢复。
   如果改成 `Restart=no`，故障表现会变成「代理静默停摆」。
3. **不要给 `/var/lib/sing-box-plus/` 配 logrotate**：访问审计文件由进程按 `max_bytes`
   自轮转，并在每个 flush tick 用 `os.SameFile` 检测 inode 变化（README §4.8 纪律 9）。
   外部 logrotate 会与它抢同一个文件，产生难以归因的丢记录。

升级与重启的顺序见 `docs/OPERATIONS.md`，不要直接 `systemctl restart`——
那会跳过 §5.3 的排空与最终结算，留下未闭合窗口。
