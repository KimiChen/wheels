#!/usr/bin/env python3
"""Unit checks for the portable audit benchmark gate."""

from __future__ import annotations

import importlib.util
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
            self.assertEqual(scenario["proxy_errors"], 0)
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
        report = {
            "cases": [
                {
                    "name": "plus_compiled_runtime_disabled",
                    "aggregate": {
                        "bidirectional_mib_per_second": {"median": 100.0},
                        "process_cpu_seconds_median": {"combined": 1.0},
                        "process_peak_rss_kib_max": {"combined": 10_000},
                    },
                },
                {
                    "name": "plus_runtime_enabled",
                    "aggregate": {
                        "bidirectional_mib_per_second": {"median": 97.0},
                        "process_cpu_seconds_median": {"combined": 1.05},
                        "process_peak_rss_kib_max": {"combined": 10_100},
                    },
                },
            ]
        }
        with tempfile.TemporaryDirectory(prefix="ssrp-bench-test-") as directory:
            path = Path(directory) / "report.json"
            path.write_text(__import__("json").dumps(report), encoding="utf-8")
            metrics = MODULE._evaluate_data_path(MODULE._read_data_path_report(path))
        self.assertTrue(metrics["throughput_gate"])
        self.assertTrue(metrics["cpu_gate"])
        self.assertTrue(metrics["ssserver_rss_gate"])

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
