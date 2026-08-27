#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import tempfile
import unittest
from pathlib import Path

from mock_collector import (
    CollectorError,
    MockCollector,
    ResponseMetadata,
    canonical_request,
    canonical_response,
    parse_lease,
    request_mac,
    sha256_hex,
    strict_json,
)


KEY = bytes.fromhex("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
NODE = "node-example-01"
NONCE = "0123456789abcdef0123456789abcdef"


class MockCollectorProtocolTest(unittest.TestCase):
    def test_request_golden_vector(self) -> None:
        body = b'{"schema_version":1}'
        digest = sha256_hex(body)
        canonical = canonical_request(
            "POST", "/v1/audit/lease", NODE, "1787587200", NONCE, digest
        )
        self.assertEqual(
            canonical,
            b"SHADOWSOCKS-AUDIT-V1\nPOST\n/v1/audit/lease\nnode-example-01\n"
            b"1787587200\n0123456789abcdef0123456789abcdef\n"
            b"a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23",
        )
        self.assertEqual(
            request_mac(KEY, canonical),
            "c03af3fa5fab585d4f7edd738a4fba9755551d01502402486f0bafc3816659ab",
        )

    def test_response_golden_vector(self) -> None:
        metadata = ResponseMetadata(
            status=200,
            content_type="application/x-ndjson",
            schema="1",
            lease_body_sha256="a" * 64,
            node_id=NODE,
            batch_id="b" * 32,
            spool_epoch="c" * 32,
            first_sequence="1",
            last_sequence="1",
            event_count="1",
            response_sha256="d" * 64,
            response_mac="",
            request_nonce=NONCE,
        )
        self.assertEqual(
            hashlib.sha256(canonical_response(metadata)).hexdigest(),
            "4ff4330b2039925ab7b77f788eabdf4e2c9f434de0025a1427a80132e2b3d1e8",
        )
        self.assertEqual(
            request_mac(KEY, canonical_response(metadata)),
            "05e06f725faca49f6e18c71aefb99d7a443c2132848e49bded90ad63e85a0fd9",
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
            self.assertTrue(state.exists())
            restored = MockCollector(NODE, KEY, state)
            self.assertEqual(len(restored.events), 1)


if __name__ == "__main__":
    unittest.main()
