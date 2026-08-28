#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from mock_collector import (
    CollectorError,
    MockCollector,
    ResponseMetadata,
    canonical_json,
    canonical_request,
    canonical_response,
    parse_lease,
    request_mac,
    sha256_hex,
    strict_json,
)


VECTORS_PATH = Path(__file__).with_name("golden_vectors.json")


def vectors() -> dict:
    return json.loads(VECTORS_PATH.read_text(encoding="utf-8"))


_GOLDEN_VECTORS = vectors()
KEY = bytes.fromhex(_GOLDEN_VECTORS["hmac_key_hex"])
NODE = _GOLDEN_VECTORS["request"]["node_id"]
NONCE = _GOLDEN_VECTORS["request"]["nonce"]


def wrapper_bytes(event_bytes: bytes, sequence: int, *, received_at: int = 1) -> bytes:
    """Build a valid one-line wrapper while allowing raw wrapper variation."""

    return (
        b'{"spool_schema_version":1,"spool_epoch":"'
        + b"c" * 32
        + f'","spool_sequence":"{sequence}","received_at_unix_ms":"{received_at}","event_payload_sha256":"'.encode(
            "ascii"
        )
        + sha256_hex(event_bytes).encode("ascii")
        + b'","event":'
        + event_bytes
        + b"}\n"
    )


def lease_metadata(body: bytes, batch_id: str, first: int, last: int, count: int) -> ResponseMetadata:
    digest = sha256_hex(body)
    return ResponseMetadata(
        200,
        "application/x-ndjson",
        "1",
        digest,
        NODE,
        batch_id,
        "c" * 32,
        str(first),
        str(last),
        str(count),
        digest,
        "",
        NONCE,
    )


