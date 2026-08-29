#!/usr/bin/env python3
"""Unit checks for the native data-path benchmark evidence producer.

``benchmark_data_path.py`` only runs end to end on a Linux host with root and a
live auditd, so the parts that can be exercised anywhere are covered here: the
workload description, the tunnel configuration it derives, and the loopback
worker that has to reach every audit target.
"""

from __future__ import annotations

import importlib.util
import json
import socket
import stat
import sys
import tempfile
import threading
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
TESTS = ROOT / "tests"
if str(TESTS) not in sys.path:
    sys.path.insert(0, str(TESTS))


def _load(name: str) -> object:
    spec = importlib.util.spec_from_file_location(name, TESTS / f"{name}.py")
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules.setdefault(name, module)
    spec.loader.exec_module(module)
    return module


MODULE = _load("benchmark_data_path")
GATE = _load("benchmark_audit")


def _workload(**overrides: int) -> object:
    values: dict[str, int] = {
        "tcp_workers": 2,
        "udp_workers": 2,
        "tcp_bytes_per_worker": 1024,
        "tcp_chunk_bytes": 256,
        "udp_datagrams_per_worker": 8,
        "udp_payload_bytes": 32,
        "udp_targets": 4,
    }
    values.update(overrides)
    return MODULE.Workload(**values)


class CountingEchoTargets:
    """UDP echo endpoints that record how many datagrams each one served."""

    def __init__(self, count: int) -> None:
        self.sockets: list[socket.socket] = []
        for _ in range(count):
            endpoint = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            endpoint.bind(("127.0.0.1", 0))
            endpoint.settimeout(0.2)
            self.sockets.append(endpoint)
        self.ports = tuple(int(item.getsockname()[1]) for item in self.sockets)
        self.counts = [0] * count
        self.stop_event = threading.Event()
        self.threads = [
            threading.Thread(target=self._serve, args=(index,), daemon=True)
            for index in range(count)
        ]

    def _serve(self, index: int) -> None:
        endpoint = self.sockets[index]
        while not self.stop_event.is_set():
            try:
                data, peer = endpoint.recvfrom(65_535)
            except socket.timeout:
                continue
            except OSError:
                return
            self.counts[index] += 1
            endpoint.sendto(data, peer)

    def __enter__(self) -> CountingEchoTargets:
        for thread in self.threads:
            thread.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.stop_event.set()
        for endpoint in self.sockets:
            endpoint.close()
        for thread in self.threads:
            thread.join(timeout=2)


class DataPathWorkloadTest(unittest.TestCase):
    def test_default_workload_satisfies_the_release_target_floor(self) -> None:
        self.assertGreaterEqual(
            MODULE.DEFAULT_UDP_TARGETS,
            GATE.DATA_PATH_MIN_UDP_TARGETS,
        )

    def test_workload_report_publishes_the_distinct_udp_target_count(self) -> None:
        report = _workload(udp_targets=9).report()
        self.assertEqual(report["udp"]["distinct_targets"], 9)

    def test_local_config_creates_one_tunnel_per_udp_target(self) -> None:
        config = MODULE.local_config([4001, 4002, 4003], 5000, [6001, 6002, 6003], "k")
        locals_ = config["locals"]
        self.assertEqual(len(locals_), 3)
        self.assertEqual([item["local_port"] for item in locals_], [4001, 4002, 4003])
        self.assertEqual([item["forward_port"] for item in locals_], [6001, 6002, 6003])
        for item in locals_:
            self.assertEqual(item["mode"], "tcp_and_udp")
            self.assertEqual(item["protocol"], "tunnel")

    def test_local_config_rejects_a_port_list_mismatch(self) -> None:
        with self.assertRaises(RuntimeError):
            MODULE.local_config([4001], 5000, [6001, 6002], "k")
        with self.assertRaises(RuntimeError):
            MODULE.local_config([], 5000, [], "k")
        with self.assertRaises(RuntimeError):
            MODULE.local_config([4001, 4001], 5000, [6001, 6002], "k")

    def test_echo_services_expose_every_distinct_udp_target(self) -> None:
        with MODULE.EchoServices(4) as echo:
            self.assertEqual(len(echo.udp_ports), 4)
            self.assertEqual(len(set(echo.udp_ports)), 4)
            self.assertEqual(echo.udp_ports[0], echo.port)
            for index, port in enumerate(echo.udp_ports):
                with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as client:
                    client.settimeout(5)
                    client.connect(("127.0.0.1", port))
                    payload = f"target-{index}".encode("ascii")
                    client.send(payload)
                    self.assertEqual(client.recv(1024), payload)

    def test_udp_worker_rotates_over_every_target(self) -> None:
        with CountingEchoTargets(4) as targets:
            gate = MODULE.StartGate(1)
            gate.release()
            result = MODULE.udp_worker(0, targets.ports, 12, 32, gate)
            self.assertEqual(result["bytes_each_direction"], 12 * 32)
            self.assertEqual(targets.counts, [3, 3, 3, 3])

    def test_udp_worker_requires_at_least_one_target(self) -> None:
        gate = MODULE.StartGate(1)
        gate.release()
        with self.assertRaises(RuntimeError):
            MODULE.udp_worker(0, [], 1, 32, gate)


