#!/usr/bin/env python3
"""与节点侧**参考实现**的账本逐桶对账。

两个采集器可以同时采同一个节点——各持各的账本、各走各的单元，
`sequence` 跳号本来就合法（C26）。**但配额下发不能两边都做**，那是 epoch 冲突的来源。
节点侧运维文档写死了另一半：两者不得同时对同一个账本写入。

对账为什么能做到**精确**而不是「看着差不多」：两边都是对同一组累计计数器做差分，
所以「从序号 A 到序号 B 的增量之和」恒等于 `C(B) − C(A)`，与中间采了几次无关。
于是有四项断言，全部不需要估算：

1. **字节守恒**（各自内部）：`Σ 增量 == 末次绝对值 − 首次绝对值`。
   两端的绝对值从**原始快照字节**里读，不用自己的记账反推——否则就是自证。
2. **夹逼**：参考在我方窗口**内侧**的合计 ≤ 我方合计 ≤ **外侧**的合计。
3. **反向夹逼**：用我方的密采样去夹住参考侧报出的那个数。
   采集间隔更短的一方夹得更紧；带宽只来自两端各一个序号的间隙，
   而那点间隙**消不掉**——每次 GET 都消耗一个序号，两边不可能采到同一个。
4. **轨迹交错**：两边重建出的绝对值按序号归并后必须单调不减。
   这一条最强——它说明两边认的是**同一条计数器轨迹**。

用法：
    reconcile-with-reference.py <我方.db> <参考ledger.jsonl> <参考state.json> <runtime_id>

参考侧的两个文件在节点上，路径见节点仓库 `docs/OPERATIONS.md`（带 `reference` 的那一组）。
本脚本只读，不改任何一边。
"""

from __future__ import annotations
import json, sqlite3, sys
from pathlib import Path

FIELDS = ("tcp_uplink_bytes", "tcp_downlink_bytes", "udp_uplink_bytes", "udp_downlink_bytes")


def zero(): return {f: 0 for f in FIELDS}
def add(a, b): return {f: a[f] + b[f] for f in FIELDS}
def sub(a, b): return {f: a[f] - b[f] for f in FIELDS}
def total(c): return sum(c[f] for f in FIELDS)


def our_trajectory(db_path: str):
    """我方：从原始快照字节读两端绝对值，从账本读逐次增量。"""
    db = sqlite3.connect(db_path)
    batches = db.execute(
        "SELECT b.batch_pk, CAST(b.sequence AS INTEGER), p.payload FROM snapshot_batches b "
        "JOIN snapshot_payloads p ON p.batch_pk=b.batch_pk "
        "WHERE b.status='applied' ORDER BY b.sequence"
    ).fetchall()
    if len(batches) < 2:
        raise SystemExit(f"我方只有 {len(batches)} 个已入账批次，不足以对账")

    def absolutes(payload):
        snap = json.loads(payload)
        out = {}
        for ib in snap["inbounds"]:
            for u in ib["users"]:
                out[(ib["tag"], u["name"])] = {f: u[f] for f in FIELDS}
        return out

    first_abs = absolutes(batches[0][2])
    last_abs = absolutes(batches[-1][2])

    deltas = {}
    for pk, seq, _ in batches:
        for tag, name, *vals in db.execute(
            "SELECT s.inbound_tag, i.identity_name, "
            "CAST(l.tcp_uplink_bytes AS INTEGER), CAST(l.tcp_downlink_bytes AS INTEGER), "
            "CAST(l.udp_uplink_bytes AS INTEGER), CAST(l.udp_downlink_bytes AS INTEGER) "
            "FROM usage_ledger l JOIN runtime_identities i ON i.runtime_identity_id=l.runtime_identity_id "
            "JOIN runtime_services s ON s.runtime_service_id=i.runtime_service_id "
            "WHERE l.batch_pk=?", (pk,)
        ):
            deltas.setdefault((tag, name), []).append((seq, dict(zip(FIELDS, vals))))
    seqs = [seq for _, seq, _ in batches]
    return seqs, first_abs, last_abs, deltas


def ref_trajectory(ledger_path: str, state_path: str, runtime_id: str):
    """参考侧：从 state.json 的末次绝对值往回走，重建每个序号上的绝对值。"""
    state = json.load(open(state_path))
    run = next(r for r in state["runtimes"] if r["runtime_id"] == runtime_id)
    last_seq = run["last_sequence"]
    absolute = {}
    for b in run["baselines"]:
        _, tag, _, name, _, _ = b["key"]
        absolute[(tag, name)] = dict(b["counters"])

    records = []
    for line in open(ledger_path):
        d = json.loads(line)
        if "sequence" not in d or d.get("runtime_id") != runtime_id:
            continue
        if d.get("result") != "accepted":
            continue
        per = {}
        for e in d["entries"]:
            _, tag, _, name, _, _ = e["key"]
            per[(tag, name)] = {f: e["delta"][f] for f in FIELDS}
        records.append((d["sequence"], per))
    records.sort()

    # 从末次往回：C(前一个序号) = C(本序号) − 本次增量
    traj = {key: {} for key in absolute}
    cur = {k: dict(v) for k, v in absolute.items()}
    for key in traj:
        traj[key][last_seq] = dict(cur[key])
    for seq, per in reversed(records):
        if seq > last_seq:
            continue
        prev_seq = None
        for s, _ in records:
            if s < seq and (prev_seq is None or s > prev_seq):
                prev_seq = s
        for key in traj:
            d = per.get(key, zero())
            cur[key] = sub(cur[key], d)
            if prev_seq is not None:
                traj[key][prev_seq] = dict(cur[key])
    return traj, records


