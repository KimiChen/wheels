#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import json
import socket
import tempfile
import threading
import unittest
from pathlib import Path

from mock_collector import (
    CollectorError,
    MockCollector,
    ResponseMetadata,
    U64_MAX,
    _canonical_event,
    _strict_decimal,
    canonical_json,
    canonical_request,
    canonical_response,
    _parse_http_response,
    parse_lease,
    request_mac,
    sha256_hex,
    strict_json,
)


VECTORS_PATH = Path(__file__).with_name("golden_vectors.json")
REMOVE = object()


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


def golden_event(name: str) -> dict:
    """Return one golden record parsed into a dict with its canonical order."""

    return strict_json(_GOLDEN_VECTORS["records"][name]["canonical"])


def access_event(sequence: int, *, port: int = 443) -> bytes:
    """Return canonical `tcp_target_success` bytes for a synthetic event.

    `parse_lease` applies the full §6 variant rules, so unit fixtures have to be
    real wire events.  `port` gives two events the same `event_id` with
    different payload bytes, which is what the idempotency cases need.
    """

    event = golden_event("tcp_access")
    event["event_id"] = f"{event['runtime_id']}:{sequence}"
    event["audit_sequence"] = str(sequence)
    event["target"]["port"] = port
    return canonical_json(event)


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
            {
                "tcp_access",
                "tcp_access_null_normalized",
                "udp_access",
                "producer_gap",
                "producer_gap_encode_error",
                "producer_gap_permanent_nack",
                "udp_window_contention",
                "spool_gap",
                "spool_gap_min_free",
                "spool_gap_quarantine",
                "spool_gap_tail_truncation",
                "spool_gap_segment_corruption",
                "unicode_access",
                "escaping_access",
                "nullable_spool_gap",
            },
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
        zero = canonical_request("GET", "/v1/audit/healthz", NODE, "0", NONCE, sha256_hex(b""))
        self.assertIn(b"\n0\n", zero)

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

    def test_empty_response_golden_vector(self) -> None:
        data = vectors()
        response_vector = data["empty_response"]
        metadata = ResponseMetadata(
            status=response_vector["status"],
            content_type="",
            schema="",
            lease_body_sha256="",
            node_id=response_vector["node_id"],
            batch_id="",
            spool_epoch="",
            first_sequence="",
            last_sequence="",
            event_count="",
            response_sha256=response_vector["response_sha256"],
            response_mac="",
            request_nonce=response_vector["request_nonce"],
        )
        canonical = canonical_response(metadata)
        self.assertEqual(canonical, response_vector["canonical"].encode("utf-8"))
        self.assertEqual(sha256_hex(canonical), response_vector["canonical_sha256"])
        self.assertEqual(request_mac(KEY, canonical), response_vector["mac"])

    def test_ndjson_golden_vectors_are_exact_and_parse_together(self) -> None:
        data = vectors()
        wrappers = data["ndjson_records"]
        self.assertEqual(set(wrappers), {"escaping_access", "nullable_spool_gap"})
        body = b""
        for name, vector in wrappers.items():
            raw = vector["canonical"].encode("utf-8")
            self.assertTrue(raw.endswith(b"\n"), name)
            self.assertEqual(sha256_hex(raw), vector["sha256"], name)
            self.assertEqual(canonical_json(strict_json(raw[:-1])), raw[:-1], name)
            body += raw
        parsed = parse_lease(body, lease_metadata(body, "e" * 32, 1, 2, 2))
        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["event"]["server_id"], 'server"quoted\\path')
        self.assertIsNone(parsed[1]["event"]["lost_spool_epoch"])

    def test_unencodable_text_stays_on_the_collector_error_path(self) -> None:
        # A lone surrogate is valid JSON input but has no UTF-8 encoding.  The
        # CLI only catches CollectorError/OSError, so a raw UnicodeEncodeError
        # would bypass the "never echo a body" error path.
        lone_surrogate = strict_json(b'{"value":"\\ud800"}')
        with self.assertRaisesRegex(CollectorError, "cannot be encoded"):
            canonical_json(lone_surrogate)
        body = (
            b'{"spool_schema_version":1,"spool_epoch":"\\ud800",'
            b'"spool_sequence":"1","received_at_unix_ms":"1","event_payload_sha256":"'
            + b"a" * 64
            + b'","event":{}}\n'
        )
        with self.assertRaisesRegex(CollectorError, "cannot be encoded"):
            parse_lease(body, lease_metadata(body, "b" * 32, 1, 1, 1))

    def test_strict_json_rejects_duplicate_and_nonfinite_values(self) -> None:
        with self.assertRaises(CollectorError):
            strict_json(b'{"schema_version":1,"schema_version":1}')
        with self.assertRaises(CollectorError):
            strict_json(b'{"value":NaN}')
        with self.assertRaises(CollectorError):
            strict_json(b"\xef\xbb\xbf{}")

    def test_lease_parser_checks_wrapper_and_contiguous_sequences(self) -> None:
        event_bytes = vectors()["records"]["tcp_access"]["canonical"].encode("utf-8")
        event = strict_json(event_bytes)
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

    def test_lease_parser_matches_decimal_u64_bounds_for_received_at(self) -> None:
        event_bytes = vectors()["records"]["tcp_access"]["canonical"].encode("utf-8")
        zero_timestamp = wrapper_bytes(event_bytes, 1, received_at=0)
        self.assertEqual(
            len(parse_lease(zero_timestamp, lease_metadata(zero_timestamp, "0" * 32, 1, 1, 1))),
            1,
        )

        maximum_timestamp = wrapper_bytes(event_bytes, 1, received_at=U64_MAX)
        self.assertEqual(
            len(parse_lease(maximum_timestamp, lease_metadata(maximum_timestamp, "f" * 32, 1, 1, 1))),
            1,
        )

        overflowing = wrapper_bytes(event_bytes, 1, received_at=U64_MAX + 1)
        with self.assertRaisesRegex(CollectorError, "u64 range"):
            parse_lease(overflowing, lease_metadata(overflowing, "1" * 32, 1, 1, 1))

        self.assertEqual(_strict_decimal("0"), "0")
        self.assertEqual(_strict_decimal(str(U64_MAX)), str(U64_MAX))
        self.assertEqual(_strict_decimal(str(U64_MAX), positive=True), str(U64_MAX))
        for invalid in (str(U64_MAX + 1), "-1", "+1", "01", "1.0", " 1", 1, True, None):
            with self.subTest(invalid=invalid):
                with self.assertRaises(CollectorError):
                    _strict_decimal(invalid)
        with self.assertRaises(CollectorError):
            _strict_decimal("0", positive=True)

    def test_lease_parser_rejects_noncanonical_wrapper_and_event_escaping(self) -> None:
        event = access_event(1)
        canonical = wrapper_bytes(event, 1)
        metadata = lease_metadata(canonical, "b" * 32, 1, 1, 1)
        self.assertEqual(len(parse_lease(canonical, metadata)), 1)

        parsed_wrapper = strict_json(canonical[:-1])
        reordered = canonical_json(
            {
                "spool_epoch": parsed_wrapper["spool_epoch"],
                "spool_schema_version": parsed_wrapper["spool_schema_version"],
                "spool_sequence": parsed_wrapper["spool_sequence"],
                "received_at_unix_ms": parsed_wrapper["received_at_unix_ms"],
                "event_payload_sha256": parsed_wrapper["event_payload_sha256"],
                "event": parsed_wrapper["event"],
            }
        ) + b"\n"
        with self.assertRaisesRegex(CollectorError, "canonical"):
            parse_lease(reordered, lease_metadata(reordered, "c" * 32, 1, 1, 1))

        escaped_event = event.replace(b'"identity_kind":"user"', b'"identity_kind":"use\\u0072"')
        escaped = wrapper_bytes(escaped_event, 1)
        with self.assertRaisesRegex(CollectorError, "canonical"):
            parse_lease(escaped, lease_metadata(escaped, "d" * 32, 1, 1, 1))

        access = strict_json(vectors()["records"]["tcp_access"]["canonical"])
        reordered_access = canonical_json(
            {"record_type": access["record_type"], **{key: value for key, value in access.items() if key != "record_type"}}
        )
        reordered_event_wrapper = wrapper_bytes(reordered_access, 1)
        with self.assertRaisesRegex(CollectorError, "embedded event.*canonical"):
            parse_lease(
                reordered_event_wrapper,
                lease_metadata(reordered_event_wrapper, "f" * 32, 1, 1, 1),
            )

    def test_event_and_batch_conflicts_are_isolated(self) -> None:
        event_bytes = access_event(1)
        event = strict_json(event_bytes)
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
            conflict_bytes = access_event(1, port=444)
            conflicting = strict_json(conflict_bytes)
            self.assertNotEqual(conflicting, event)
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
        event_bytes = access_event(1)
        event_id = strict_json(event_bytes)["event_id"]
        first = wrapper_bytes(event_bytes, 1, received_at=1)
        second = wrapper_bytes(event_bytes, 1, received_at=2)
        self.assertNotEqual(first, second)
        first_metadata = lease_metadata(first, "1" * 32, 1, 1, 1)
        second_metadata = lease_metadata(second, "2" * 32, 1, 1, 1)
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-sequence-") as directory:
            collector = MockCollector(NODE, KEY, Path(directory) / "collector.json")
            collector.accept_records(parse_lease(first, first_metadata), first_metadata)
            collector.accept_records(parse_lease(second, second_metadata), second_metadata)
            self.assertEqual(collector.events[event_id][1]["target"]["port"], 443)
            self.assertTrue(any(item["kind"] == "spool_sequence_conflict" for item in collector.conflicts))
            self.assertEqual(collector.health()["isolated_wrapper_count"], 1)

    def test_same_batch_event_conflict_isolated_and_acked_atomically(self) -> None:
        first_event = access_event(1)
        second_event = access_event(1, port=444)
        event_id = strict_json(first_event)["event_id"]
        body = wrapper_bytes(first_event, 1) + wrapper_bytes(second_event, 2)
        metadata = lease_metadata(body, "3" * 32, 1, 2, 2)
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-batch-conflict-") as directory:
            state = Path(directory) / "collector.json"
            collector = MockCollector(NODE, KEY, state)
            records = parse_lease(body, metadata)
            collector.accept_records(records, metadata)
            conflict_count = len(collector.conflicts)
            self.assertEqual(collector.events[event_id][1]["target"]["port"], 443)
            self.assertEqual(collector.health()["isolated_wrapper_count"], 1)
            self.assertEqual(collector.batches[metadata.batch_id][0], metadata.lease_body_sha256)
            # A retry of the now-durable batch is an idempotent replay, not a
            # poison retry that records another conflict.
            collector.accept_records(parse_lease(body, metadata), metadata)
            self.assertEqual(len(collector.conflicts), conflict_count)
            restored = MockCollector(NODE, KEY, state)
            self.assertEqual(restored.events[event_id][1]["target"]["port"], 443)
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

    def test_wire_collection_requires_durable_state(self) -> None:
        collector = MockCollector(NODE, KEY)
        with self.assertRaisesRegex(CollectorError, "durable state"):
            collector.collect_once(Path("/does/not/matter.sock"))

    def test_state_requires_all_base_members(self) -> None:
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-state-") as directory:
            state = Path(directory) / "collector.json"
            state.write_text('{"schema_version":1,"node_id":"' + NODE + '"}', encoding="utf-8")
            with self.assertRaisesRegex(CollectorError, "missing or unexpected"):
                MockCollector(NODE, KEY, state)

    def test_http_content_length_is_case_insensitive(self) -> None:
        status, headers, body = _parse_http_response(
            b"HTTP/1.1 200 OK\r\ncontent-length: 2\r\n\r\n{}"
        )
        self.assertEqual((status, body), (200, b"{}"))
        self.assertEqual(headers, {"content-length": "2"})

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
        with self.assertRaises(CollectorError):
            collector.record_diagnostic("invalid\nkind")


