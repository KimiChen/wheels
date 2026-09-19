#!/bin/sh
# proxy-manager 的一致性备份（README §10 的 R3）。
#
# 由 proxy-manager-backup.timer 调起，**以服务账号身份运行**。
# 不写 /root/backup（本仓库运维约定的默认备份目录）是有具体理由的：
# 那个目录只有 root 写得动，而让 root 去打开一个 WAL 库，
# 会在服务停着的时候创建 **root 拥有的 `-wal` / `-shm`**——
# 之后服务以 proxy-manager 身份起来就再也写不进去了。
# 一个每天自动跑的任务不该带着这种地雷。手工的、动手术之前的那种备份仍然放 /root/backup。
set -eu

DB_CONFIG=${DB_CONFIG:-/etc/proxy-manager}
DEST=${DEST:-/var/backups/proxy-manager}
# 留几份，以及**总量封顶**（字节）。
#
# 只按份数留是不够的：库在长（`quota_requests.request_body` 实测 3.4 GB/月，
# 而且它没有读者，留着纯粹是为了出问题时能复盘），14 份全量副本是 15 倍放大，
# 按当时的速率**七周就能把根分区填满**——而那个分区上还跑着 nginx、MySQL
# 与 paoyou-ssserver。满盘的后果远大于「主控挂了」。
#
# 所以以总量为准、份数为辅：先按份数砍，再按总量从最旧的往下砍，至少留一份。
KEEP=${KEEP:-14}
BUDGET=${BUDGET:-21474836480}   # 20 GiB

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT="$DEST/proxy-manager.$STAMP.db"

mkdir -p "$DEST"
chmod 700 "$DEST"

# `ledger backup` 自己会在写完之后核对一遍：总账必须能从账本重算出来。
# **核对不过就退出码非 0**，于是这一次备份在 systemd 里是失败的——
# 一份没被核对过的备份，与没有备份的区别只在你自以为有。
/usr/local/bin/proxy-manager ledger backup --config-dir "$DB_CONFIG" --out "$OUT"

# 只有核对通过才会走到这里。先按份数砍。
ls -1t "$DEST"/proxy-manager.*.db 2>/dev/null | tail -n "+$((KEEP + 1))" | while read -r old; do
    echo "清掉旧备份（超出份数）$old"
    rm -f -- "$old"
done

# 再按总量砍，从最旧的开始。**至少留一份**——一个把最后一份也删掉的清理器，
# 会在磁盘最紧张的那天把唯一的备份也拿走。
while :; do
    total=$(du -sb "$DEST" 2>/dev/null | cut -f1)
    count=$(ls -1 "$DEST"/proxy-manager.*.db 2>/dev/null | wc -l)
    [ "${total:-0}" -le "$BUDGET" ] && break
    [ "$count" -le 1 ] && {
        echo "只剩一份备份，总量 $total 仍超 $BUDGET —— 不再删，请人看一眼"
        break
    }
    oldest=$(ls -1t "$DEST"/proxy-manager.*.db | tail -1)
    echo "清掉旧备份（总量 $total > $BUDGET）$oldest"
    rm -f -- "$oldest"
done

echo "备份完成：$OUT"
