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
