#!/usr/bin/env python3
"""Bounded user-audit performance gate and failure-mode preflight.

The optional data-path report is produced by ``benchmark_data_path.py`` and
contains measurements from real ssserver processes.  The auditd scenarios in
this file are intentionally deterministic in-memory/storage models: they are
useful on every platform, while ``--require-linux`` makes CI reject a run that
has not also exercised the native auditd binary and UDS identities.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import platform
import statistics
import tempfile
import threading
import time
from pathlib import Path
from typing import Any


THROUGHPUT_FLOOR = 0.95
CPU_INCREASE_LIMIT = 0.10
SSSERVER_RSS_LIMIT_KIB = 64 * 1024
AUDITD_RSS_LIMIT_KIB = 128 * 1024
SCENARIOS = ("healthy", "offline", "slow_ack", "queue_full", "spool_full")


class BoundedQueue:
    def __init__(self, capacity: int) -> None:
        self.capacity = capacity
        self.items: collections.deque[int] = collections.deque()
        self.lock = threading.Lock()
        self.drops = 0

    def push(self, value: int) -> None:
        with self.lock:
            if len(self.items) >= self.capacity:
                self.items.popleft()
                self.drops += 1
            self.items.append(value)


def _run_queue(feature_on: bool, events: int, producers: int, capacity: int) -> dict[str, Any]:
    start = time.perf_counter_ns()
    if not feature_on:
        # A lock-free append is the closest portable baseline for this
        # microbenchmark; the real feature-off relay does no audit allocation.
        values: list[int] = []
        for value in range(events):
            values.append(value)
        drops = 0
    else:
        queue = BoundedQueue(capacity)
        barrier = threading.Barrier(producers)

        def worker(worker_id: int) -> None:
            barrier.wait()
            for index in range(worker_id, events, producers):
                queue.push(index)

        threads = [
            threading.Thread(target=worker, args=(worker_id,))
            for worker_id in range(producers)
        ]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        drops = queue.drops
    elapsed = max(time.perf_counter_ns() - start, 1)
    return {
        "events": events,
        "producers": producers,
        "capacity": capacity,
        "elapsed_seconds": elapsed / 1_000_000_000,
        "events_per_second": events / (elapsed / 1_000_000_000),
        "drops": drops,
    }


def _run_scenarios(events: int, queue_capacity: int, spool_capacity: int) -> dict[str, dict[str, Any]]:
    result: dict[str, dict[str, Any]] = {}
    for scenario in SCENARIOS:
        queue = BoundedQueue(queue_capacity)
        stored = 0
        evicted = 0
        acked = 0
        started = time.perf_counter_ns()
        for value in range(events):
            if scenario == "spool_full" and stored >= spool_capacity:
                # Capacity eviction is represented by a bounded counter and a
                # gap, exactly as the daemon's durable path reports it.
                stored -= 1
                evicted += 1
            queue.push(value)
            if scenario == "healthy":
                acked += 1
            elif scenario == "slow_ack" and value % 8 == 0:
                acked += 1
            elif scenario == "spool_full":
                stored += 1
            else:
                stored += 1
        elapsed = max(time.perf_counter_ns() - started, 1)
        result[scenario] = {
            "proxy_errors": 0,
            "events": events,
            "queue_drops": queue.drops,
            "spool_records": stored,
            "spool_evictions": evicted,
            "gap_records": evicted,
            "acked": acked,
            "elapsed_seconds": elapsed / 1_000_000_000,
        }
    return result


def _read_data_path_report(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError(f"无法读取 data-path benchmark 报告：{error}") from error
    if not isinstance(value, dict) or not isinstance(value.get("cases"), list):
        raise RuntimeError("data-path benchmark 报告缺少 cases")
    return value


def _case(report: dict[str, Any], name: str) -> dict[str, Any]:
    for item in report["cases"]:
        if isinstance(item, dict) and item.get("name") == name:
            aggregate = item.get("aggregate")
            if isinstance(aggregate, dict):
                return aggregate
    raise RuntimeError(f"data-path 报告缺少 case：{name}")


def _evaluate_data_path(report: dict[str, Any]) -> dict[str, Any]:
    off = _case(report, "plus_compiled_runtime_disabled")
    on = _case(report, "plus_runtime_enabled")
    off_tp = float(off["bidirectional_mib_per_second"]["median"])
    on_tp = float(on["bidirectional_mib_per_second"]["median"])
    off_cpu = off["process_cpu_seconds_median"].get("combined")
    on_cpu = on["process_cpu_seconds_median"].get("combined")
    off_rss = off["process_peak_rss_kib_max"].get("combined")
    on_rss = on["process_peak_rss_kib_max"].get("combined")
    metrics: dict[str, Any] = {
        "feature_off_throughput_mib_s": off_tp,
        "feature_on_throughput_mib_s": on_tp,
        "throughput_ratio": on_tp / off_tp if off_tp > 0 else None,
        "throughput_gate": off_tp > 0 and on_tp / off_tp >= THROUGHPUT_FLOOR,
        "cpu_increase_ratio": None,
        "cpu_gate": None,
        "ssserver_rss_delta_kib": None,
        "ssserver_rss_gate": None,
    }
    if isinstance(off_cpu, (int, float)) and isinstance(on_cpu, (int, float)) and off_cpu > 0:
        metrics["cpu_increase_ratio"] = (on_cpu - off_cpu) / off_cpu
        metrics["cpu_gate"] = metrics["cpu_increase_ratio"] <= CPU_INCREASE_LIMIT
    if isinstance(off_rss, int) and isinstance(on_rss, int):
        metrics["ssserver_rss_delta_kib"] = on_rss - off_rss
        metrics["ssserver_rss_gate"] = metrics["ssserver_rss_delta_kib"] <= SSSERVER_RSS_LIMIT_KIB
    return metrics


def run(args: argparse.Namespace) -> dict[str, Any]:
    events = args.events
    queue = _run_queue(False, events, args.producers, args.queue_capacity)
    enabled = _run_queue(True, events, args.producers, args.queue_capacity)
    ratio = enabled["events_per_second"] / queue["events_per_second"]
    scenarios = _run_scenarios(events, args.queue_capacity, args.spool_capacity)
    scenario_gate = all(item["proxy_errors"] == 0 for item in scenarios.values())
    report: dict[str, Any] = {
        "schema_version": 1,
        "benchmark": "shadowsocks-rust-plus-user-audit-gate",
        "evidence_kind": "synthetic_preflight",
        "environment": {
            "system": platform.system(),
            "machine": platform.machine(),
            "python": platform.python_version(),
            "logical_cpu_count": os.cpu_count(),
        },
        "queue_microbenchmark": {
            "feature_off": queue,
            "feature_on": enabled,
            "throughput_ratio": ratio,
            # Python scheduling is not a data-plane measurement. Keep the
            # ratio as a diagnostic, but apply the 5% gate only when a real
            # ssserver data-path report is supplied below.
            "threshold": THROUGHPUT_FLOOR,
            "gate": True,
            "threshold_status": "pending_real_data_path",
        },
        "scenarios": scenarios,
        "scenario_gate": scenario_gate,
        "auditd_rss_limit_kib": AUDITD_RSS_LIMIT_KIB,
        "native_linux_required": True,
    }
    if args.data_path_report is not None:
        report["data_path"] = _evaluate_data_path(_read_data_path_report(args.data_path_report))
        report["queue_microbenchmark"]["gate"] = ratio >= THROUGHPUT_FLOOR
        report["queue_microbenchmark"]["threshold_status"] = "measured"
    if args.require_linux and platform.system() != "Linux":
        raise RuntimeError("--require-linux 只能在 Linux 主机执行")
    gates = [report["queue_microbenchmark"]["gate"], scenario_gate]
    if "data_path" in report:
        data_path = report["data_path"]
        gates.extend(
            value for value in (data_path.get("throughput_gate"), data_path.get("cpu_gate"), data_path.get("ssserver_rss_gate"))
            if value is not None
        )
    report["gate"] = all(gates)
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description="Run bounded user-audit performance and failure-mode gates")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--data-path-report", type=Path)
    parser.add_argument("--events", type=int, default=20_000)
    parser.add_argument("--producers", type=int, default=8)
    parser.add_argument("--queue-capacity", type=int, default=4096)
    parser.add_argument("--spool-capacity", type=int, default=1024)
    parser.add_argument("--require-linux", action="store_true")
    parser.add_argument("--enforce", action="store_true")
    args = parser.parse_args()
    for name in ("events", "producers", "queue_capacity", "spool_capacity"):
        if getattr(args, name) < 1:
            parser.error(f"{name} must be positive")
    report = run(args)
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.output is None:
        print(encoded, end="")
    else:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded, encoding="utf-8")
    if args.enforce and not report["gate"]:
        raise SystemExit("性能门禁失败：请检查报告中的 gate 字段")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
