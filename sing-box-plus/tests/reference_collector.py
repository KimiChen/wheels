#!/usr/bin/env python3
"""参考 collector：取快照 → v2 校验 → 差分 → 幂等落地本地账本。

范围**不含** outbox、mTLS 与重试——那些属于下游控制面。这里只把 §5.1 的契约跑通，
并且让「本次采集失败」与「本次没有流量」在账本里可区分。
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


def collect_once(socket_path: str, ledger: Ledger, collector: Collector, timeout: float) -> int:
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
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--report", action="store_true", help="只输出长跑判据，不采集")
    args = parser.parse_args(argv)

    ledger = Ledger(args.ledger)
    if args.report:
        return report(ledger)
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
    return collect_once(args.socket, ledger, collector, args.timeout)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
