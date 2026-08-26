#!/usr/bin/env python3

from __future__ import annotations

import copy
import unittest

from settlement_model import COUNTER_FIELDS, SettlementModel, SnapshotError


def snapshot(runtime: str = "runtime-a", sequence: int = 1) -> dict:
    return {
        "schema_version": 1,
        "node_id": "node-test-01",
        "runtime_id": runtime,
        "started_at_unix_ms": 1_700_000_000_000,
        "sequence": sequence,
        "health": {"counter_overflow": False, "sequence_overflow": False},
        "servers": [
            {
                "server_id": "ss-entry-01",
                "listen": "127.0.0.1:8388",
                "generation": 1,
                "active": True,
                "users": [
                    {
                        "identity_kind": "user",
                        "name": "u_000123",
                        "generation": 1,
                        "active": True,
                        "tcp_uplink_bytes": 10,
                        "tcp_downlink_bytes": 20,
                        "udp_uplink_bytes": 30,
                        "udp_downlink_bytes": 40,
                    }
                ],
            }
        ],
    }


class SettlementModelTest(unittest.TestCase):
    def test_baseline_then_delta_and_duplicate(self) -> None:
        model = SettlementModel("baseline")
        first = snapshot()
        self.assertEqual(model.ingest(first), [])

        second = snapshot(sequence=2)
        user = second["servers"][0]["users"][0]
        for field in COUNTER_FIELDS:
            user[field] += 7
        batches = model.ingest(second)
        self.assertEqual(len(batches), 1)
        self.assertEqual(set(batches[0].counters.values()), {7})
        self.assertEqual(model.ingest(second), [])

    def test_include_first_strategy(self) -> None:
        batch = SettlementModel("include").ingest(snapshot())[0]
        self.assertEqual(batch.counters["tcp_uplink_bytes"], 10)
        self.assertEqual(batch.counters["udp_downlink_bytes"], 40)

    def test_runtime_change_starts_new_cycle(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())
        self.assertEqual(model.ingest(snapshot(runtime="runtime-b")), [])

        next_snapshot = snapshot(runtime="runtime-b", sequence=2)
        next_snapshot["servers"][0]["users"][0]["tcp_uplink_bytes"] += 3
        self.assertEqual(model.ingest(next_snapshot)[0].counters["tcp_uplink_bytes"], 3)

    def test_distinct_server_generation_uses_a_distinct_baseline(self) -> None:
        model = SettlementModel("baseline")
        original = snapshot()
        model.ingest(original)

        multi_generation = snapshot(sequence=2)
        old_server = copy.deepcopy(original["servers"][0])
        old_server["active"] = False
        old_server["users"][0]["active"] = False
        multi_generation["servers"].insert(0, old_server)
        multi_generation["servers"][1]["generation"] = 2
        multi_generation["servers"][1]["users"][0]["tcp_uplink_bytes"] = 1
        self.assertEqual(model.ingest(multi_generation), [])

        advanced = copy.deepcopy(multi_generation)
        advanced["sequence"] = 3
        advanced["servers"][1]["users"][0]["tcp_uplink_bytes"] = 6
        batch = model.ingest(advanced)[0]
        self.assertEqual(batch.server_generation, 2)
        self.assertEqual(batch.counters["tcp_uplink_bytes"], 5)

    def test_stale_sequence_is_ignored_atomically(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot(sequence=2))
        stale = snapshot(sequence=1)
        stale["servers"][0]["users"][0]["tcp_uplink_bytes"] = 999
        self.assertEqual(model.ingest(stale), [])

        current = snapshot(sequence=3)
        current["servers"][0]["users"][0]["tcp_uplink_bytes"] = 11
        self.assertEqual(model.ingest(current)[0].counters["tcp_uplink_bytes"], 1)

    def test_counter_regression_rejects_entire_snapshot(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())
        invalid = snapshot(sequence=2)
        invalid["servers"][0]["users"][0]["udp_uplink_bytes"] = 29
        with self.assertRaises(SnapshotError):
            model.ingest(invalid)

        valid = snapshot(sequence=2)
        valid["servers"][0]["users"][0]["tcp_uplink_bytes"] = 12
        self.assertEqual(model.ingest(valid)[0].counters["tcp_uplink_bytes"], 2)

    def test_multi_lineage_regression_rejects_without_partial_commit(self) -> None:
        model = SettlementModel("baseline")
        first = snapshot()
        second_server = copy.deepcopy(first["servers"][0])
        second_server["server_id"] = "ss-entry-02"
        second_server["users"][0]["name"] = "u_000456"
        first["servers"].append(second_server)
        model.ingest(first)

        invalid = copy.deepcopy(first)
        invalid["sequence"] = 2
        invalid["servers"][0]["users"][0]["tcp_uplink_bytes"] += 5
        invalid["servers"][1]["users"][0]["udp_downlink_bytes"] -= 1
        with self.assertRaises(SnapshotError):
            model.ingest(invalid)

        valid = copy.deepcopy(first)
        valid["sequence"] = 2
        valid["servers"][0]["users"][0]["tcp_uplink_bytes"] += 5
        batches = model.ingest(valid)
        self.assertEqual(len(batches), 1)
        self.assertEqual(batches[0].server_id, "ss-entry-01")
        self.assertEqual(batches[0].counters["tcp_uplink_bytes"], 5)

    def test_disappearing_server_or_identity_rejects_without_state_change(self) -> None:
        model = SettlementModel("baseline")
        first = snapshot()
        second_server = copy.deepcopy(first["servers"][0])
        second_server["server_id"] = "ss-entry-02"
        second_server["users"][0]["name"] = "u_000456"
        first["servers"].append(second_server)
        model.ingest(first)

        missing_identity = copy.deepcopy(first)
        missing_identity["sequence"] = 2
        missing_identity["servers"][0]["users"] = []
        with self.assertRaisesRegex(SnapshotError, "identity lineage disappeared"):
            model.ingest(missing_identity)

        missing_server = copy.deepcopy(first)
        missing_server["sequence"] = 2
        missing_server["servers"].pop()
        with self.assertRaisesRegex(SnapshotError, "server lineage disappeared"):
            model.ingest(missing_server)

        valid = copy.deepcopy(first)
        valid["sequence"] = 2
        valid["servers"][1]["users"][0]["tcp_downlink_bytes"] += 9
        batches = model.ingest(valid)
        self.assertEqual(len(batches), 1)
        self.assertEqual(batches[0].identity_name, "u_000456")
        self.assertEqual(batches[0].counters["tcp_downlink_bytes"], 9)

    def test_started_at_must_remain_fixed_within_runtime(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())

        changed = snapshot(sequence=2)
        changed["started_at_unix_ms"] += 1
        changed["servers"][0]["users"][0]["tcp_uplink_bytes"] += 4
        with self.assertRaisesRegex(SnapshotError, "started_at_unix_ms changed"):
            model.ingest(changed)

        valid = snapshot(sequence=2)
        valid["servers"][0]["users"][0]["tcp_uplink_bytes"] += 4
        self.assertEqual(model.ingest(valid)[0].counters["tcp_uplink_bytes"], 4)

    def test_unknown_identity_kind_rejects_entire_snapshot(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())

        invalid = snapshot(sequence=2)
        invalid["servers"][0]["users"][0]["identity_kind"] = "admin"
        with self.assertRaisesRegex(SnapshotError, "unsupported identity_kind"):
            model.ingest(invalid)

        valid = snapshot(sequence=2)
        valid["servers"][0]["users"][0]["udp_uplink_bytes"] += 6
        self.assertEqual(model.ingest(valid)[0].counters["udp_uplink_bytes"], 6)

    def test_unhealthy_snapshot_is_rejected(self) -> None:
        invalid = copy.deepcopy(snapshot())
        invalid["health"]["counter_overflow"] = True
        with self.assertRaises(SnapshotError):
            SettlementModel().ingest(invalid)

        invalid_sequence = copy.deepcopy(snapshot())
        invalid_sequence["health"]["sequence_overflow"] = True
        with self.assertRaises(SnapshotError):
            SettlementModel().ingest(invalid_sequence)

    def test_unhealthy_higher_sequence_does_not_advance_runtime(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())

        unhealthy = snapshot(sequence=2)
        unhealthy["health"]["counter_overflow"] = True
        unhealthy["servers"][0]["users"][0]["tcp_uplink_bytes"] += 8
        with self.assertRaises(SnapshotError):
            model.ingest(unhealthy)

        healthy = snapshot(sequence=2)
        healthy["servers"][0]["users"][0]["tcp_uplink_bytes"] += 8
        self.assertEqual(model.ingest(healthy)[0].counters["tcp_uplink_bytes"], 8)

    def test_duplicate_generation_is_rejected_atomically(self) -> None:
        model = SettlementModel("baseline")
        model.ingest(snapshot())
        duplicate = snapshot(sequence=2)
        duplicate["servers"].append(copy.deepcopy(duplicate["servers"][0]))
        with self.assertRaises(SnapshotError):
            model.ingest(duplicate)

        valid = snapshot(sequence=2)
        valid["servers"][0]["users"][0]["tcp_uplink_bytes"] += 2
        self.assertEqual(model.ingest(valid)[0].counters["tcp_uplink_bytes"], 2)

    def test_envelope_rejects_boolean_integer_fields(self) -> None:
        boolean_version = snapshot()
        boolean_version["schema_version"] = True
        with self.assertRaises(SnapshotError):
            SettlementModel().ingest(boolean_version)

        boolean_start = snapshot()
        boolean_start["started_at_unix_ms"] = True
        with self.assertRaises(SnapshotError):
            SettlementModel().ingest(boolean_start)

        boolean_sequence = snapshot()
        boolean_sequence["sequence"] = True
        with self.assertRaises(SnapshotError):
            SettlementModel().ingest(boolean_sequence)

    def test_batch_id_is_deterministic(self) -> None:
        first = SettlementModel("include").ingest(snapshot())[0]
        second = SettlementModel("include").ingest(snapshot())[0]
        self.assertEqual(first.batch_id, second.batch_id)


if __name__ == "__main__":
    unittest.main(verbosity=2)