def main():
    db_path, ledger_path, state_path, runtime_id = sys.argv[1:5]
    our_seqs, first_abs, last_abs, our_deltas = our_trajectory(db_path)
    ref_traj, ref_records = ref_trajectory(ledger_path, state_path, runtime_id)

    o1, om = our_seqs[0], our_seqs[-1]
    print(f"我方窗口：sequence {o1} → {om}（{len(our_seqs)} 个已入账批次）")
    ref_in_window = [s for s, _ in ref_records if o1 <= s <= om]
    print(f"参考侧在同窗口内的批次：{len(ref_in_window)} 个，序号 {ref_in_window[:3]}…{ref_in_window[-3:] if len(ref_in_window)>3 else ''}")
    print()

    ok = True
    for key in sorted(set(first_abs) & set(ref_traj), key=str):
        tag, name = key
        label = f"{tag}/<user>"
        ours_sum = zero()
        for _, d in our_deltas.get(key, []):
            ours_sum = add(ours_sum, d)
        span = sub(last_abs[key], first_abs[key])

        print(f"[{label}]")
        # 1) 字节守恒：两端绝对值来自原始快照字节，不是自己的记账
        conserved = ours_sum == span
        print(f"  字节守恒  Σ增量={total(ours_sum):>12,}  末−首={total(span):>12,}  {'一致' if conserved else '不一致 ✗'}")
        ok &= conserved

        # 2) 夹逼
        ref_seqs = sorted(ref_traj[key])
        before = [s for s in ref_seqs if s <= o1]
        after = [s for s in ref_seqs if s >= om]
        inner_lo = [s for s in ref_seqs if s >= o1]
        inner_hi = [s for s in ref_seqs if s <= om]
        if before and after and inner_lo and inner_hi and inner_lo[0] <= inner_hi[-1]:
            outer = total(sub(ref_traj[key][after[0]], ref_traj[key][before[-1]]))
            inner = total(sub(ref_traj[key][inner_hi[-1]], ref_traj[key][inner_lo[0]]))
            sandwiched = inner <= total(ours_sum) <= outer
            print(f"  夹  逼  参考内侧={inner:>12,} ≤ 我方={total(ours_sum):>12,} ≤ 参考外侧={outer:>12,}  {'成立' if sandwiched else '不成立 ✗'}")
            ok &= sandwiched
        else:
            print("  夹  逼  参考侧样本不足以夹住我方窗口，跳过")

        # 2b) 反向夹逼：用**我方**的密采样去夹住参考侧报出的那个数。
        #     我方 15 秒一采、参考侧 60 秒一采，所以这个方向的夹逼紧得多。
        our_abs = {o1: dict(first_abs[key])}
        running = dict(first_abs[key])
        for seq, d in our_deltas.get(key, []):
            running = add(running, d)
            our_abs[seq] = dict(running)
        # 我方在每个采样点的绝对值；未产生账本行的序号沿用上一个值（零增量）
        for s in our_seqs:
            if s not in our_abs:
                prior = max((x for x in our_abs if x < s), default=o1)
                our_abs[s] = dict(our_abs[prior])

        inside = [s for s in ref_seqs if o1 <= s <= om]
        if len(inside) >= 2:
            ra, rb = inside[0], inside[-1]
            ref_value = total(sub(ref_traj[key][rb], ref_traj[key][ra]))
            lo_hi = max((s for s in our_seqs if s <= rb), default=None)
            lo_lo = min((s for s in our_seqs if s >= ra), default=None)
            up_hi = min((s for s in our_seqs if s >= rb), default=None)
            up_lo = max((s for s in our_seqs if s <= ra), default=None)
            if None not in (lo_hi, lo_lo, up_hi, up_lo):
                lower = total(sub(our_abs[lo_hi], our_abs[lo_lo]))
                upper = total(sub(our_abs[up_hi], our_abs[up_lo]))
                held = lower <= ref_value <= upper
                width = upper - lower
                rel = (width / ref_value * 100) if ref_value else 0.0
                print(f"  反向夹逼  我方 [{lower:,} , {upper:,}] 夹住参考侧 [{ra},{rb}] 的 {ref_value:,}  {'成立' if held else '不成立 ✗'}")
                print(f"            夹逼带宽 {width:,} 字节（参考值的 {rel:.2f}%），来自两端各一个序号的间隙")
                ok &= held

        # 3) 轨迹交错：两边重建的绝对值按序号归并后必须单调不减
        merged = []
        running = dict(first_abs[key])
        merged.append((o1, total(running), "我"))
        for seq, d in our_deltas.get(key, []):
            running = add(running, d)
            merged.append((seq, total(running), "我"))
        for s in ref_seqs:
            if o1 <= s <= om:
                merged.append((s, total(ref_traj[key][s]), "参"))
        merged.sort()
        violations = [
            (merged[i], merged[i + 1])
            for i in range(len(merged) - 1)
            if merged[i][1] > merged[i + 1][1]
        ]
        print(f"  轨迹交错  {len(merged)} 个采样点归并后单调不减：{'成立' if not violations else f'{len(violations)} 处违反 ✗'}")
        if violations:
            print(f"    首处违反：{violations[0]}")
        ok &= not violations
        print()

    print("=" * 60)
    print("对账结论：" + ("三项全部成立。" if ok else "**有不成立项。**"))
    print("这证明的是两边认的是同一条计数器轨迹、且各自没有重复或漏记；")
    print("它**不**证明「四向拆分口径一致」——那需要按方向逐项比，而两边口径本就相同。")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