class EventValidationTest(unittest.TestCase):
    """`parse_lease` must apply the §6 variant rules to every embedded event.

    Every case below is a single-field mutation of a golden record, so dropping
    any one rule from the collector leaves exactly that case green.
    """

    def parse_one(self, event: dict) -> list:
        raw = canonical_json(event)
        body = wrapper_bytes(raw, 1)
        return parse_lease(body, lease_metadata(body, "b" * 32, 1, 1, 1))

    def assert_rejected(self, event: dict) -> None:
        with self.assertRaises(CollectorError):
            self.parse_one(event)

    def mutate(self, name: str, path: tuple[str, ...], value: object) -> dict:
        event = golden_event(name)
        holder = event
        for key in path[:-1]:
            holder = holder[key]
        if value is REMOVE:
            del holder[path[-1]]
        else:
            holder[path[-1]] = value
        return event

    def assert_each_mutation_rejected(self, name: str, cases: tuple) -> None:
        for path, value in cases:
            with self.subTest(record=name, field=".".join(path), value=repr(value)):
                self.assert_rejected(self.mutate(name, path, value))

    def test_every_golden_record_is_accepted_by_the_lease_parser(self) -> None:
        for name in _GOLDEN_VECTORS["records"]:
            with self.subTest(record=name):
                self.assertEqual(len(self.parse_one(golden_event(name))), 1)

    def test_every_golden_record_parses_in_one_ndjson_lease(self) -> None:
        names = list(_GOLDEN_VECTORS["records"])
        body = b"".join(
            wrapper_bytes(canonical_json(golden_event(name)), index + 1) for index, name in enumerate(names)
        )
        parsed = parse_lease(body, lease_metadata(body, "a" * 32, 1, len(names), len(names)))
        self.assertEqual(
            [record["event"]["event_type"] for record in parsed],
            [golden_event(name)["event_type"] for name in names],
        )

    def test_canonical_member_order_is_bound_for_every_variant(self) -> None:
        # Once the field set matches, the per-variant order table is the only
        # thing pinning member order, so every variant needs a transposition
        # case that turns red when its row drifts.
        for name in _GOLDEN_VECTORS["records"]:
            event = golden_event(name)
            self.assertEqual(len(self.parse_one(event)), 1, name)
            transpositions: list[tuple[dict, str]] = []
            keys = list(event)
            for index in range(len(keys) - 1):
                order = keys[:]
                order[index], order[index + 1] = order[index + 1], order[index]
                transpositions.append(({key: event[key] for key in order}, keys[index]))
            target = event.get("target")
            if isinstance(target, dict):
                target_keys = list(target)
                for index in range(len(target_keys) - 1):
                    order = target_keys[:]
                    order[index], order[index + 1] = order[index + 1], order[index]
                    moved = dict(event)
                    moved["target"] = {key: target[key] for key in order}
                    transpositions.append((moved, f"target.{target_keys[index]}"))
            for mutant, moved in transpositions:
                with self.subTest(record=name, moved=moved):
                    with self.assertRaisesRegex(CollectorError, "embedded event is not canonical"):
                        self.parse_one(mutant)

    def test_unknown_or_missing_event_type_is_rejected(self) -> None:
        for value in ("tcp_target_failure", "", "TCP_TARGET_SUCCESS", 1, None, REMOVE):
            with self.subTest(event_type=repr(value)):
                self.assert_rejected(self.mutate("tcp_access", ("event_type",), value))

    def test_access_common_field_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "tcp_access",
            (
                (("schema_version",), 2),
                (("schema_version",), "1"),
                (("schema_version",), True),
                (("schema_version",), 1.0),
                (("record_type",), "diagnostic"),
                (("record_type",), None),
                (("event_id",), "0123456789abcdef0123456789abcdef:43"),
                (("event_id",), "0123456789abcdef0123456789abcdef"),
                (("event_id",), 42),
                (("audit_sequence",), "0"),
                (("audit_sequence",), "042"),
                (("audit_sequence",), "+42"),
                (("audit_sequence",), 42),
                (("audit_sequence",), str(U64_MAX + 1)),
                (("occurred_at_unix_ms",), 1787587200000),
                (("occurred_at_unix_ms",), "01"),
                (("runtime_monotonic_ms",), " 1"),
                (("runtime_monotonic_ms",), None),
                (("node_id",), ""),
                (("node_id",), "node example"),
                (("node_id",), "x" * 129),
                (("node_id",), "nodé"),
                (("runtime_id",), "0123456789ABCDEF0123456789abcdef"),
                (("runtime_id",), "0123456789abcdef"),
                (("server_id",), ""),
                (("server_generation",), 2),
                (("server_generation",), "1"),
                (("identity_kind",), "admin"),
                (("identity_name",), ""),
                (("identity_generation",), 0),
                (("transport",), "udp"),
                (("transport",), None),
                (("success_evidence",), "udp_send_ok"),
            ),
        )

    def test_access_target_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "tcp_access",
            (
                (("target",), None),
                (("target", "kind"), "hostname"),
                (("target", "host"), ""),
                (("target", "host"), "x" * 256),
                (("target", "host"), 443),
                (("target", "normalized_host"), None),
                (("target", "normalized_host"), "Example.com"),
                (("target", "normalized_host"), "example.com."),
                (("target", "normalized_host"), 1),
                (("target", "port"), 0),
                (("target", "port"), 65536),
                (("target", "port"), "443"),
                (("target", "port"), True),
                (("target", "remote_ip"), "192.000.2.10"),
                (("target", "remote_ip"), "999.0.2.10"),
                (("target", "remote_ip"), "192.0.2.10 "),
                (("target", "remote_ip"), "example.com"),
                (("target", "remote_ip"), "fe80::1%eth0"),
                (("target", "remote_ip"), None),
                (("target", "extra"), 1),
            ),
        )
        # A domain that cannot be normalized must carry a null normalized_host.
        self.assert_rejected(
            self.mutate("tcp_access_null_normalized", ("target", "normalized_host"), "invalid.example")
        )
        # An IP target repeats the same canonical text in both members.
        self.assert_each_mutation_rejected(
            "udp_access",
            (
                (("target", "host"), "192.0.2.53."),
                (("target", "normalized_host"), "192.0.2.54"),
                (("target", "normalized_host"), None),
            ),
        )

    def test_udp_access_association_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "udp_access",
            (
                (("association_id",), REMOVE),
                (("association_id",), None),
                (("association_id",), "abcdefabcdefabcdefabcdefabcdefAB"),
                (("association_id",), "abcdef"),
                (("transport",), "tcp"),
                (("success_evidence",), "tcp_bidirectional_payload"),
            ),
        )
        # A TCP event must not carry an association at all.
        tcp = golden_event("tcp_access")
        tcp["association_id"] = "abcdefabcdefabcdefabcdefabcdefab"
        self.assert_rejected(tcp)

    def test_producer_gap_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "producer_gap",
            (
                (("record_type",), "access"),
                (("event_type",), "spool_gap"),
                (("event_id",), "0123456789abcdef0123456789abcdef:98"),
                (("audit_sequence",), "0"),
                (("dropped_events",), "0"),
                (("dropped_events",), None),
                (("runtime_id",), "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"),
                (("node_id",), ""),
                (("reason",), "unknown_reason"),
                (("reason",), None),
                (("permanent_nack_code",), "invalid_schema"),
                (("first_dropped_sequence",), None),
                (("first_dropped_sequence",), "99"),
                (("first_dropped_sequence",), "0"),
                (("last_dropped_sequence",), None),
                (("first_seen_unix_ms",), "1787587209001"),
                (("last_seen_unix_ms",), "1787587210001"),
            ),
        )
        self.assert_each_mutation_rejected(
            "producer_gap_permanent_nack",
            (
                (("permanent_nack_code",), None),
                (("permanent_nack_code",), "not_a_code"),
                (("reason",), "queue_overflow"),
            ),
        )

    def test_udp_window_contention_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "udp_window_contention",
            (
                (("record_type",), "access"),
                (("event_id",), "0123456789abcdef0123456789abcdef:99"),
                (("audit_sequence",), "0"),
                (("skipped_successful_datagrams",), "0"),
                (("skipped_successful_datagrams",), 17),
                (("first_seen_unix_ms",), "1787587210001"),
                (("last_seen_unix_ms",), "1787587211001"),
                (("node_id",), "node id"),
            ),
        )

    def test_spool_gap_rules(self) -> None:
        self.assert_each_mutation_rejected(
            "spool_gap",
            (
                (("record_type",), "access"),
                (("event_id",), "gap:fedcba9876543210fedcba9876543210"),
                (("event_id",), "spool:fedcba98765432"),
                (("event_id",), None),
                (("node_id",), ""),
                (("spool_epoch",), "89ABCDEF0123456789abcdef01234567"),
                (("lost_spool_epoch",), "zz"),
                (("lost_batch_id",), "fedcba98"),
                (("reason",), "unknown_reason"),
                (("first_lost_spool_sequence",), None),
                (("first_lost_spool_sequence",), "1001"),
                (("first_lost_spool_sequence",), "0"),
                (("last_lost_spool_sequence",), None),
                (("lost_events",), "0"),
                (("lost_bytes",), "0"),
                (("lost_events",), 1000),
                (("occurred_at_unix_ms",), "-1"),
            ),
        )
        # The nullable variant keeps every optional member null together.
        self.assert_each_mutation_rejected(
            "nullable_spool_gap",
            (
                (("first_lost_spool_sequence",), "1"),
                (("lost_events",), "0"),
            ),
        )


