#!/usr/bin/env python3
"""Unit checks for the portable audit benchmark gate."""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "tests" / "benchmark_audit.py"
SPEC = importlib.util.spec_from_file_location("benchmark_audit", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class AuditBenchmarkTest(unittest.TestCase):
    @staticmethod
    def _native_report(
        *,
        proxy_errors: int = 0,
        include_auditd: bool = True,
        plus_features: list[str] | None = None,
    ) -> dict[str, object]:
        run_id = "1" * 32
        runtime_id = "2" * 32
        spool_epoch = "3" * 32
        node_id = "benchmark-node"
        before_health: dict[str, object] = {
            "http_status": 200,
            "status": "ok",
            "node_id": node_id,
            "producer_connected": False,
            "producer_runtime_id": None,
            "last_ingest_at_unix_ms": None,
            "spool_epoch": spool_epoch,
            "stored_records": 40,
            "storage_rejected_attempts": 0,
            "evicted_unacked_records": 0,
        }
        after_health: dict[str, object] = {
            "http_status": 200,
            "status": "ok",
            "node_id": node_id,
            "producer_connected": True,
            "producer_runtime_id": runtime_id,
            "last_ingest_at_unix_ms": 2_000,
            "spool_epoch": spool_epoch,
            "stored_records": 44,
            "storage_rejected_attempts": 0,
            "evicted_unacked_records": 0,
        }
        ingest: dict[str, object] = {
            "source": "signed_health_stored_records_delta",
            "producer_runtime_id": runtime_id,
            "before": before_health,
            "after": after_health,
            "stored_records_delta": 4,
            "last_ingest_advanced": True,
        }

        def aggregate(
            throughput: float,
            cpu: float,
            rss: int,
            proxy: dict[str, object] | None = None,
        ) -> dict[str, object]:
            value: dict[str, object] = {
                "bidirectional_mib_per_second": {"median": throughput},
                "process_cpu_seconds_median": {
                    "ssserver": cpu,
                    # Deliberately huge to prove the gate does not use the
                    # combined ssserver+sslocal value.
                    "combined": cpu * 100,
                },
                "process_peak_rss_kib_max": {
                    "ssserver": rss,
                    "combined": rss + 1_000_000,
                },
            }
            if proxy is not None:
                value["proxy"] = proxy
            return value

        attempts = 100
        report: dict[str, object] = {
            "schema_version": 1,
            "run_id": run_id,
            "benchmark": MODULE.DATA_PATH_BENCHMARK,
            "evidence_kind": MODULE.DATA_PATH_EVIDENCE_KIND,
            "environment": {"system": "Linux"},
            "workload": {
                "udp": {"distinct_targets": MODULE.DATA_PATH_MIN_UDP_TARGETS},
            },
            "build": {
                "locked": True,
                "plus_extra_features": (
                    ["user-audit"] if plus_features is None else plus_features
                ),
                "artifacts": {
                    "plus_user_audit": {
                        "ssserver": {"sha256": "b" * 64, "bytes": 1},
                        "sslocal": {"sha256": "c" * 64, "bytes": 1},
                        "shadowsocks-auditd": {"sha256": "a" * 64, "bytes": 1},
                    }
                },
            },
            "cases": [
                {
                    "name": "locked_upstream",
                    "runtime_user_stats": False,
                    "runtime_user_audit": False,
                    "native_process_measurement": True,
                    "aggregate": aggregate(100.0, 1.0, 10_000),
                },
                {
                    "name": "plus_compiled_runtime_disabled",
                    "runtime_user_stats": False,
                    "runtime_user_audit": False,
                    "native_process_measurement": True,
                    "aggregate": aggregate(99.0, 1.02, 10_050),
                },
                {
                    "name": "plus_runtime_enabled",
                    "runtime_user_stats": True,
                    "runtime_user_audit": True,
                    "native_process_measurement": True,
                    "producer_user": "benchmark-producer",
                    "producer_uid": 1002,
                    "runtime_id": runtime_id,
                    "audit_ingest": ingest,
                    "aggregate": aggregate(
                        97.0,
                        1.05,
                        10_100,
                        {
                            "source": "worker_outcomes",
                            "attempts": attempts,
                            "successes": attempts - proxy_errors,
                            "errors": proxy_errors,
                        },
                    ),
                },
            ],
        }
        if include_auditd:
            enabled = report["cases"][2]["aggregate"]
            enabled["process_peak_rss_kib_max"]["auditd"] = 20_000
            enabled["process_rss_sample_count_min"] = {"auditd": 5}
            report["auditd"] = {
                "run_id": run_id,
                "measurement_source": "resource_monitor_pid",
                "pid": 1234,
                "process_start_time_ticks": 5678,
                "user": "benchmark-auditd",
                "uid": 1001,
                "producer_user": "benchmark-producer",
                "producer_uid": 1002,
                "export_user": "benchmark-export",
                "export_uid": 1003,
                "node_id": node_id,
                "executable_path": "/usr/local/bin/shadowsocks-auditd",
                "ingest_socket_path": "/run/shadowsocks-audit/ingest/ingest.sock",
                "ingest_socket_device": 7,
                "ingest_socket_inode": 11,
                "export_socket_path": "/run/shadowsocks-audit/export/export.sock",
                "export_socket_device": 7,
                "export_socket_inode": 12,
                "peak_rss_kib": 20_000,
                "rss_sample_count": 5,
                "executable_sha256": "a" * 64,
                "ingest": ingest,
            }
        return report

    @staticmethod
    def _run_with_data_path(report: dict[str, object]) -> dict[str, object]:
        with tempfile.TemporaryDirectory(prefix="ssrp-bench-test-") as directory:
            path = Path(directory) / "report.json"
            path.write_text(json.dumps(report), encoding="utf-8")
            args = type(
                "Args",
                (),
                {
                    "events": 200,
                    "producers": 2,
                    "queue_capacity": 16,
                    "spool_capacity": 8,
                    "data_path_report": path,
                    "require_linux": False,
                },
            )()
            return MODULE.run(args)

    def test_synthetic_scenarios_preserve_proxy_path_and_bounds(self) -> None:
        args = type(
            "Args",
            (),
            {
                "events": 200,
                "producers": 2,
                "queue_capacity": 16,
                "spool_capacity": 8,
                "data_path_report": None,
                "require_linux": False,
            },
        )()
        report = MODULE.run(args)
        self.assertIsNone(report["gate"])
        self.assertEqual(set(report["scenarios"]), set(MODULE.SCENARIOS))
        for scenario in report["scenarios"].values():
            self.assertNotIn("proxy_attempts", scenario)
            self.assertNotIn("proxy_successes", scenario)
            self.assertNotIn("proxy_errors", scenario)
            self.assertIsNone(scenario["proxy_path_gate"])
            self.assertTrue(scenario["gate"])
        self.assertEqual(report["scenarios"]["offline"]["queue_drops"], 0)
        self.assertEqual(report["scenarios"]["offline"]["spool_evictions"], 0)
        self.assertGreater(report["scenarios"]["queue_full"]["queue_drops"], 0)
        self.assertEqual(
            report["scenarios"]["queue_full"]["gap_records"],
            report["scenarios"]["queue_full"]["queue_drops"],
        )
        self.assertEqual(report["scenarios"]["healthy"]["acked"], 200)
        self.assertEqual(report["scenarios"]["healthy"]["queue_drops"], 0)
        self.assertLessEqual(report["queue_microbenchmark"]["feature_on"]["capacity"], 4096)

    def test_data_path_report_enforces_feature_thresholds(self) -> None:
        metrics = MODULE._evaluate_data_path(self._native_report())
        self.assertTrue(metrics["native_evidence_gate"])
        self.assertTrue(metrics["throughput_gate"])
        self.assertTrue(metrics["cpu_gate"])
        self.assertTrue(metrics["ssserver_rss_gate"])
        self.assertTrue(metrics["auditd_rss_gate"])
        self.assertTrue(metrics["proxy_path_gate"])
        self.assertEqual(metrics["locked_upstream_throughput_mib_s"], 100.0)
        self.assertAlmostEqual(metrics["cpu_increase_ratio"], 0.05)

    def test_missing_auditd_measurements_remain_indeterminate(self) -> None:
        report = self._run_with_data_path(self._native_report(include_auditd=False))
        metrics = report["data_path"]
        self.assertIsNone(metrics["auditd_rss_gate"])
        self.assertIsNone(metrics["native_evidence_gate"])
        self.assertIsNone(report["gate"])

    def test_user_stats_only_build_cannot_be_release_evidence(self) -> None:
        report = self._run_with_data_path(
            self._native_report(plus_features=["user-stats"])
        )
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertIsNone(report["gate"])

    def test_proxy_worker_error_fails_gate(self) -> None:
        report = self._run_with_data_path(self._native_report(proxy_errors=1))
        self.assertTrue(report["data_path"]["native_evidence_gate"])
        self.assertFalse(report["data_path"]["proxy_path_gate"])
        self.assertFalse(report["gate"])

    def test_missing_durable_ingest_delta_is_indeterminate(self) -> None:
        evidence = self._native_report()
        auditd = evidence["auditd"]
        assert isinstance(auditd, dict)
        ingest = auditd["ingest"]
        assert isinstance(ingest, dict)
        after = ingest["after"]
        before = ingest["before"]
        assert isinstance(after, dict) and isinstance(before, dict)
        after["stored_records"] = before["stored_records"]
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["audit_ingest_gate"])
        self.assertIsNone(report["data_path"]["native_evidence_gate"])

    def test_running_auditd_must_match_built_artifact(self) -> None:
        evidence = self._native_report()
        auditd = evidence["auditd"]
        assert isinstance(auditd, dict)
        auditd["executable_sha256"] = "d" * 64
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertTrue(
            any("current plus build" in issue for issue in report["data_path"]["evidence_issues"])
        )

    def test_producer_runtime_must_match_signed_health(self) -> None:
        evidence = self._native_report()
        cases = evidence["cases"]
        assert isinstance(cases, list) and isinstance(cases[2], dict)
        cases[2]["runtime_id"] = "4" * 32
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["audit_ingest_gate"])
        self.assertIsNone(report["data_path"]["native_evidence_gate"])

    def test_auditd_run_id_must_match_the_benchmark_run(self) -> None:
        evidence = self._native_report()
        auditd = evidence["auditd"]
        assert isinstance(auditd, dict)
        auditd["run_id"] = "9" * 32
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertIn(
            "auditd measurement run_id does not match benchmark",
            report["data_path"]["evidence_issues"],
        )

    def test_auditd_rss_must_be_a_live_process_sample(self) -> None:
        evidence = self._native_report()
        auditd = evidence["auditd"]
        assert isinstance(auditd, dict)
        auditd["measurement_source"] = "self_reported"
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertIn(
            "auditd RSS is not identified as a live process sample",
            report["data_path"]["evidence_issues"],
        )

    def test_three_service_identities_must_be_distinct(self) -> None:
        # Collapse each of the three UIDs onto one of the others in turn: the
        # gate must refuse evidence that cannot prove three separate accounts.
        for field, collision in (
            ("uid", 1003),
            ("producer_uid", 1003),
            ("export_uid", 1001),
        ):
            with self.subTest(field=field):
                evidence = self._native_report()
                auditd = evidence["auditd"]
                assert isinstance(auditd, dict)
                auditd[field] = collision
                if field == "producer_uid":
                    cases = evidence["cases"]
                    assert isinstance(cases, list) and isinstance(cases[2], dict)
                    cases[2]["producer_uid"] = collision
                report = self._run_with_data_path(evidence)
                self.assertIsNone(report["data_path"]["native_evidence_gate"])
                self.assertIn(
                    "auditd, producer and export identities are not independently bound",
                    report["data_path"]["evidence_issues"],
                )

    def test_ingest_socket_inode_is_required(self) -> None:
        evidence = self._native_report()
        auditd = evidence["auditd"]
        assert isinstance(auditd, dict)
        del auditd["ingest_socket_inode"]
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertTrue(
            any("socket inode" in issue for issue in report["data_path"]["evidence_issues"])
        )

    def test_single_udp_target_workload_cannot_be_release_evidence(self) -> None:
        evidence = self._native_report()
        workload = evidence["workload"]
        assert isinstance(workload, dict) and isinstance(workload["udp"], dict)
        workload["udp"]["distinct_targets"] = 1
        report = self._run_with_data_path(evidence)
        self.assertIsNone(report["data_path"]["native_evidence_gate"])
        self.assertIn(
            "workload did not exercise enough distinct UDP targets",
            report["data_path"]["evidence_issues"],
        )

    def test_missing_measurements_cannot_pass_enforcement_gate(self) -> None:
        args = type(
            "Args",
            (),
            {
                "events": 200,
                "producers": 2,
                "queue_capacity": 16,
                "spool_capacity": 8,
                "data_path_report": None,
                "require_linux": False,
            },
        )()
        report = MODULE.run(args)
        self.assertIsNone(report["queue_microbenchmark"]["gate"])
        self.assertIsNone(report["gate"])
        completed = subprocess.run(
            [
                sys.executable,
                str(MODULE_PATH),
                "--events",
                "200",
                "--producers",
                "2",
                "--queue-capacity",
                "16",
                "--spool-capacity",
                "8",
                "--enforce",
            ],
            capture_output=True,
            text=True,
            timeout=10,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("--data-path-report", completed.stderr)


if __name__ == "__main__":
    unittest.main()