def _health(**overrides: object) -> dict[str, object]:
    value: dict[str, object] = {
        "http_status": 200,
        "status": "ok",
        "node_id": "benchmark-node",
        "producer_connected": True,
        "producer_runtime_id": "2" * 32,
        "last_ingest_at_unix_ms": 2_000,
        "spool_epoch": "3" * 32,
        "stored_records": 44,
        "storage_rejected_attempts": 0,
        "evicted_unacked_records": 0,
    }
    value.update(overrides)
    return value


def _before() -> dict[str, object]:
    return _health(
        producer_connected=False,
        producer_runtime_id=None,
        last_ingest_at_unix_ms=1_000,
        stored_records=40,
    )


def _snapshot(**user_overrides: object) -> dict[str, object]:
    user: dict[str, object] = {
        "name": "benchmark-user",
        "tcp_uplink_bytes": 1,
        "tcp_downlink_bytes": 2,
        "udp_uplink_bytes": 3,
        "udp_downlink_bytes": 4,
    }
    user.update(user_overrides)
    return {"servers": [{"users": [user]}]}


def _sample(**overrides: object) -> dict[str, object]:
    value: dict[str, object] = {
        "wall_seconds": 1.0,
        "bidirectional_mib_per_second": 10.0,
        "protocols": {
            "tcp": {"bidirectional_mib_per_second": 6.0},
            "udp": {"bidirectional_mib_per_second": 4.0},
        },
        "process_cpu_seconds": {"ssserver": 0.5, "sslocal": 0.5, "combined": 1.0},
        "process_rss": {
            "peak_kib": {"ssserver": 100, "sslocal": 100},
            "sample_count": {"ssserver": 5, "sslocal": 5},
            "peak_combined_kib": 200,
        },
        "proxy": {"source": "worker_outcomes", "attempts": 2, "successes": 2, "errors": 0},
    }
    value.update(overrides)
    return value


# One negative case per clause of ``_audit_ingest_evidence``. The signed health
# delta is the only proof that the benchmark run actually reached auditd, so
# every clause has to be able to reject a report on its own.
INGEST_EVIDENCE_MUTATIONS: tuple[tuple[str, dict[str, object], dict[str, object]], ...] = (
    ("producer_connected_before", {"producer_connected": True}, {}),
    ("http_status_after", {}, {"http_status": 503}),
    ("status_after", {}, {"status": "degraded"}),
    ("producer_connected_after", {}, {"producer_connected": False}),
    ("producer_runtime_id_after", {}, {"producer_runtime_id": "9" * 32}),
    ("spool_epoch_changed", {}, {"spool_epoch": "4" * 32}),
    ("stored_records_before_type", {"stored_records": "40"}, {}),
    ("stored_records_after_type", {}, {"stored_records": None}),
    ("stored_records_not_advanced", {}, {"stored_records": 40}),
    ("last_ingest_after_type", {}, {"last_ingest_at_unix_ms": None}),
    ("last_ingest_not_advanced", {}, {"last_ingest_at_unix_ms": 1_000}),
    ("storage_rejected_attempts", {}, {"storage_rejected_attempts": 1}),
    ("evicted_unacked_records", {}, {"evicted_unacked_records": 1}),
)


class DataPathEvidenceTest(unittest.TestCase):
    def test_signed_ingest_evidence_accepts_a_durable_delta(self) -> None:
        evidence = MODULE._audit_ingest_evidence(_before(), _health(), "2" * 32)
        assert evidence is not None
        self.assertEqual(evidence["source"], "signed_health_stored_records_delta")
        self.assertEqual(evidence["stored_records_delta"], 4)
        self.assertIs(evidence["last_ingest_advanced"], True)

    def test_every_ingest_evidence_clause_can_reject(self) -> None:
        for name, before_overrides, after_overrides in INGEST_EVIDENCE_MUTATIONS:
            with self.subTest(clause=name):
                before = _before()
                before.update(before_overrides)
                after = _health(**after_overrides)
                self.assertIsNone(
                    MODULE._audit_ingest_evidence(before, after, "2" * 32),
                    "a mutated health pair must not become ingest evidence",
                )

    def test_ingest_evidence_requires_the_measured_runtime_identity(self) -> None:
        self.assertIsNone(MODULE._audit_ingest_evidence(_before(), _health(), "9" * 32))


