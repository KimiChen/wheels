"""§4.5 / §5.1 的契约测试。每一条都对应一句规范，而不是对实现的复述。"""

from __future__ import annotations

import copy
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from fixtures import encode, snapshot  # noqa: E402
from settlement_model import (  # noqa: E402
    Collector,
    SettlementFailure,
    SnapshotRejected,
    batch_id,
    parse_snapshot,
)


class ParseSnapshotTest(unittest.TestCase):
    def test_accepts_reference_snapshot(self):
        parsed = parse_snapshot(encode(snapshot()))
        self.assertEqual(parsed["schema_version"], 2)
        self.assertEqual(len(parsed["inbounds"]), 2)

    def test_rejects_schema_version_one(self):
        # 下游若已对接 shadowsocks-rust-plus，其 schema_version 约束须先放开为同时接受 2；
        # 反过来，本校验器也必须拒绝 1，否则两套快照会被混入同一个账本。
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(snapshot(schema_version=1)))

    def test_rejects_extra_and_missing_top_level_keys(self):
        extra = snapshot()
        extra["identity_kind"] = "user"
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(extra))
        missing = snapshot()
        del missing["health"]
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(missing))

    def test_health_is_closed_set(self):
        value = snapshot()
        value["health"]["new_flag"] = False
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(value))

    def test_rejects_float_and_bool_counters(self):
        for bad in (1.0, True, "5"):
            value = snapshot()
            value["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = bad
            with self.assertRaises(SnapshotRejected):
                parse_snapshot(encode(value))

    def test_rejects_unsorted_users(self):
        value = snapshot()
        value["inbounds"][1]["users"].reverse()
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(value))

    def test_rejects_unsorted_inbounds(self):
        value = snapshot()
        value["inbounds"].reverse()
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(value))

    def test_rejects_listen_with_port(self):
        value = snapshot()
        value["inbounds"][0]["listen"] = "127.0.0.1:8388"
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(encode(value))

    def test_rejects_bad_runtime_id(self):
        for bad in ("ABCDEF0123456789abcdef0123456789", "short", "0123456789abcdef0123456789abcdeg"):
            with self.assertRaises(SnapshotRejected):
                parse_snapshot(encode(snapshot(runtime_id=bad)))

    def test_rejects_truncated_json(self):
        raw = encode(snapshot())[:-20]
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(raw)

    def test_rejects_duplicate_keys(self):
        raw = b'{"schema_version":2,"schema_version":2}'
        with self.assertRaises(SnapshotRejected):
            parse_snapshot(raw)


class CollectorTest(unittest.TestCase):
    def test_baseline_strategy_does_not_bill_first_snapshot(self):
        collector = Collector(first_snapshot="baseline")
        result = collector.ingest(parse_snapshot(encode(snapshot())))
        self.assertTrue(result.accepted)
        self.assertTrue(result.first_snapshot)
        self.assertEqual(result.deltas, {})

    def test_include_strategy_bills_first_snapshot(self):
        collector = Collector(first_snapshot="include")
        result = collector.ingest(parse_snapshot(encode(snapshot())))
        self.assertTrue(result.accepted)
        total = sum(sum(delta.values()) for delta in result.deltas.values())
        self.assertEqual(total, 100 + 200 + 5 + 7)

    def test_first_snapshot_strategy_must_be_explicit(self):
        with self.assertRaises(ValueError):
            Collector(first_snapshot="auto")

    def test_stale_sequence_is_discarded(self):
        collector = Collector()
        collector.ingest(parse_snapshot(encode(snapshot(sequence=5))))
        result = collector.ingest(parse_snapshot(encode(snapshot(sequence=5))))
        self.assertFalse(result.accepted)
        result = collector.ingest(parse_snapshot(encode(snapshot(sequence=4))))
        self.assertFalse(result.accepted)
        self.assertEqual(collector.stats["rejected_stale_sequence"], 2)

    def test_counter_regression_fails_closed(self):
        collector = Collector()
        collector.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        regressed = snapshot(sequence=2)
        regressed["inbounds"][1]["users"][0]["tcp_uplink_bytes"] = 50
        with self.assertRaises(SettlementFailure):
            collector.ingest(parse_snapshot(encode(regressed)))

    def test_sessions_gauge_may_decrease(self):
        # tcp_sessions / udp_sessions 是瞬时 gauge：正常排空时会降到 0，
        # 不参与差分、不进基线、不触发回退告警。
        collector = Collector()
        collector.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        drained = snapshot(sequence=2)
        drained["inbounds"][1]["tcp_sessions"] = 0
        drained["inbounds"][1]["udp_sessions"] = 0
        result = collector.ingest(parse_snapshot(encode(drained)))
        self.assertTrue(result.accepted)

    def test_unhealthy_snapshot_is_rejected(self):
        collector = Collector()
        for flag in ("counter_overflow", "sequence_overflow", "identity_limit_reached"):
            value = snapshot(sequence=1)
            value["health"][flag] = True
            result = collector.ingest(parse_snapshot(encode(value)))
            self.assertFalse(result.accepted, flag)

    def test_started_at_change_is_unknown_runtime(self):
        collector = Collector()
        collector.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        shifted = snapshot(sequence=2, started_at_unix_ms=1787587200001)
        result = collector.ingest(parse_snapshot(encode(shifted)))
        self.assertFalse(result.accepted)
        self.assertEqual(collector.stats["rejected_unknown_runtime"], 1)

    def test_inactive_lineage_baseline_is_retained(self):
        collector = Collector(first_snapshot="include")
        collector.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        tombstoned = snapshot(sequence=2)
        tombstoned["inbounds"][1]["users"][0]["active"] = False
        result = collector.ingest(parse_snapshot(encode(tombstoned)))
        self.assertTrue(result.accepted)
        # 基线仍在：同名重建后不会把历史累计值当成一次巨额增量重复入账。
        self.assertEqual(result.deltas, {})

    def test_duplicate_batch_is_idempotent(self):
        collector = Collector(first_snapshot="include")
        first = collector.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        self.assertTrue(first.accepted)
        collector.applied_batches.add(first.batch)
        replay = Collector(first_snapshot="include")
        replay.applied_batches.add(first.batch)
        result = replay.ingest(parse_snapshot(encode(snapshot(sequence=1))))
        self.assertFalse(result.accepted)
        self.assertEqual(replay.stats["duplicate_batch"], 1)

    def test_batch_id_covers_sequence_and_delta(self):
        base = parse_snapshot(encode(snapshot(sequence=1)))
        other = parse_snapshot(encode(snapshot(sequence=2)))
        deltas = {("n", "t", 1, "u", 1, "r"): {
            "tcp_uplink_bytes": 1,
            "tcp_downlink_bytes": 0,
            "udp_uplink_bytes": 0,
            "udp_downlink_bytes": 0,
        }}
        changed = copy.deepcopy(deltas)
        changed[("n", "t", 1, "u", 1, "r")]["tcp_uplink_bytes"] = 2
        self.assertNotEqual(batch_id(base, deltas), batch_id(other, deltas))
        self.assertNotEqual(batch_id(base, deltas), batch_id(base, changed))


if __name__ == "__main__":
    unittest.main()
