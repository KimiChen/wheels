"""参考 collector 的故障矩阵测试（README §8）。"""

from __future__ import annotations

import json
import os
import socket
import sys
import tempfile
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixtures import encode, snapshot  # noqa: E402
from reference_collector import Ledger, collect_once, report  # noqa: E402
from settlement_model import Collector  # noqa: E402


class SnapshotServer:
    """按序回放若干份响应的 UDS 服务端。"""

    def __init__(self, responses: list[bytes]) -> None:
        self.directory = tempfile.mkdtemp(prefix="sbp", dir="/tmp")
        self.path = os.path.join(self.directory, "s.sock")
        self.responses = list(responses)
        self.socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.socket.bind(self.path)
        self.socket.listen(8)
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        while True:
            try:
                connection, _ = self.socket.accept()
            except OSError:
                return
            with connection:
                try:
                    connection.recv(65536)
                    if self.responses:
                        connection.sendall(self.responses.pop(0))
                except OSError:
                    pass

    def close(self) -> None:
        self.socket.close()
        try:
            os.unlink(self.path)
        except OSError:
            pass
        os.rmdir(self.directory)


def http_ok(body: bytes) -> bytes:
    return (
        b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
        + f"Content-Length: {len(body)}\r\n".encode()
        + b"Connection: close\r\n\r\n"
        + body
    )


class ReferenceCollectorTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ledger_dir = Path(tempfile.mkdtemp(prefix="sbp", dir="/tmp"))

    def _records(self) -> list[dict]:
        path = self.ledger_dir / "ledger.jsonl"
        if not path.is_file():
            return []
        return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]

    def test_accepts_and_diffs(self):
        first = snapshot(sequence=1)
        second = snapshot(sequence=2)
        second["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 150
        server = SnapshotServer([http_ok(encode(first)), http_ok(encode(second))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collector = Collector(first_snapshot="baseline")

        self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 0)
        self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 0)
        records = self._records()
        self.assertEqual([record["result"] for record in records], ["accepted", "accepted"])
        # 首份走 baseline 策略只建基线；第二份只入账 50 字节的增量。
        self.assertEqual(sum(records[0]["total"].values()), 0)
        self.assertEqual(records[1]["total"]["tcp_uplink_bytes"], 50)

    def test_transport_error_is_recorded_not_swallowed(self):
        # 「本次采集失败」与「本次没有流量」必须在账本里可区分。
        server = SnapshotServer([b"HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n{}"])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collector = Collector()
        self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 1)
        self.assertEqual(self._records()[0]["result"], "transport_error")

    def test_schema_rejection_is_recorded(self):
        bad = snapshot()
        bad["schema_version"] = 1
        server = SnapshotServer([http_ok(encode(bad))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        self.assertEqual(collect_once(server.path, ledger, Collector(), 5.0), 1)
        self.assertEqual(self._records()[0]["result"], "schema_rejected")

    def test_regression_fails_closed_and_report_flags_it(self):
        first = snapshot(sequence=1)
        second = snapshot(sequence=2)
        second["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 1
        server = SnapshotServer([http_ok(encode(first)), http_ok(encode(second))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collector = Collector(first_snapshot="include")
        self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 0)
        self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 2)
        self.assertEqual(self._records()[1]["result"], "failed_closed")
        self.assertEqual(report(ledger), 1)

    def test_429_is_retryable_and_not_billed(self):
        server = SnapshotServer([b"HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\n\r\n"])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        self.assertEqual(collect_once(server.path, ledger, Collector(), 5.0), 0)
        self.assertEqual(self._records()[0]["result"], "retryable")

    def test_report_on_clean_run(self):
        first = snapshot(sequence=1)
        server = SnapshotServer([http_ok(encode(first))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collect_once(server.path, ledger, Collector(), 5.0)
        self.assertEqual(report(ledger), 0)


if __name__ == "__main__":
    unittest.main()
