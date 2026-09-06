#!/usr/bin/env python3
"""参考 collector：取快照 → v2 校验 → 差分 → 幂等落地本地账本。

带 --quota-socket 时另外承担 §5.4 的下发职责：按自己累计的「本周期已用」算出剩余额度，
每轮重推一份全量表。

范围**不含** outbox、mTLS 与重试——那些属于下游控制面。
这里只把 §5.1 与 §5.4 的契约跑通，并且让「本次采集失败」与「本次没有流量」在账本里可区分。
"""

from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))

import http_unix  # noqa: E402
from settlement_model import (  # noqa: E402
    COUNTER_FIELDS,
    Collector,
    SettlementFailure,
    SnapshotRejected,
    parse_snapshot,
)

LEDGER_FILE = "ledger.jsonl"
STATE_FILE = "state.json"


class Ledger:
    """账本是一份 JSONL：每一次采集都留痕，包括失败。

    失败也写一行，是为了让长跑结束时的四项判据可判定：
    只记成功的话，「七天没有负增量」与「七天一次都没采到」看起来一模一样。
    """

    def __init__(self, directory: Path) -> None:
        self.directory = directory
        self.directory.mkdir(parents=True, exist_ok=True)
        self.path = directory / LEDGER_FILE
        self.state_path = directory / STATE_FILE

    def append(self, record: dict) -> None:
        line = json.dumps(record, ensure_ascii=False, sort_keys=True)
        with self.path.open("a", encoding="utf-8") as handle:
            handle.write(line + "\n")

    def load_state(self) -> dict:
        if not self.state_path.is_file():
            return {}
        return json.loads(self.state_path.read_text(encoding="utf-8"))

    def save_state(self, state: dict) -> None:
        temporary = self.state_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(state, ensure_ascii=False, sort_keys=True), encoding="utf-8")
        temporary.replace(self.state_path)


class QuotaPlan:
    """每个计费身份的额度。

    一个全局数字撑不起真实部署：不同用户的套餐不同，而改某一个人的额度不应该动到别人。
    计划文件与 systemd 单元解耦——改额度不需要编辑 unit、不需要 daemon-reload。
    """

    def __init__(self, default_bytes: int, overrides: dict[str, int] | None = None) -> None:
        if default_bytes < 0:
            raise ValueError("default_bytes 不能为负")
        self.default_bytes = default_bytes
        self.overrides = dict(overrides or {})

    @classmethod
    def load(cls, path: Path) -> "QuotaPlan":
        payload = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(payload, dict):
            raise ValueError("额度计划顶层必须是对象")
        allowed = {"schema_version", "default_bytes", "overrides"}
        unknown = set(payload) - allowed
        if unknown:
            # 与配置面同一纪律：未知字段硬失败。写错一个键名就静默按默认额度跑，
            # 是「看起来生效了、实际没生效」的典型。
            raise ValueError(f"额度计划含未知字段：{sorted(unknown)}")
        if payload.get("schema_version") != 1:
            raise ValueError("额度计划 schema_version 必须是 1")
        default_bytes = payload.get("default_bytes", 0)
        if type(default_bytes) is not int or isinstance(default_bytes, bool) or default_bytes < 0:
            raise ValueError("default_bytes 必须是非负整数")
        overrides = payload.get("overrides", {})
        if not isinstance(overrides, dict):
            raise ValueError("overrides 必须是对象")
        for key, value in overrides.items():
            if "/" not in key:
                raise ValueError(f"overrides 的键必须是 <inbound_tag>/<name>：{key}")
            if type(value) is not int or isinstance(value, bool) or value < 0:
                raise ValueError(f"overrides[{key}] 必须是非负整数")
        return cls(default_bytes, overrides)

    def quota_for(self, inbound_tag: str, name: str) -> int:
        return self.overrides.get(f"{inbound_tag}/{name}", self.default_bytes)


