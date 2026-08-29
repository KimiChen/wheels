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
import re
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
DATA_PATH_BENCHMARK = "shadowsocks-rust-plus-loopback-data-path"
DATA_PATH_EVIDENCE_KIND = "native_user_audit_data_path"
# A workload that only ever addresses one UDP destination keeps a single audit
# window entry alive per association, so the ssserver RSS budget of this file
# cannot observe the window cache at all. Refuse such evidence.
DATA_PATH_MIN_UDP_TARGETS = 8
DATA_PATH_CASES = (
    "locked_upstream",
    "plus_compiled_runtime_disabled",
    "plus_runtime_enabled",
)
SHA256_PATTERN = re.compile(r"[0-9a-f]{64}")


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
        started = time.perf_counter_ns()
        for value in range(events):
            queue.push(value)
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
        item: dict[str, Any] = {
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
        # This is a queue/spool state model, not a proxy execution. A nullable
        # marker prevents synthetic bookkeeping from being cited as data-path
        # availability evidence.
        item["proxy_path_gate"] = None
        item["gate"] = gate
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


def _case(report: dict[str, Any], name: str) -> dict[str, Any] | None:
    cases = report.get("cases")
    if not isinstance(cases, list):
        return None
    matches = [item for item in cases if isinstance(item, dict) and item.get("name") == name]
    return matches[0] if len(matches) == 1 else None


def _mapping(value: object) -> dict[str, Any] | None:
    return value if isinstance(value, dict) else None


def _nested_mapping(value: object, *keys: str) -> dict[str, Any] | None:
    current = _mapping(value)
    for key in keys:
        if current is None:
            return None
        current = _mapping(current.get(key))
    return current


def _nested_value(value: object, *keys: str) -> object:
    current: object = value
    for key in keys:
        mapping = _mapping(current)
        if mapping is None:
            return None
        current = mapping.get(key)
    return current


def _finite_number(value: object) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def _integer(value: object) -> int | None:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        return None
    return value


def _extract_proxy_measurement(
    value: dict[str, Any] | None,
) -> tuple[int | None, int | None, int | None]:
    if value is None:
        return None, None, None
    return (
        _integer(value.get("attempts")),
        _integer(value.get("successes")),
        _integer(value.get("errors")),
    )


def _evaluate_data_path(
    report: dict[str, Any],
    auditd_report: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Evaluate only self-contained native user-audit evidence.

    A detached auditd report cannot prove that its process samples belong to
    the same execution as the proxy workload, so it is deliberately rejected.
    The optional argument remains for callers of the previous helper API and
    produces an indeterminate evidence gate.
    """

    issues: list[str] = []
    if report.get("benchmark") != DATA_PATH_BENCHMARK:
        issues.append("unexpected benchmark identity")
    if report.get("evidence_kind") != DATA_PATH_EVIDENCE_KIND:
        issues.append("missing native user-audit evidence kind")
    if _nested_value(report, "environment", "system") != "Linux":
        issues.append("data-path evidence was not collected on Linux")
    run_id = report.get("run_id")
    if not isinstance(run_id, str) or not re.fullmatch(r"[0-9a-f]{32}", run_id):
        issues.append("invalid benchmark run_id")
    udp_targets = _integer(_nested_value(report, "workload", "udp", "distinct_targets"))
    if udp_targets is None or udp_targets < DATA_PATH_MIN_UDP_TARGETS:
        issues.append("workload did not exercise enough distinct UDP targets")

    build = _mapping(report.get("build"))
    plus_features = build.get("plus_extra_features") if build is not None else None
    if not isinstance(plus_features, list) or "user-audit" not in plus_features:
        issues.append("plus build does not prove the user-audit feature")
    if build is None or build.get("locked") is not True:
        issues.append("build was not performed with the locked dependency graph")

    records = {name: _case(report, name) for name in DATA_PATH_CASES}
    for name, item in records.items():
        if item is None or _mapping(item.get("aggregate")) is None:
            issues.append(f"missing unique case {name}")

    upstream_record = records["locked_upstream"]
    disabled_record = records["plus_compiled_runtime_disabled"]
    enabled_record = records["plus_runtime_enabled"]
    expected_runtime = (
        (upstream_record, False, False, "locked_upstream"),
        (disabled_record, False, False, "plus_compiled_runtime_disabled"),
        (enabled_record, True, True, "plus_runtime_enabled"),
    )
    for item, expected_stats, expected_audit, label in expected_runtime:
        if item is None:
            continue
        if item.get("runtime_user_stats") is not expected_stats:
            issues.append(f"{label} has unexpected user-stats runtime state")
        if item.get("runtime_user_audit") is not expected_audit:
            issues.append(f"{label} has unexpected user-audit runtime state")
        if item.get("native_process_measurement") is not True:
            issues.append(f"{label} lacks native process measurement marker")

    upstream = _mapping(upstream_record.get("aggregate")) if upstream_record is not None else None
    enabled = _mapping(enabled_record.get("aggregate")) if enabled_record is not None else None
    upstream_tp = _finite_number(_nested_value(upstream, "bidirectional_mib_per_second", "median"))
    enabled_tp = _finite_number(_nested_value(enabled, "bidirectional_mib_per_second", "median"))
    metrics: dict[str, Any] = {
        "native_evidence_gate": None,
        "evidence_issues": issues,
        "locked_upstream_throughput_mib_s": upstream_tp,
        "user_audit_throughput_mib_s": enabled_tp,
        "throughput_ratio": None,
        "throughput_gate": None,
        "cpu_increase_ratio": None,
        "cpu_gate": None,
        "ssserver_rss_delta_kib": None,
        "ssserver_rss_gate": None,
        "auditd_peak_rss_kib": None,
        "auditd_rss_gate": None,
        "audit_ingest_gate": None,
        "proxy_attempts": None,
        "proxy_successes": None,
        "proxy_errors": None,
        "proxy_path_gate": None,
    }
    if upstream_tp is not None and enabled_tp is not None and upstream_tp > 0:
        metrics["throughput_ratio"] = enabled_tp / upstream_tp
        metrics["throughput_gate"] = metrics["throughput_ratio"] >= THROUGHPUT_FLOOR

    upstream_cpu = _finite_number(_nested_value(upstream, "process_cpu_seconds_median", "ssserver"))
    enabled_cpu = _finite_number(_nested_value(enabled, "process_cpu_seconds_median", "ssserver"))
    if upstream_cpu is not None and enabled_cpu is not None and upstream_cpu > 0:
        metrics["cpu_increase_ratio"] = (enabled_cpu - upstream_cpu) / upstream_cpu
        metrics["cpu_gate"] = metrics["cpu_increase_ratio"] <= CPU_INCREASE_LIMIT

    upstream_rss = _integer(_nested_value(upstream, "process_peak_rss_kib_max", "ssserver"))
    enabled_rss = _integer(_nested_value(enabled, "process_peak_rss_kib_max", "ssserver"))
    if upstream_rss is not None and enabled_rss is not None:
        metrics["ssserver_rss_delta_kib"] = enabled_rss - upstream_rss
        metrics["ssserver_rss_gate"] = metrics["ssserver_rss_delta_kib"] <= SSSERVER_RSS_LIMIT_KIB

    auditd = _mapping(report.get("auditd"))
    if auditd_report is not None:
        issues.append("detached auditd evidence is not accepted")
    if auditd is None:
        issues.append("missing in-run auditd measurement")
    else:
        if auditd.get("run_id") != run_id:
            issues.append("auditd measurement run_id does not match benchmark")
        if auditd.get("measurement_source") != "resource_monitor_pid":
            issues.append("auditd RSS is not identified as a live process sample")
        sample_count = _integer(auditd.get("rss_sample_count"))
        if sample_count is None or sample_count < 1:
            issues.append("auditd RSS has no successful process samples")
        executable_sha256 = auditd.get("executable_sha256")
        if not isinstance(executable_sha256, str) or SHA256_PATTERN.fullmatch(executable_sha256) is None:
            issues.append("auditd executable digest is missing or invalid")
        built_auditd_sha256 = _nested_value(
            build,
            "artifacts",
            "plus_user_audit",
            "shadowsocks-auditd",
            "sha256",
        )
        if executable_sha256 != built_auditd_sha256:
            issues.append("running auditd does not match the current plus build artifact")
        if (_integer(auditd.get("pid")) or 0) < 1:
            issues.append("auditd PID is missing or invalid")
        if (_integer(auditd.get("process_start_time_ticks")) or 0) < 1:
            issues.append("auditd process start identity is missing or invalid")
        executable_path = auditd.get("executable_path")
        if not isinstance(executable_path, str) or not executable_path.startswith("/"):
            issues.append("auditd executable path is missing or not absolute")
        ingest_socket_path = auditd.get("ingest_socket_path")
        if not isinstance(ingest_socket_path, str) or not ingest_socket_path.startswith("/"):
            issues.append("auditd ingest socket path is missing or not absolute")
        if (_integer(auditd.get("ingest_socket_inode")) or 0) < 1:
            issues.append("auditd ingest socket inode is missing")
        if _integer(auditd.get("ingest_socket_device")) is None:
            issues.append("auditd ingest socket device is missing")
        export_socket_path = auditd.get("export_socket_path")
        if not isinstance(export_socket_path, str) or not export_socket_path.startswith("/"):
            issues.append("auditd export socket path is missing or not absolute")
        if (_integer(auditd.get("export_socket_inode")) or 0) < 1:
            issues.append("auditd export socket inode is missing")
        if _integer(auditd.get("export_socket_device")) is None:
            issues.append("auditd export socket device is missing")
        peak = _integer(auditd.get("peak_rss_kib"))
        if peak is None:
            issues.append("auditd peak RSS is missing")
        else:
            metrics["auditd_peak_rss_kib"] = peak
            metrics["auditd_rss_gate"] = peak <= AUDITD_RSS_LIMIT_KIB
        enabled_peak = _integer(_nested_value(enabled, "process_peak_rss_kib_max", "auditd"))
        enabled_samples = _integer(
            _nested_value(enabled, "process_rss_sample_count_min", "auditd")
        )
        if peak != enabled_peak or sample_count != enabled_samples:
            issues.append("auditd summary does not match the enabled case process samples")

        producer_uid = _integer(auditd.get("producer_uid"))
        daemon_uid = _integer(auditd.get("uid"))
        export_uid = _integer(auditd.get("export_uid"))
        identity_names = (
            auditd.get("user"),
            auditd.get("producer_user"),
            auditd.get("export_user"),
        )
        if (
            producer_uid is None
            or daemon_uid is None
            or export_uid is None
            or len({producer_uid, daemon_uid, export_uid}) != 3
            or any(not isinstance(name, str) or not name for name in identity_names)
            or len(set(identity_names)) != 3
            or enabled_record is None
            or enabled_record.get("producer_uid") != producer_uid
            or enabled_record.get("producer_user") != auditd.get("producer_user")
        ):
            issues.append("auditd, producer and export identities are not independently bound")

        ingest = _mapping(auditd.get("ingest"))
        case_ingest = _mapping(enabled_record.get("audit_ingest")) if enabled_record is not None else None
        before = _mapping(ingest.get("before")) if ingest is not None else None
        after = _mapping(ingest.get("after")) if ingest is not None else None
        before_records = _integer(before.get("stored_records")) if before is not None else None
        after_records = _integer(after.get("stored_records")) if after is not None else None
        before_ingest = _integer(before.get("last_ingest_at_unix_ms")) if before is not None else None
        after_ingest = _integer(after.get("last_ingest_at_unix_ms")) if after is not None else None
        runtime_id = enabled_record.get("runtime_id") if enabled_record is not None else None
        ingest_valid = (
            ingest is not None
            and ingest == case_ingest
            and ingest.get("source") == "signed_health_stored_records_delta"
            and isinstance(runtime_id, str)
            and re.fullmatch(r"[0-9a-f]{32}", runtime_id) is not None
            and ingest.get("producer_runtime_id") == runtime_id
            and before is not None
            and before.get("producer_connected") is False
            and after is not None
            and after.get("http_status") == 200
            and after.get("status") == "ok"
            and after.get("producer_connected") is True
            and after.get("producer_runtime_id") == runtime_id
            and before.get("node_id") == auditd.get("node_id")
            and after.get("node_id") == auditd.get("node_id")
            and before.get("spool_epoch") == after.get("spool_epoch")
            and before_records is not None
            and after_records is not None
            and after_records > before_records
            and ingest.get("stored_records_delta") == after_records - before_records
            and after_ingest is not None
            and (before_ingest is None or after_ingest > before_ingest)
            and ingest.get("last_ingest_advanced") is True
            and before.get("storage_rejected_attempts")
            == after.get("storage_rejected_attempts")
            and before.get("evicted_unacked_records") == after.get("evicted_unacked_records")
        )
        if ingest_valid:
            metrics["audit_ingest_gate"] = True
        else:
            issues.append("signed auditd health does not prove a durable ingest delta")

    proxy = _nested_mapping(enabled, "proxy")
    if proxy is None or proxy.get("source") != "worker_outcomes":
        issues.append("enabled proxy counters are not worker outcomes")
    attempts, successes, errors = _extract_proxy_measurement(proxy)
    metrics["proxy_attempts"] = attempts
    metrics["proxy_successes"] = successes
    metrics["proxy_errors"] = errors
    if attempts is None or successes is None or errors is None:
        issues.append("enabled proxy outcome counters are incomplete")
    else:
        metrics["proxy_path_gate"] = (
            attempts > 0
            and successes <= attempts
            and successes + errors == attempts
            and successes > 0
            and errors == 0
        )

    required_metrics = (
        metrics["throughput_gate"],
        metrics["cpu_gate"],
        metrics["ssserver_rss_gate"],
        metrics["auditd_rss_gate"],
        metrics["audit_ingest_gate"],
        metrics["proxy_path_gate"],
    )
    if any(value is None for value in required_metrics):
        issues.append("one or more required native measurements are missing")
    if not issues:
        metrics["native_evidence_gate"] = True
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
        report["data_path"] = _evaluate_data_path(data_path_report)
        report["queue_microbenchmark"]["threshold_status"] = "diagnostic_only"
    if args.require_linux and platform.system() != "Linux":
        raise RuntimeError("--require-linux 只能在 Linux 主机执行")
    report["gate"] = None
    if "data_path" in report:
        data_path = report["data_path"]
        measured_gates = (
            data_path.get("native_evidence_gate"),
            data_path.get("throughput_gate"),
            data_path.get("cpu_gate"),
            data_path.get("ssserver_rss_gate"),
            data_path.get("auditd_rss_gate"),
            data_path.get("audit_ingest_gate"),
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