class MockCollectorProtocolTest(unittest.TestCase):
    def test_record_golden_vectors_are_canonical(self) -> None:
        data = vectors()
        records = data.get("records")
        self.assertIsInstance(records, dict)
        self.assertEqual(
            set(records),
            {"tcp_access", "udp_access", "producer_gap", "udp_window_contention", "spool_gap", "unicode_access"},
        )
        for name, vector in records.items():
            self.assertIsInstance(vector, dict, name)
            raw = vector["canonical"].encode("utf-8")
            self.assertEqual(sha256_hex(raw), vector["sha256"], name)
            parsed = strict_json(raw)
            self.assertIsInstance(parsed, dict, name)
            # json.dumps preserves insertion order, so this catches any drift
            # in member ordering or escaping in the collector serializer.
            self.assertEqual(canonical_json(parsed), raw, name)

    def test_request_golden_vector(self) -> None:
        data = vectors()
        request_vector = data["request"]
        key = bytes.fromhex(data["hmac_key_hex"])
        body = b'{"schema_version":1}'
        digest = sha256_hex(body)
        canonical = canonical_request(
            request_vector["method"], request_vector["path"], request_vector["node_id"],
            request_vector["timestamp"], request_vector["nonce"], request_vector["body_sha256"],
        )
        self.assertEqual(digest, request_vector["body_sha256"])
        self.assertEqual(canonical, request_vector["canonical"].encode("utf-8"))
        self.assertEqual(hashlib.sha256(canonical).hexdigest(), request_vector["canonical_sha256"])
        self.assertEqual(
            request_mac(key, canonical),
            request_vector["mac"],
        )

    def test_response_golden_vector(self) -> None:
        data = vectors()
        response_vector = data["response"]
        key = bytes.fromhex(data["hmac_key_hex"])
        metadata = ResponseMetadata(
            status=response_vector["status"],
            content_type=response_vector["content_type"],
            schema=response_vector["schema"],
            lease_body_sha256=response_vector["lease_body_sha256"],
            node_id=response_vector["node_id"],
            batch_id=response_vector["batch_id"],
            spool_epoch=response_vector["spool_epoch"],
            first_sequence=response_vector["first_sequence"],
            last_sequence=response_vector["last_sequence"],
            event_count=response_vector["event_count"],
            response_sha256=response_vector["response_sha256"],
            response_mac="",
            request_nonce=response_vector["request_nonce"],
        )
        self.assertEqual(
            hashlib.sha256(canonical_response(metadata)).hexdigest(),
            response_vector["canonical_sha256"],
        )
        self.assertEqual(
            request_mac(key, canonical_response(metadata)),
            response_vector["mac"],
        )

    def test_strict_json_rejects_duplicate_and_nonfinite_values(self) -> None:
        with self.assertRaises(CollectorError):
            strict_json(b'{"schema_version":1,"schema_version":1}')
        with self.assertRaises(CollectorError):
            strict_json(b'{"value":NaN}')
        with self.assertRaises(CollectorError):
            strict_json(b"\xef\xbb\xbf{}")

    def test_lease_parser_checks_wrapper_and_contiguous_sequences(self) -> None:
        event = {
            "schema_version": 1,
            "record_type": "access",
            "event_type": "tcp_target_success",
            "event_id": "0" * 32 + ":1",
        }
        event_bytes = b'{"schema_version":1,"record_type":"access","event_type":"tcp_target_success","event_id":"' + (b"0" * 32) + b':1"}'
        wrapper = (
            b'{"spool_schema_version":1,"spool_epoch":"'
            + b"c" * 32
            + b'","spool_sequence":"1","received_at_unix_ms":"1787587200000","event_payload_sha256":"'
            + sha256_hex(event_bytes).encode("ascii")
            + b'","event":'
            + event_bytes
            + b"}\n"
        )
        metadata = ResponseMetadata(
            status=200,
            content_type="application/x-ndjson",
            schema="1",
            lease_body_sha256=sha256_hex(wrapper),
            node_id=NODE,
            batch_id="b" * 32,
            spool_epoch="c" * 32,
            first_sequence="1",
            last_sequence="1",
            event_count="1",
            response_sha256=sha256_hex(wrapper),
            response_mac="",
            request_nonce=NONCE,
        )
        # The event is already canonical, so strict re-encoding has the same hash.
        self.assertEqual(parse_lease(wrapper, metadata)[0]["event"], event)
        bad = wrapper.replace(b'"spool_sequence":"1"', b'"spool_sequence":"2"')
        with self.assertRaises(CollectorError):
            parse_lease(bad, metadata)

    def test_event_and_batch_conflicts_are_isolated(self) -> None:
        event = {"event_id": "0" * 32 + ":1", "value": 1}
        event_bytes = b'{"event_id":"' + b"0" * 32 + b':1","value":1}'
        wrapper = (
            b'{"spool_schema_version":1,"spool_epoch":"'
            + b"c" * 32
            + b'","spool_sequence":"1","received_at_unix_ms":"1","event_payload_sha256":"'
            + sha256_hex(event_bytes).encode("ascii")
            + b'","event":'
            + event_bytes
            + b"}\n"
        )
        metadata = ResponseMetadata(
            200,
            "application/x-ndjson",
            "1",
            sha256_hex(wrapper),
            NODE,
            "b" * 32,
            "c" * 32,
            "1",
            "1",
            "1",
            sha256_hex(wrapper),
            "",
            NONCE,
        )
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-") as directory:
            state = Path(directory) / "collector.json"
            collector = MockCollector(NODE, KEY, state)
            records = parse_lease(wrapper, metadata)
            collector.accept_records(records, metadata)
            collector.accept_records(records, metadata)  # idempotent replay
            self.assertEqual(len(collector.events), 1)
            conflicting = dict(event)
            conflicting["value"] = 2
            self.assertNotEqual(conflicting, event)
            conflict_bytes = b'{"event_id":"' + b"0" * 32 + b':1","value":2}'
            conflict_wrapper = (
                b'{"spool_schema_version":1,"spool_epoch":"' + b"c" * 32
                + b'","spool_sequence":"2","received_at_unix_ms":"1","event_payload_sha256":"'
                + sha256_hex(conflict_bytes).encode("ascii") + b'","event":' + conflict_bytes + b"}\n"
            )
            conflict_metadata = ResponseMetadata(
                200, "application/x-ndjson", "1", sha256_hex(conflict_wrapper), NODE,
                "d" * 32, "c" * 32, "2", "2", "1", sha256_hex(conflict_wrapper), "", NONCE,
            )
            collector.accept_records(parse_lease(conflict_wrapper, conflict_metadata), conflict_metadata)
            self.assertEqual(collector.events[event["event_id"]][1], event)
            self.assertTrue(any(item["kind"] == "event_payload_conflict" for item in collector.conflicts))
            self.assertTrue(state.exists())
            restored = MockCollector(NODE, KEY, state)
            self.assertEqual(len(restored.events), 1)

            # Reusing the same spool key with a different payload must retain
            # the first wrapper and durably isolate the incoming one.
            sequence_conflict_wrapper = conflict_wrapper.replace(
                b'"spool_sequence":"2"', b'"spool_sequence":"1"'
            )
            sequence_conflict = ResponseMetadata(
                200, "application/x-ndjson", "1", sha256_hex(sequence_conflict_wrapper), NODE,
                "e" * 32, "c" * 32, "1", "1", "1", sha256_hex(sequence_conflict_wrapper), "", NONCE,
            )
            collector.accept_records(parse_lease(sequence_conflict_wrapper, sequence_conflict), sequence_conflict)
            self.assertEqual(collector.events[event["event_id"]][1], event)
            self.assertGreaterEqual(collector.health()["isolated_wrapper_count"], 1)

            # A batch identity conflict is all-or-nothing and cannot smuggle a
            # new event into the accepted maps.
            batch_conflict = ResponseMetadata(
                200, "application/x-ndjson", "1", sha256_hex(conflict_wrapper), NODE,
                "b" * 32, "c" * 32, "2", "2", "1", sha256_hex(conflict_wrapper), "", NONCE,
            )
            collector.accept_records(parse_lease(conflict_wrapper, batch_conflict), batch_conflict)
            self.assertNotIn(conflict_bytes.decode("utf-8"), [
                json.dumps(item[1], separators=(",", ":"))
                for item in collector.events.values()
            ])
            self.assertGreaterEqual(collector.health()["isolated_batch_count"], 1)
            conflict_count = len(collector.conflicts)
            isolated_count = collector.health()["isolated_batch_count"]
            collector.accept_records(parse_lease(conflict_wrapper, batch_conflict), batch_conflict)
            self.assertEqual(len(collector.conflicts), conflict_count)
            self.assertEqual(collector.health()["isolated_batch_count"], isolated_count)

    def test_sequence_conflict_uses_raw_wrapper_hash(self) -> None:
        event_id = "0" * 32 + ":1"
        event_bytes = b'{"event_id":"' + event_id.encode("ascii") + b'","value":1}'
        first = wrapper_bytes(event_bytes, 1, received_at=1)
        second = wrapper_bytes(event_bytes, 1, received_at=2)
        self.assertNotEqual(first, second)
        first_metadata = lease_metadata(first, "1" * 32, 1, 1, 1)
        second_metadata = lease_metadata(second, "2" * 32, 1, 1, 1)
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-sequence-") as directory:
            collector = MockCollector(NODE, KEY, Path(directory) / "collector.json")
            collector.accept_records(parse_lease(first, first_metadata), first_metadata)
            collector.accept_records(parse_lease(second, second_metadata), second_metadata)
            self.assertEqual(collector.events[event_id][1]["value"], 1)
            self.assertTrue(any(item["kind"] == "spool_sequence_conflict" for item in collector.conflicts))
            self.assertEqual(collector.health()["isolated_wrapper_count"], 1)

    def test_same_batch_event_conflict_isolated_and_acked_atomically(self) -> None:
        event_id = "0" * 32 + ":1"
        first_event = json.dumps({"event_id": event_id, "value": 1}, separators=(",", ":")).encode()
        second_event = json.dumps({"event_id": event_id, "value": 2}, separators=(",", ":")).encode()
        body = wrapper_bytes(first_event, 1) + wrapper_bytes(second_event, 2)
        metadata = lease_metadata(body, "3" * 32, 1, 2, 2)
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-batch-conflict-") as directory:
            state = Path(directory) / "collector.json"
            collector = MockCollector(NODE, KEY, state)
            records = parse_lease(body, metadata)
            collector.accept_records(records, metadata)
            conflict_count = len(collector.conflicts)
            self.assertEqual(collector.events[event_id][1]["value"], 1)
            self.assertEqual(collector.health()["isolated_wrapper_count"], 1)
            self.assertEqual(collector.batches[metadata.batch_id][0], metadata.lease_body_sha256)
            # A retry of the now-durable batch is an idempotent replay, not a
            # poison retry that records another conflict.
            collector.accept_records(parse_lease(body, metadata), metadata)
            self.assertEqual(len(collector.conflicts), conflict_count)
            restored = MockCollector(NODE, KEY, state)
            self.assertEqual(restored.events[event_id][1]["value"], 1)
            self.assertEqual(restored.health()["isolated_wrapper_count"], 1)

    def test_durable_failure_rolls_back_in_memory_state(self) -> None:
        event = {"event_id": "0" * 32 + ":1", "value": 1}
        event_bytes = json.dumps(event, separators=(",", ":")).encode("utf-8")
        wrapper = {
            "spool_schema_version": 1,
            "spool_epoch": "c" * 32,
            "spool_sequence": "1",
            "received_at_unix_ms": "1",
            "event_payload_sha256": sha256_hex(event_bytes),
            "event": event,
        }
        metadata = ResponseMetadata(
            200, "application/x-ndjson", "1", "a" * 64, NODE,
            "f" * 32, "c" * 32, "1", "1", "1", "a" * 64, "", NONCE,
        )
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-rollback-") as directory:
            state = Path(directory) / "collector.json"
            collector = MockCollector(NODE, KEY, state)
            state.mkdir()
            with self.assertRaises(OSError):
                collector.accept_records([wrapper], metadata)
            self.assertEqual(collector.events, {})
            self.assertEqual(collector.batches, {})

    def test_health_exposes_diagnostics_and_gap_alerts(self) -> None:
        collector = MockCollector(NODE, KEY)
        self.assertEqual(collector.health()["status"], "ok")
        collector.record_diagnostic("udp_window_contention", skipped=3)
        collector.record_gap("spool_gap", first_sequence="4", last_sequence="5")
        collector.record_gap("producer_gap", count=2)
        collector.record_gap("batch_evicted", batch_id="e" * 32)
        health = collector.health()
        self.assertEqual(health["status"], "degraded")
        self.assertEqual(health["diagnostic_count"], 1)
        self.assertEqual(health["spool_gap_count"], 1)
        self.assertEqual(health["producer_gap_count"], 1)
        self.assertEqual(health["batch_evicted_count"], 1)
        self.assertEqual(
            health["alerts"],
            ["batch_evicted", "producer_gap", "spool_gap", "udp_window_contention"],
        )


if __name__ == "__main__":
    unittest.main()
