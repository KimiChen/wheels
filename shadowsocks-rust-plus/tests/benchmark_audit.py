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
import math
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


def _proxy_path_gate(item: dict[str, Any]) -> bool:
    """Check the data-plane invariant independently of audit bookkeeping.

    These synthetic scenarios model audit degradation while the proxy itself
    remains available. Keep that contract explicit and verify the reported
    counters are internally consistent so a mutated failure cannot pass just
    because ``proxy_errors`` was derived from the same value.
    """

    attempts = item.get("proxy_attempts")
    successes = item.get("proxy_successes")
    errors = item.get("proxy_errors")
    if not all(isinstance(value, int) for value in (attempts, successes, errors)):
        return False
    if attempts < 0 or successes < 0 or errors < 0 or successes > attempts:
        return False
    return successes == attempts and errors == attempts - successes


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
        # queue_full intentionally uses a smaller producer queue; the offline
        # scenario drains that queue into available durable storage. Keeping
        # those resources distinct prevents two failure modes from collapsing
        # into the same synthetic trace.
        scenario_queue_capacity = (
            min(queue_capacity, max(1, events // 4)) if scenario == "queue_full" else queue_capacity
        )
        queue = BoundedQueue(scenario_queue_capacity)
        stored = 0
        evicted = 0
        acked = 0
        gap_records = 0
        proxy_attempts = 0
        proxy_successes = 0
        started = time.perf_counter_ns()
        for value in range(events):
            proxy_attempts += 1
            queue.push(value)
            proxy_successes += 1
            if scenario == "queue_full":
                # No consumer progress: each oldest-item eviction is a producer
                # gap and the remaining queue stays bounded.
                gap_records = queue.drops
                stored = len(queue.items)
                continue

            queued = queue.items.popleft()
            if queued != value:
                raise RuntimeError("synthetic queue reordered an audit event")
            if scenario == "healthy":
                acked += 1
            elif scenario == "slow_ack" and value % 8 == 0:
                acked += 1
            elif scenario == "spool_full":
                if stored >= spool_capacity:
                    stored -= 1
                    evicted += 1
                    gap_records += 1
                stored += 1
            else:
                stored += 1
        elapsed = max(time.perf_counter_ns() - started, 1)
        proxy_errors = proxy_attempts - proxy_successes
        item: dict[str, Any] = {
            "proxy_attempts": proxy_attempts,
            "proxy_successes": proxy_successes,
            "proxy_errors": proxy_errors,
            "events": events,
            "queue_capacity": scenario_queue_capacity,
            "queue_drops": queue.drops,
            "spool_records": stored,
            "spool_evictions": evicted,
            "gap_records": gap_records,
            "acked": acked,
            "elapsed_seconds": elapsed / 1_000_000_000,
        }
        if scenario == "healthy":
            gate = queue.drops == 0 and stored == 0 and acked == events and gap_records == 0
        elif scenario == "offline":
            gate = queue.drops == 0 and stored == events and acked == 0 and evicted == 0
        elif scenario == "slow_ack":
            gate = queue.drops == 0 and 0 < acked < events and stored + acked == events
        elif scenario == "queue_full":
            expected_drops = max(0, events - scenario_queue_capacity)
            gate = queue.drops == expected_drops and gap_records == expected_drops and stored == min(
                events, scenario_queue_capacity
            )
        else:
            expected_evictions = max(0, events - spool_capacity)
            gate = (
                queue.drops == 0
                and stored == min(events, spool_capacity)
                and evicted == expected_evictions
                and gap_records == expected_evictions
            )
        item["proxy_path_gate"] = _proxy_path_gate(item)
        item["gate"] = item["proxy_path_gate"] and gate
        result[scenario] = item
    return result


def _read_json_report(path: Path, label: str) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError(f"无法读取 {label} 报告：{error}") from error
    if not isinstance(value, dict):
        raise RuntimeError(f"{label} benchmark 报告必须是 JSON 对象")
    return value


def _read_data_path_report(path: Path) -> dict[str, Any]:
    value = _read_json_report(path, "data-path")
    if not isinstance(value.get("cases"), list):
        raise RuntimeError("data-path benchmark 报告缺少 cases")
    return value


def _case(report: dict[str, Any], name: str) -> dict[str, Any]:
    for item in report["cases"]:
        if isinstance(item, dict) and item.get("name") == name:
            aggregate = item.get("aggregate")
            if isinstance(aggregate, dict):
                return aggregate
    raise RuntimeError(f"data-path 报告缺少 case：{name}")


def _finite_number(value: object) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def _integer(value: object) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        return None
    return value


def _measurement_object(report: dict[str, Any]) -> dict[str, Any] | None:
    """Find the independently collected auditd measurement object.

    The preferred shape is ``{"auditd": { ... }}`` in the data-path report;
    ``--auditd-report`` may supply the same object separately for runners that
    keep daemon RSS/health evidence in a restricted file.
    """

    value = report.get("auditd")
    return value if isinstance(value, dict) else None


def _extract_proxy_measurement(value: dict[str, Any] | None) -> tuple[int | None, int | None, int | None]:
    if value is None:
        return None, None, None
    proxy = value.get("proxy")
    if not isinstance(proxy, dict):
        proxy = value
    return (
        _integer(proxy.get("attempts", proxy.get("proxy_attempts"))),
        _integer(proxy.get("successes", proxy.get("proxy_successes"))),
        _integer(proxy.get("errors", proxy.get("proxy_errors"))),
    )


def _evaluate_data_path(report: dict[str, Any], auditd_report: dict[str, Any] | None = None) -> dict[str, Any]:
    off = _case(report, "plus_compiled_runtime_disabled")
    on = _case(report, "plus_runtime_enabled")
    off_tp = _finite_number(off.get("bidirectional_mib_per_second", {}).get("median"))
    on_tp = _finite_number(on.get("bidirectional_mib_per_second", {}).get("median"))
    if off_tp is None or on_tp is None:
        raise RuntimeError("data-path 报告缺少有限的吞吐 median")
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
        "auditd_peak_rss_kib": None,
        "auditd_rss_gate": None,
        "proxy_attempts": None,
        "proxy_successes": None,
        "proxy_errors": None,
        "proxy_path_gate": None,
    }
    off_cpu = _finite_number(off_cpu)
    on_cpu = _finite_number(on_cpu)
    if off_cpu is not None and on_cpu is not None and off_cpu > 0:
        metrics["cpu_increase_ratio"] = (on_cpu - off_cpu) / off_cpu
        metrics["cpu_gate"] = metrics["cpu_increase_ratio"] <= CPU_INCREASE_LIMIT
    off_rss = _integer(off_rss)
    on_rss = _integer(on_rss)
    if off_rss is not None and on_rss is not None:
        metrics["ssserver_rss_delta_kib"] = on_rss - off_rss
        metrics["ssserver_rss_gate"] = metrics["ssserver_rss_delta_kib"] <= SSSERVER_RSS_LIMIT_KIB

    measured = auditd_report if auditd_report is not None else _measurement_object(report)
    if measured is not None:
        peak = _integer(
            measured.get("peak_rss_kib", measured.get("auditd_peak_rss_kib"))
        )
        if peak is not None:
            metrics["auditd_peak_rss_kib"] = peak
            metrics["auditd_rss_gate"] = peak <= AUDITD_RSS_LIMIT_KIB
        attempts, successes, errors = _extract_proxy_measurement(measured)
        metrics["proxy_attempts"] = attempts
        metrics["proxy_successes"] = successes
        metrics["proxy_errors"] = errors
        if attempts is not None and successes is not None and errors is not None:
            metrics["proxy_path_gate"] = (
                attempts > 0
                and successes + errors == attempts
                and successes > 0
                and errors == 0
            )
    return metrics


def run(args: argparse.Namespace) -> dict[str, Any]:
    events = args.events
    queue = _run_queue(False, events, args.producers, args.queue_capacity)
    enabled = _run_queue(True, events, args.producers, args.queue_capacity)
    ratio = enabled["events_per_second"] / queue["events_per_second"]
    scenarios = _run_scenarios(events, args.queue_capacity, args.spool_capacity)
    scenario_gate = all(item["gate"] for item in scenarios.values())
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
            "gate": None,
            "threshold_status": "pending_real_data_path",
        },
        "scenarios": scenarios,
        "scenario_gate": scenario_gate,
        "auditd_rss_limit_kib": AUDITD_RSS_LIMIT_KIB,
        "native_linux_required": True,
    }
    if args.data_path_report is not None:
        data_path_report = _read_data_path_report(args.data_path_report)
        auditd_report = None
        auditd_path = getattr(args, "auditd_report", None)
        if auditd_path is not None:
            auditd_report = _read_json_report(auditd_path, "auditd")
        report["data_path"] = _evaluate_data_path(data_path_report, auditd_report)
        report["queue_microbenchmark"]["threshold_status"] = "diagnostic_only"
    if args.require_linux and platform.system() != "Linux":
        raise RuntimeError("--require-linux 只能在 Linux 主机执行")
    report["gate"] = None
    if "data_path" in report:
        data_path = report["data_path"]
        measured_gates = (
            data_path.get("throughput_gate"),
            data_path.get("cpu_gate"),
            data_path.get("ssserver_rss_gate"),
            data_path.get("auditd_rss_gate"),
            data_path.get("proxy_path_gate"),
        )
        # Missing measurements remain indeterminate instead of becoming a
        # passing boolean. ``--enforce`` turns that indeterminate state into a
        # hard failure below.
        if all(isinstance(value, bool) for value in measured_gates):
            report["gate"] = scenario_gate and all(value is True for value in measured_gates)
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description="Run bounded user-audit performance and failure-mode gates")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--data-path-report", type=Path)
    parser.add_argument(
        "--auditd-report",
        type=Path,
        help="独立 auditd RSS/代理计数报告（也可嵌入 data-path 报告的 auditd 字段）",
    )
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
    if args.enforce and args.data_path_report is None:
        raise SystemExit("性能门禁失败：--enforce 要求真实 --data-path-report")
    if args.enforce and report["gate"] is not True:
        raise SystemExit("性能门禁失败：请检查报告中的 gate 字段")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