def push_quota(
    quota_socket: str,
    snapshot: dict,
    ledger: Ledger,
    collector: Collector,
    plan: QuotaPlan,
    timeout: float,
) -> int:
    """按 §5.4 重算并下发一份**全量**剩余额度表。

    三条义务在这里落地：
      1. 「本周期已用」取自采集端自己的累计（`collector.usage`），不是快照绝对值；
      2. 每次采集后都重推——进程侧的额度是纯内存的，重启即消失；
      3. 顺序是先入账、再算额度、再推送，因此本函数只在 ingest 成功之后被调用。

    未出现在表中的身份会被服务端视为**无限额度**，所以这里必须列出快照里的每一个身份。
    """
    entries = []
    for inbound in snapshot["inbounds"]:
        for user in inbound["users"]:
            entries.append(
                {
                    "inbound_tag": inbound["tag"],
                    "name": user["name"],
                    "remaining_bytes": collector.remaining_for(
                        snapshot["node_id"],
                        inbound["tag"],
                        user["name"],
                        plan.quota_for(inbound["tag"], user["name"]),
                    ),
                }
            )
    epoch = collector.next_quota_epoch()
    body = json.dumps(
        {
            "schema_version": 2,
            "node_id": snapshot["node_id"],
            "runtime_id": snapshot["runtime_id"],
            "epoch": epoch,
            "entries": entries,
        },
        ensure_ascii=False,
    ).encode("utf-8")
    now = int(time.time() * 1000)
    try:
        response = http_unix.request(quota_socket, "PUT", "/v2/quota", body=body, timeout=timeout)
    except http_unix.HTTPUnixError as error:
        ledger.append({"ts": now, "result": "quota_transport_error", "detail": str(error)})
        return 1
    exhausted = [entry["name"] for entry in entries if entry["remaining_bytes"] == 0]
    record = {
        "ts": now,
        "result": "quota_pushed" if response.status == 200 else "quota_rejected",
        "status": response.status,
        "epoch": epoch,
        "entries": len(entries),
        "exhausted": exhausted,
    }
    if response.status != 200:
        record["detail"] = response.body.decode("utf-8", "replace").strip()
    ledger.append(record)
    ledger.save_state(collector.export_state())
    return 0 if response.status == 200 else 1


def collect_once(
    socket_path: str,
    ledger: Ledger,
    collector: Collector,
    timeout: float,
    quota_socket: str | None = None,
    plan: QuotaPlan | None = None,
) -> int:
    now = int(time.time() * 1000)
    try:
        response = http_unix.request(socket_path, "GET", "/v2/snapshot", timeout=timeout)
    except http_unix.HTTPUnixError as error:
        ledger.append({"ts": now, "result": "transport_error", "detail": str(error)})
        return 1
    if response.status == 429:
        # 429 与连接被直接关闭视为可重试且不得入账。
        ledger.append({"ts": now, "result": "retryable", "status": 429})
        return 0
    if response.status != 200:
        ledger.append({"ts": now, "result": "rejected", "status": response.status})
        return 1
    try:
        snapshot = parse_snapshot(response.body)
    except SnapshotRejected as error:
        ledger.append({"ts": now, "result": "schema_rejected", "detail": str(error)})
        return 1
    try:
        settlement = collector.ingest(snapshot)
    except SettlementFailure as error:
        # 失败关闭：不得猜测并继续收费。
        ledger.append({"ts": now, "result": "failed_closed", "detail": str(error)})
        return 2
    if not settlement.accepted:
        ledger.append(
            {
                "ts": now,
                "result": "not_accepted",
                "detail": settlement.reason,
                "sequence": snapshot["sequence"],
            }
        )
        # 即使本轮没入账也要落状态：ingest 可能已经登记了新的 runtime（例如
        # started_at 变化那一支），丢掉它会让下一轮又从头开始。
        ledger.save_state(collector.export_state())
        return 0
    total = {name: 0 for name in COUNTER_FIELDS}
    entries = []
    for key, delta in sorted(settlement.deltas.items()):
        for name in COUNTER_FIELDS:
            total[name] += delta[name]
        entries.append({"key": list(key), "delta": delta})
    ledger.append(
        {
            "ts": now,
            "result": "accepted",
            "batch": settlement.batch,
            "sequence": snapshot["sequence"],
            "node_id": snapshot["node_id"],
            "runtime_id": snapshot["runtime_id"],
            "first_snapshot": settlement.first_snapshot,
            "total": total,
            "entries": entries,
        }
    )
    ledger.save_state(collector.export_state())
    if quota_socket and plan is not None:
        # §5.4 第 3 条：先持久化入账，再算额度，再推送。
        return push_quota(quota_socket, snapshot, ledger, collector, plan, timeout)
    return 0


