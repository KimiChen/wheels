#!/usr/bin/env python3
"""Portable regression checks for the native audit integration assertions."""

from __future__ import annotations

import unittest

from integration_audit import (
    _validate_event_ack,
    _validate_health_body,
    _validate_hello_ack,
    _validate_hello_nack,
)
from mock_collector import canonical_json


RUNTIME = "0123456789abcdef0123456789abcdef"
NODE = "node-example-01"


class AuditIntegrationAssertionTest(unittest.TestCase):
    def test_producer_requires_exact_success_acknowledgements(self) -> None:
        _validate_hello_ack({"protocol_version": 1, "frame_type": "hello_ack", "status": "ready"})
        _validate_event_ack(
            {
                "protocol_version": 1,
                "frame_type": "ack",
                "event_id": f"{RUNTIME}:1",
                "status": "stored",
                "spool_epoch": "a" * 32,
                "spool_sequence": "1",
            },
            RUNTIME,
        )
        with self.assertRaises(RuntimeError):
            _validate_hello_ack(
                {
                    "protocol_version": 1,
                    "frame_type": "hello_nack",
                    "error_code": "producer_busy",
                    "retryable": True,
                }
            )
        with self.assertRaises(RuntimeError):
            _validate_event_ack(
                {
                    "protocol_version": 1,
                    "frame_type": "event_nack",
                    "event_id": f"{RUNTIME}:1",
                    "error_code": "storage_unavailable",
                    "retryable": True,
                },
                RUNTIME,
            )

    def test_identity_negatives_require_exact_nonretryable_codes(self) -> None:
        for code in ("unauthorized_peer", "node_mismatch"):
            value = {
                "protocol_version": 1,
                "frame_type": "hello_nack",
                "error_code": code,
                "retryable": False,
            }
            _validate_hello_nack(value, code)
            with self.assertRaises(RuntimeError):
                _validate_hello_nack({**value, "retryable": True}, code)

    def test_health_requires_http_200_healthy_canonical_body(self) -> None:
        health = {
            "schema_version": 1,
            "node_id": NODE,
            "status": "ok",
            "producer_connected": False,
            "producer_runtime_id": None,
            "last_ingest_at_unix_ms": None,
            "spool_epoch": "b" * 32,
            "spool_bytes": "0",
            "max_spool_bytes": "5368709120",
            "sealed_batches": "0",
            "oldest_unacked_at_unix_ms": None,
            "stored_records": "0",
            "storage_rejected_attempts": "0",
            "evicted_unacked_records": "0",
        }
        body = canonical_json(health)
        _validate_health_body(200, body, NODE)
        with self.assertRaises(RuntimeError):
            _validate_health_body(503, body, NODE)
        with self.assertRaises(RuntimeError):
            _validate_health_body(200, canonical_json({**health, "status": "degraded"}), NODE)
        with self.assertRaises(RuntimeError):
            _validate_health_body(200, b'{ "schema_version": 1 }', NODE)


if __name__ == "__main__":
    unittest.main()
