#!/usr/bin/env python3
"""Unit checks for the native data-path benchmark evidence producer.

``benchmark_data_path.py`` only runs end to end on a Linux host with root and a
live auditd, so the parts that can be exercised anywhere are covered here: the
workload description, the tunnel configuration it derives, and the loopback
worker that has to reach every audit target.
"""

from __future__ import annotations

import importlib.util
import socket
import sys
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


if __name__ == "__main__":
    unittest.main()
