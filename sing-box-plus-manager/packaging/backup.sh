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
KEEP=${KEEP:-14}

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT="$DEST/proxy-manager.$STAMP.db"

mkdir -p "$DEST"
chmod 700 "$DEST"

# `ledger backup` 自己会在写完之后核对一遍：总账必须能从账本重算出来。
# **核对不过就退出码非 0**，于是这一次备份在 systemd 里是失败的——
# 一份没被核对过的备份，与没有备份的区别只在你自以为有。
/usr/local/bin/proxy-manager ledger backup --config-dir "$DB_CONFIG" --out "$OUT"

# 只有核对通过才会走到这里。旧的按份数留，**从最新往回数**。
ls -1t "$DEST"/proxy-manager.*.db 2>/dev/null | tail -n "+$((KEEP + 1))" | while read -r old; do
    echo "清掉旧备份 $old"
    rm -f -- "$old"
done

echo "备份完成：$OUT"
