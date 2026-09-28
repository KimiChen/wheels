"""Check acceptance arithmetic, timestamp envelopes, and sanitized failures."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("linux_acceptance", Path(__file__).with_name("linux_acceptance.py"))
acceptance = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(acceptance)


class LinuxAcceptanceTests(unittest.TestCase):
    def public_node(self):
        return {"id": "1", "name": "example", "groups": [], "session": "waiting", "freshness": "waiting",
                "last_seen": None, "metrics_at": None, "interval_seconds": 1,
                "frp": {"reconciliation": "", "server_online": None, "registered": 0,
                        "control_state": "unknown", "proxy_total": 0, "proxy_running": 0,
                        "today_rx_bytes": None, "today_tx_bytes": None, "traffic_scope": ""},
                "hardware": None, "metrics": None, "public_note": "",
                "traffic_today": {"day": "2026-09-28", "timezone": "UTC", "rx_bytes": "0",
                                  "tx_bytes": "0", "partial": True},
                "accounting_state": "ready"}

    def test_public_node_accepts_current_fields_and_optional_published_data(self):
        billing = {"price_minor": None, "currency": None, "billing_cycle": None,
                   "expires_at_ms": None, "renewal_note": ""}
        plan = {"quota_bytes": None, "mode": "max", "reset_mode": "manual", "reset_day": 1,
                "reset_timezone": "UTC", "period_start_at_ms": None, "period_end_at_ms": None,
                "rx_bytes": "0", "tx_bytes": "0", "used_bytes": "0", "partial": True}
        for optional in ({}, {"billing": billing}, {"traffic_plan": plan},
                         {"billing": billing, "traffic_plan": plan}):
            with self.subTest(optional_fields=tuple(optional)):
                acceptance.validate_public_node(dict(self.public_node(), **optional))

    def test_public_node_requires_fields_and_rejects_private_or_unknown_fields(self):
        for field in ("groups", "hardware", "public_note", "traffic_today", "accounting_state"):
            node = self.public_node()
            del node[field]
            with self.subTest(missing=field), self.assertRaisesRegex(acceptance.AcceptanceError, "^invalid_public_node_fields$"):
                acceptance.validate_public_node(node)
        for field in ("facts", "hostname", "private_note", "token_sha256", "unknown"):
            with self.subTest(extra=field), self.assertRaisesRegex(acceptance.AcceptanceError, "^invalid_public_node_fields$"):
                acceptance.validate_public_node(dict(self.public_node(), **{field: "private value"}))

    def test_invalid_public_fields_report_is_sanitized(self):
        for fields, error in (({"private_note": "private path and credentials"}, "invalid_public_node_fields"),
                              ({"groups": [{"id": "1", "name": "example", "node_ids": ["private"]}]}, "invalid_public_node_groups")):
            with self.subTest(error=error):
                output = io.StringIO()
                node = dict(self.public_node(), **fields)
                with mock.patch("sys.argv", ["linux_acceptance.py", "--url", "https://private.invalid"]), \
                        mock.patch.object(acceptance, "run", side_effect=lambda args: acceptance.validate_public_node(node)), \
                        contextlib.redirect_stdout(output):
                    self.assertEqual(acceptance.main(), 1)
                self.assertEqual(json.loads(output.getvalue()), {"schema": 1, "passed": False, "error": error})
                self.assertNotIn("private", output.getvalue())

    def test_public_groups_accepts_empty_and_multiple_public_references(self):
        for groups in ([], [{"id": "1", "name": "亚洲"}, {"id": str(2**63 - 1), "name": "实验 🛰"}]):
            with self.subTest(groups=groups):
                acceptance.validate_public_node(dict(self.public_node(), groups=groups))

    def test_public_groups_rejects_bad_shape_ids_names_and_private_fields(self):
        valid = {"id": "1", "name": "example"}
        invalid = [None, {}, "example", [None], [{}], [{"id": "1"}], [{"name": "example"}],
                   [dict(valid, node_ids=["2"])], [dict(valid, config_revision=1)], [dict(valid, unknown="value")],
                   [valid, dict(valid)], [dict(valid, id=str(index + 1)) for index in range(129)]]
        invalid += [[dict(valid, id=value)] for value in (None, True, 1, "", "0", "01", "-1", "1.0", str(2**63))]
        invalid += [[dict(valid, name=value)] for value in (None, True, 1, "", " ", " example", "example ",
                                                           "a\nb", "a\x7fb", "a\x85b", "a" * 129, "亚" * 43)]
        for groups in invalid:
            with self.subTest(groups=groups), self.assertRaisesRegex(acceptance.AcceptanceError, "^invalid_public_node_groups$"):
                acceptance.validate_public_node(dict(self.public_node(), groups=groups))

    def test_memory_zero_available_and_missing_fallback(self):
        text = "MemTotal: 100 kB\nMemAvailable: 0 kB\nMemFree: 10 kB\nBuffers: 5 kB\nCached: 15 kB\nSwapTotal: 20 kB\nSwapFree: 8 kB\n"
        self.assertEqual(acceptance.meminfo(text)["mem_used"], 100 * 1024)
        self.assertEqual(acceptance.meminfo(text.replace("MemAvailable: 0 kB\n", ""))["mem_used"], 70 * 1024)
        self.assertEqual(acceptance.meminfo(text)["swap_used"], 12 * 1024)

    def test_cpu_does_not_double_count_guest_and_includes_iowait_in_idle(self):
        self.assertEqual(acceptance.cpu_counters("cpu 10 2 8 50 5 2 3 1 8 1\n"), (81, 55))

    def test_mount_filter_deduplication_and_overmounts(self):
        text = "/dev/root / ext4 rw 0 0\n/dev/root /bind ext4 rw 0 0\n/dev/data /with\\040space xfs rw 0 0\n/dev/old /hidden ext4 rw 0 0\ntmpfs /hidden tmpfs rw 0 0\nserver:/share /remote nfs4 rw 0 0\npool/first /z1 zfs rw 0 0\npool/second /z2 zfs rw 0 0\n"
        self.assertEqual(acceptance.mount_points(text), ["/", "/with space", "/z1"])

    def test_quality_requires_null_and_uint64_requires_exact_decimal_string(self):
        self.assertEqual(acceptance.metric_value("mem_total", {"quality": "ok", "value": str(2**64 - 1)}), 2**64 - 1)
        for value in (2**53, "01", str(2**64), "-1"):
            with self.subTest(value=value), self.assertRaises(acceptance.AcceptanceError):
                acceptance.metric_value("mem_total", {"quality": "ok", "value": value})
        with self.assertRaises(acceptance.AcceptanceError):
            acceptance.metric_value("cpu", {"quality": "unavailable", "value": 0})

    def test_counter_envelope_is_exact_and_static_total_mismatch_fails(self):
        values = {key: 100 for key in acceptance.UINT_FIELDS}
        values.update(cpu=12.0, load=[0.1, 0.2, 0.3])
        metrics = {key: {"quality": "ok", "value": str(value) if key in acceptance.UINT_FIELDS else value} for key, value in values.items()}
        metrics["scope"] = "unknown"
        refs = [{"values": dict(values)}, {"values": dict(values, net_rx_total=120)}]
        metrics["net_rx_total"]["value"] = "121"
        metrics["mem_total"]["value"] = "101"
        result = {}
        acceptance.compare(metrics, refs, result)
        self.assertEqual(result["net_rx_total"]["failures"], 1)
        self.assertEqual(result["net_rx_total"]["tolerance"], 0)
        self.assertEqual(result["mem_total"]["failures"], 1)
        self.assertEqual(result["cpu"]["failures"], 0)
        metrics["net_rx_total"]["value"] = "110"
        result = {}
        acceptance.compare(metrics, refs, result)
        self.assertEqual(result["net_rx_total"]["failures"], 0)

    def test_failure_report_does_not_echo_url_path_or_exception(self):
        output = io.StringIO()
        with mock.patch("sys.argv", ["linux_acceptance.py", "--url", "https://private.invalid"]), \
                mock.patch.object(acceptance, "run", side_effect=OSError("private path and credentials")), contextlib.redirect_stdout(output):
            self.assertEqual(acceptance.main(), 1)
        result = json.loads(output.getvalue())
        self.assertEqual(result, {"schema": 1, "passed": False, "error": "reference_or_api_read_failed"})
        self.assertNotIn("private", output.getvalue())


if __name__ == "__main__":
    unittest.main()
