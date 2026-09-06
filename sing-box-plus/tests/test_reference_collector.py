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
from reference_collector import Ledger, QuotaPlan, collect_once, report  # noqa: E402
from settlement_model import Collector, SnapshotRejected  # noqa: E402


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

    def test_state_survives_process_boundary(self):
        """每次采集起一个新进程时，基线必须能从账本恢复。

        这是 scripts/soak.sh 的用法。不恢复的话，每一轮都会被当成该 runtime 的首快照：
        baseline 策略下永远只建基线、永不入账，而 --report 的四项判据依然全是 0——
        一个看起来完全正常、实际一个字节都没记的采集器。
        """
        first = snapshot(sequence=1)
        second = snapshot(sequence=2)
        second["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 150
        third = snapshot(sequence=3)
        third["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 275
        server = SnapshotServer([http_ok(encode(first)), http_ok(encode(second)), http_ok(encode(third))])
        self.addCleanup(server.close)

        for _ in range(3):
            # 每一轮都新建 Ledger 与 Collector，模拟独立进程。
            ledger = Ledger(self.ledger_dir)
            collector = Collector(first_snapshot="baseline")
            collector.restore_state(ledger.load_state())
            self.assertEqual(collect_once(server.path, ledger, collector, 5.0), 0)

        records = self._records()
        self.assertEqual([record["result"] for record in records], ["accepted"] * 3)
        self.assertEqual(sum(records[0]["total"].values()), 0, "首快照按 baseline 只建基线")
        self.assertEqual(records[1]["total"]["tcp_uplink_bytes"], 50)
        self.assertEqual(records[2]["total"]["tcp_uplink_bytes"], 125)

    def test_state_refuses_strategy_switch(self):
        # 中途换 first_snapshot 会同时改变「首快照记不记账」与基线含义，必须显式重来。
        ledger = Ledger(self.ledger_dir)
        ledger.save_state(Collector(first_snapshot="baseline").export_state())
        with self.assertRaises(SnapshotRejected):
            Collector(first_snapshot="include").restore_state(ledger.load_state())

    def test_quota_push_uses_period_usage_not_snapshot_absolute(self):
        """§5.4 第 1 条：剩余额度必须按采集端自己累计的用量算。

        拿快照绝对值去减是错的——进程重启后快照从零开始，额度会被凭空补满。
        这里用两个 runtime 覆盖这一点：第二个 runtime 的快照绝对值很小，
        但本周期已用应当把第一个 runtime 的量也算进去。
        """
        first = snapshot(sequence=1)
        second = snapshot(sequence=2)
        second["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 1000
        # 新 runtime：绝对值回到很小，但已用不该因此清零。
        third = snapshot(sequence=1, runtime_id="ffffffffffffffffffffffffffffffff")
        third["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 7
        third["inbounds"][1]["users"][0]["tcp_downlink_bytes"] = 0

        server = SnapshotServer([http_ok(encode(first)), http_ok(encode(second)), http_ok(encode(third))])
        self.addCleanup(server.close)
        quota = SnapshotServer([http_ok(b'{"schema_version":2,"epoch":1,"applied":2}\n')] * 3)
        self.addCleanup(quota.close)

        ledger = Ledger(self.ledger_dir)
        collector = Collector(first_snapshot="include")
        for _ in range(3):
            collect_once(server.path, ledger, collector, 5.0, quota.path, QuotaPlan(10_000))

        pushes = [r for r in self._records() if r["result"] == "quota_pushed"]
        self.assertEqual(len(pushes), 3)
        # epoch 必须单调递增，否则服务端会以 409 拒绝后续下发。
        self.assertEqual([p["epoch"] for p in pushes], [1, 2, 3])

        used = collector.usage[("node-example-01", "vless-entry-01", "u_example_01")]
        # include 策略下第一份记满 300（tcp 100 上 / 200 下）；第二份上行涨到 1000，+900；
        # 第三份是新 runtime 的首份，绝对值 7/0 全记，+7。跨 runtime 累加正是这条断言的重点。
        self.assertEqual(used, 300 + 900 + 7)
        self.assertEqual(
            collector.remaining_for("node-example-01", "vless-entry-01", "u_example_01", 10_000),
            10_000 - used,
        )

    def test_quota_remaining_floors_at_zero(self):
        used_key = ("node-example-01", "vless-entry-01", "u_example_01")
        collector = Collector(first_snapshot="include")
        collector.usage[used_key] = 5_000
        self.assertEqual(collector.remaining_for(*used_key, 1_000), 0)

    def test_quota_epoch_survives_process_boundary(self):
        # 每次采集起一个新进程时 epoch 若从 1 重来，服务端会以 409 拒绝，
        # 配额就永远停在第一次下发的值上。
        ledger = Ledger(self.ledger_dir)
        first = Collector(first_snapshot="baseline")
        first.next_quota_epoch()
        first.next_quota_epoch()
        ledger.save_state(first.export_state())
        second = Collector(first_snapshot="baseline")
        second.restore_state(ledger.load_state())
        self.assertEqual(second.next_quota_epoch(), 3)

    def test_reset_period_clears_usage_but_keeps_baselines(self):
        """开新计费周期只清已用量。

        基线一并清掉的话，下一次采集会把快照里的全部历史累计值当成一次巨额增量重新入账。
        """
        first = snapshot(sequence=1)
        second = snapshot(sequence=2)
        second["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 1000
        server = SnapshotServer([http_ok(encode(first)), http_ok(encode(second))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collector = Collector(first_snapshot="include")
        collect_once(server.path, ledger, collector, 5.0)

        saved = ledger.load_state()
        self.assertTrue(any(entry["bytes"] > 0 for entry in saved["usage"]))
        baseline_count = sum(len(item["baselines"]) for item in saved["runtimes"])

        collector.usage.clear()
        ledger.save_state(collector.export_state())
        after = ledger.load_state()
        self.assertEqual(after["usage"], [])
        self.assertEqual(sum(len(item["baselines"]) for item in after["runtimes"]), baseline_count)

        # 清零之后继续采集，入账的仍然只是增量而不是历史总量。
        fresh = Collector(first_snapshot="include")
        fresh.restore_state(after)
        collect_once(server.path, ledger, fresh, 5.0)
        self.assertEqual(self._records()[-1]["total"]["tcp_uplink_bytes"], 900)

    def test_quota_plan_overrides_are_per_identity(self):
        """改一个人的额度不应该动到别人。

        真实部署里不同用户的套餐不同；只有一个全局数字时，把某个身份降到 1 GiB
        会连带把所有人一起降下去。
        """
        plan = QuotaPlan(100 * 2**30, {"vless-entry-01/u_example_01": 2**30})
        self.assertEqual(plan.quota_for("vless-entry-01", "u_example_01"), 2**30)
        self.assertEqual(plan.quota_for("vless-entry-01", "u_example_02"), 100 * 2**30)
        self.assertEqual(plan.quota_for("ss-in", "s1"), 100 * 2**30)

    def test_quota_plan_file_fails_closed(self):
        directory = Path(tempfile.mkdtemp(prefix="sbp", dir="/tmp"))
        path = directory / "plan.json"

        good = {"schema_version": 1, "default_bytes": 10, "overrides": {"a/b": 5}}
        path.write_text(json.dumps(good), encoding="utf-8")
        self.assertEqual(QuotaPlan.load(path).quota_for("a", "b"), 5)

        for name, payload in {
            "未知字段": {"schema_version": 1, "default_bytes": 10, "typo_overrides": {}},
            "版本不符": {"schema_version": 2, "default_bytes": 10},
            "默认额度为负": {"schema_version": 1, "default_bytes": -1},
            "覆盖键缺分隔符": {"schema_version": 1, "default_bytes": 10, "overrides": {"ab": 5}},
            "覆盖值是布尔": {"schema_version": 1, "default_bytes": 10, "overrides": {"a/b": True}},
        }.items():
            path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(ValueError, msg=name):
                QuotaPlan.load(path)

    def test_quota_plan_pushes_different_remaining_per_identity(self):
        first = snapshot(sequence=1)
        server = SnapshotServer([http_ok(encode(first))])
        self.addCleanup(server.close)
        quota = SnapshotServer([http_ok(b'{"schema_version":2,"epoch":1,"applied":3}\n')])
        self.addCleanup(quota.close)
        ledger = Ledger(self.ledger_dir)
        collector = Collector(first_snapshot="include")
        plan = QuotaPlan(1000, {"vless-entry-01/u_example_01": 350})
        collect_once(server.path, ledger, collector, 5.0, quota.path, plan)

        # u_example_01 首份就记满 300（tcp 100/200），额度 350 → 剩 50；
        # 其余身份走默认 1000。
        self.assertEqual(
            collector.remaining_for("node-example-01", "vless-entry-01", "u_example_01", 350), 50
        )
        self.assertEqual(
            collector.remaining_for("node-example-01", "ss-in", "s1", 1000), 1000
        )

    def test_report_on_clean_run(self):
        first = snapshot(sequence=1)
        server = SnapshotServer([http_ok(encode(first))])
        self.addCleanup(server.close)
        ledger = Ledger(self.ledger_dir)
        collect_once(server.path, ledger, Collector(), 5.0)
        self.assertEqual(report(ledger), 0)


if __name__ == "__main__":
    unittest.main()
