#!/usr/bin/env python3
"""Unit checks for the portable audit benchmark gate."""

from __future__ import annotations

import importlib.util
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
        self.assertTrue(report["gate"])
        self.assertEqual(set(report["scenarios"]), set(MODULE.SCENARIOS))
        for scenario in report["scenarios"].values():
            self.assertEqual(scenario["proxy_errors"], 0)
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


if __name__ == "__main__":
    unittest.main()