def signed_response_headers(
    status: int,
    body: bytes,
    nonce: str,
    *,
    node: str = NODE,
    headers: dict[str, str] | None = None,
) -> dict[str, str]:
    """Return response headers whose MAC covers exactly what is being sent."""

    headers = dict(headers or {})
    digest = sha256_hex(body)
    metadata = ResponseMetadata(
        status=status,
        content_type=headers.get("Content-Type", ""),
        schema=headers.get("X-Shadowsocks-Audit-Schema", ""),
        lease_body_sha256=headers.get("X-Shadowsocks-Audit-Body-SHA256", ""),
        node_id=node,
        batch_id=headers.get("X-Shadowsocks-Audit-Batch-Id", ""),
        spool_epoch=headers.get("X-Shadowsocks-Audit-Spool-Epoch", ""),
        first_sequence=headers.get("X-Shadowsocks-Audit-First-Sequence", ""),
        last_sequence=headers.get("X-Shadowsocks-Audit-Last-Sequence", ""),
        event_count=headers.get("X-Shadowsocks-Audit-Event-Count", ""),
        response_sha256=digest,
        response_mac="",
        request_nonce=nonce,
    )
    headers["X-Shadowsocks-Audit-Node"] = node
    headers["X-Shadowsocks-Audit-Response-SHA256"] = digest
    headers["X-Shadowsocks-Audit-Response-MAC"] = request_mac(KEY, canonical_response(metadata))
    return headers