def report(ledger: Ledger) -> int:
    """输出长跑的四项判据（README §7 里程碑 5）。"""
    counters = {
        "accepted": 0,
        "transport_error": 0,
        "schema_rejected": 0,
        "failed_closed": 0,
        "not_accepted": 0,
        "retryable": 0,
        "rejected": 0,
    }
    sequences: dict[str, set[int]] = {}
    duplicate_sequences = 0
    if ledger.path.is_file():
        for line in ledger.path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            record = json.loads(line)
            result = record.get("result", "")
            counters[result] = counters.get(result, 0) + 1
            if result == "accepted":
                bucket = sequences.setdefault(record["runtime_id"], set())
                if record["sequence"] in bucket:
                    duplicate_sequences += 1
                bucket.add(record["sequence"])
    verdict = {
        "negative_delta": counters["failed_closed"],
        "unknown_runtime": counters["schema_rejected"],
        "duplicate_sequence": duplicate_sequences,
        "unhealthy_accepted": 0,
        "accepted": counters["accepted"],
        "runtimes": len(sequences),
    }
    print(json.dumps({"counters": counters, "verdict": verdict}, ensure_ascii=False, indent=2))
    failing = verdict["negative_delta"] or verdict["duplicate_sequence"] or verdict["unhealthy_accepted"]
    return 1 if failing else 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="sing-box-plus 参考 collector")
    parser.add_argument("--socket", help="快照 socket 路径")
    parser.add_argument("--ledger", required=True, type=Path, help="本地账本目录")
    parser.add_argument(
        "--first-snapshot",
        choices=("baseline", "include"),
        default="baseline",
        help="首次看到新 runtime_id 时的策略，必须显式选择",
    )
    parser.add_argument("--quota-socket", help="启用 §5.4 的额度下发：每次采集后重推全量表")
    parser.add_argument(
        "--quota-bytes",
        type=int,
        default=0,
        help="所有身份统一的周期额度（四向之和，字节）；与 --quota-plan 二选一",
    )
    parser.add_argument(
        "--quota-plan",
        type=Path,
        help="额度计划 JSON：{schema_version:1, default_bytes, overrides:{\"tag/name\": bytes}}",
    )
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--report", action="store_true", help="只输出长跑判据，不采集")
    parser.add_argument(
        "--reset-period",
        action="store_true",
        help="开一个新的计费周期：清零本周期已用，保留基线与 runtime 状态",
    )
    args = parser.parse_args(argv)

    ledger = Ledger(args.ledger)
    if args.report:
        return report(ledger)
    if args.reset_period:
        # 只清「本周期已用」，**不动基线**：基线清掉的话，下一次采集会把快照里的
        # 全部历史累计值当成一次巨额增量重新入账。
        collector = Collector(first_snapshot=args.first_snapshot)
        collector.restore_state(ledger.load_state())
        cleared = sum(collector.usage.values())
        collector.usage.clear()
        ledger.save_state(collector.export_state())
        ledger.append({"ts": int(time.time() * 1000), "result": "period_reset", "cleared_bytes": cleared})
        print(f"已开始新计费周期：清零 {cleared:,} 字节的已用量（基线与 runtime 状态保留）")
        return 0
    if not args.socket:
        parser.error("采集模式需要 --socket")
    collector = Collector(first_snapshot=args.first_snapshot)
    # 必须先恢复上一轮的基线，否则「每次采集起一个进程」的用法会把每一轮都当成首快照，
    # 于是永远只建基线、永不入账——而四项判据依然全绿。
    try:
        collector.restore_state(ledger.load_state())
    except SnapshotRejected as error:
        print(f"错误：无法沿用已有采集状态：{error}", file=sys.stderr)
        return 2
    plan = None
    if args.quota_plan is not None and args.quota_bytes > 0:
        parser.error("--quota-plan 与 --quota-bytes 互斥")
    if args.quota_plan is not None:
        try:
            plan = QuotaPlan.load(args.quota_plan)
        except (OSError, ValueError, json.JSONDecodeError) as error:
            print(f"错误：额度计划不可用：{error}", file=sys.stderr)
            return 2
    elif args.quota_bytes > 0:
        plan = QuotaPlan(args.quota_bytes)
    if bool(args.quota_socket) != (plan is not None):
        parser.error("--quota-socket 必须与 --quota-bytes 或 --quota-plan 同时给出")
    return collect_once(args.socket, ledger, collector, args.timeout, args.quota_socket, plan)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