class DataPathConfigTest(unittest.TestCase):
    def test_user_audit_requires_user_statistics_identity(self) -> None:
        with self.assertRaises(RuntimeError):
            MODULE.server_config(
                8388,
                "identity",
                "user",
                "node",
                None,
                Path("/run/shadowsocks-audit/ingest/ingest.sock"),
                "shadowsocks-audit",
            )

    def test_ingest_socket_and_auditd_user_must_arrive_together(self) -> None:
        for socket_path, auditd_user in (
            (Path("/run/shadowsocks-audit/ingest/ingest.sock"), None),
            (None, "shadowsocks-audit"),
        ):
            with self.subTest(auditd_user=auditd_user):
                with self.assertRaises(RuntimeError):
                    MODULE.server_config(
                        8388,
                        "identity",
                        "user",
                        "node",
                        Path("/tmp/user-stats.sock"),
                        socket_path,
                        auditd_user,
                    )

    def test_enabled_server_config_carries_both_runtime_sections(self) -> None:
        config = MODULE.server_config(
            8388,
            "identity",
            "user",
            "node",
            Path("/tmp/user-stats.sock"),
            Path("/run/shadowsocks-audit/ingest/ingest.sock"),
            "shadowsocks-audit",
        )
        self.assertEqual(config["user_stats"]["node_id"], "node")
        self.assertEqual(config["servers"][0]["id"], "benchmark-entry")
        self.assertEqual(
            config["user_audit"],
            {
                "ingest_socket_path": "/run/shadowsocks-audit/ingest/ingest.sock",
                "auditd_user": "shadowsocks-audit",
            },
        )


class DataPathMeasurementTest(unittest.TestCase):
    def test_expected_counter_delta_tracks_the_reported_workload(self) -> None:
        workload = _workload(udp_targets=4)
        report = workload.report()
        delta = workload.expected_counter_delta(3)
        self.assertEqual(delta["tcp_uplink_bytes"], report["tcp"]["bytes_per_sample_per_direction"] * 3)
        self.assertEqual(delta["udp_uplink_bytes"], report["udp"]["bytes_per_sample_per_direction"] * 3)
        self.assertEqual(delta["tcp_uplink_bytes"], delta["tcp_downlink_bytes"])
        self.assertEqual(delta["udp_uplink_bytes"], delta["udp_downlink_bytes"])

    def test_snapshot_user_counters_reads_the_benchmark_user(self) -> None:
        self.assertEqual(
            MODULE.snapshot_user_counters(_snapshot()),
            {
                "tcp_uplink_bytes": 1,
                "tcp_downlink_bytes": 2,
                "udp_uplink_bytes": 3,
                "udp_downlink_bytes": 4,
            },
        )

    def test_snapshot_user_counters_rejects_unusable_snapshots(self) -> None:
        cases: tuple[tuple[str, dict[str, object]], ...] = (
            ("no_servers", {"servers": []}),
            ("two_servers", {"servers": [{"users": []}, {"users": []}]}),
            ("server_not_object", {"servers": ["nope"]}),
            ("two_users", {"servers": [{"users": [{}, {}]}]}),
        )
        for name, snapshot in cases:
            with self.subTest(case=name):
                with self.assertRaises(RuntimeError):
                    MODULE.snapshot_user_counters(snapshot)
        for name, override in (
            ("other_user", {"name": "someone-else"}),
            ("negative_counter", {"udp_uplink_bytes": -1}),
            ("boolean_counter", {"udp_uplink_bytes": True}),
            ("missing_counter", {"udp_uplink_bytes": None}),
        ):
            with self.subTest(case=name):
                with self.assertRaises(RuntimeError):
                    MODULE.snapshot_user_counters(_snapshot(**override))

    def test_aggregate_samples_requires_worker_outcomes(self) -> None:
        samples = [_sample(), _sample()]
        aggregate = MODULE.aggregate_samples(samples)
        self.assertEqual(aggregate["samples"], 2)
        self.assertEqual(aggregate["proxy"]["attempts"], 4)
        self.assertEqual(aggregate["process_peak_rss_kib_max"]["ssserver"], 100)
        samples[1].pop("proxy")
        with self.assertRaises(RuntimeError):
            MODULE.aggregate_samples(samples)

    def test_distribution_reports_median_and_bounds(self) -> None:
        self.assertEqual(
            MODULE.distribution([3.0, 1.0, 2.0], 3),
            {"median": 2.0, "min": 1.0, "max": 3.0},
        )

    def test_repository_test_script_runs_this_suite(self) -> None:
        # The evidence producer had no test and no gate ran it; keep the
        # wiring itself under test so it cannot silently disappear again.
        script = (ROOT / "scripts" / "test.sh").read_text(encoding="utf-8")
        self.assertIn("tests/test_benchmark_data_path.py", script)

    def test_secure_write_json_keeps_owner_only_permissions(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-dp-test-") as directory:
            path = Path(directory) / "config.json"
            MODULE.secure_write_json(path, {"secret": "value"})
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"secret": "value"})


if __name__ == "__main__":
    unittest.main()