class _CannedAuditd(threading.Thread):
    """Serve correctly signed canned export responses over a UDS.

    The MAC is computed over exactly the headers each canned response sends, so
    the collector reaches its own protocol checks instead of failing on the MAC.
    """

    def __init__(self, path: Path, responses: list[tuple[int, dict[str, str], bytes]]) -> None:
        super().__init__(daemon=True)
        self.responses = list(responses)
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(str(path))
        self.listener.listen(len(self.responses) + 1)

    def run(self) -> None:
        for status, headers, body in self.responses:
            connection, _ = self.listener.accept()
            with connection:
                request = b""
                while b"\r\n\r\n" not in request:
                    chunk = connection.recv(4096)
                    if not chunk:
                        break
                    request += chunk
                connection.sendall(self._signed(request, status, headers, body))
        self.listener.close()

    @staticmethod
    def _signed(request: bytes, status: int, headers: dict[str, str], body: bytes) -> bytes:
        head = request.split(b"\r\n\r\n", 1)[0].decode("ascii")
        nonce = ""
        for line in head.split("\r\n")[1:]:
            name, _, value = line.partition(": ")
            if name.lower() == "x-shadowsocks-audit-nonce":
                nonce = value
        signed = signed_response_headers(status, body, nonce, headers=headers)
        lines = [f"HTTP/1.1 {status} STATUS", "Connection: close", "Cache-Control: no-store"]
        lines.extend(f"{name}: {value}" for name, value in signed.items())
        if status != 204:
            lines.append(f"Content-Length: {len(body)}")
        return ("\r\n".join(lines) + "\r\n\r\n").encode("ascii") + body


class WireResponseTest(unittest.TestCase):
    """§10.1 response rules that only the wire-facing path can exercise."""

    def collect(self, responses: list[tuple[int, dict[str, str], bytes]]) -> int:
        with tempfile.TemporaryDirectory(prefix="ssrp-collector-wire-") as directory:
            socket_path = Path(directory) / "export.sock"
            server = _CannedAuditd(socket_path, responses)
            server.start()
            try:
                collector = MockCollector(NODE, KEY, Path(directory) / "state" / "collector.json")
                return collector.collect_once(socket_path)
            finally:
                server.join(timeout=5)

    def lease_response(self) -> tuple[bytes, tuple[int, dict[str, str], bytes]]:
        body = wrapper_bytes(access_event(1), 1)
        headers = {
            "Content-Type": "application/x-ndjson",
            "X-Shadowsocks-Audit-Schema": "1",
            "X-Shadowsocks-Audit-Body-SHA256": sha256_hex(body),
            "X-Shadowsocks-Audit-Batch-Id": "b" * 32,
            "X-Shadowsocks-Audit-Spool-Epoch": "c" * 32,
            "X-Shadowsocks-Audit-First-Sequence": "1",
            "X-Shadowsocks-Audit-Last-Sequence": "1",
            "X-Shadowsocks-Audit-Event-Count": "1",
        }
        return body, (200, headers, body)

    def test_empty_lease_accepts_only_a_bare_204(self) -> None:
        self.assertEqual(self.collect([(204, {}, b"")]), 0)

    def test_empty_lease_rejects_signed_batch_metadata(self) -> None:
        for name, value in (
            ("X-Shadowsocks-Audit-Batch-Id", "b" * 32),
            ("X-Shadowsocks-Audit-Spool-Epoch", "c" * 32),
            ("X-Shadowsocks-Audit-First-Sequence", "1"),
            ("X-Shadowsocks-Audit-Last-Sequence", "1"),
            ("X-Shadowsocks-Audit-Event-Count", "1"),
            ("X-Shadowsocks-Audit-Schema", "1"),
            ("X-Shadowsocks-Audit-Body-SHA256", sha256_hex(b"")),
            ("Content-Type", "application/x-ndjson"),
        ):
            with self.subTest(header=name):
                with self.assertRaisesRegex(CollectorError, "lease metadata"):
                    self.collect([(204, {name: value}, b"")])

    def test_ack_body_must_match_the_fixed_bytes(self) -> None:
        _, lease = self.lease_response()
        acked = b'{"schema_version":1,"status":"acked"}'
        self.assertEqual(self.collect([lease, (200, {"Content-Type": "application/json"}, acked)]), 1)
        for body in (
            b'{"status":"acked","schema_version":1}',
            b'{"schema_version": 1, "status": "acked"}',
            acked + b"\n",
        ):
            with self.subTest(body=body):
                with self.assertRaisesRegex(CollectorError, "did not acknowledge"):
                    self.collect([lease, (200, {"Content-Type": "application/json"}, body)])
