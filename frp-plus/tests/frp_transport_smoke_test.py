import io
from pathlib import Path
import socket
import threading
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import frp_transport_smoke as fixture
from smoke import PAYLOAD, SmokeFailure


class Chunks:
    def __init__(self, chunks):
        self.chunks = iter(chunks)
    def read1(self, _):
        return next(self.chunks, b"")


class TransportFixtureTests(unittest.TestCase):
    def test_failure_marker_survives_all_chunk_boundaries(self):
        marker = fixture.FAILURE_MARKERS["untrusted_ca"][0]
        for split in range(1, len(marker)):
            child = fixture.ObservedChild.__new__(fixture.ObservedChild)
            child.failure_markers = fixture.FAILURE_MARKERS
            child.failures = {key: threading.Event() for key in child.failure_markers}
            child.registered, child.logged_in = threading.Event(), threading.Event()
            child.process = SimpleNamespace(stdout=Chunks([b"prefix " + marker[:split], marker[split:] + b" suffix"]))
            child._read_output()
            self.assertTrue(child.failures["untrusted_ca"].is_set())
            self.assertFalse(child.logged_in.is_set())

    def test_closed_port_or_unrelated_exit_cannot_pass_rejection(self):
        for code, login, reason, listening, accepted in (
            (1, False, False, False, False), (0, False, True, False, False),
            (1, True, True, False, False), (1, False, True, True, False),
            (1, False, True, False, True),
        ):
            event, logged = threading.Event(), threading.Event()
            if reason: event.set()
            if login: logged.set()
            child = SimpleNamespace(process=SimpleNamespace(returncode=code, poll=lambda: code),
                                    reader=SimpleNamespace(join=lambda **_: None), logged_in=logged,
                                    failures={"untrusted_ca": event})
            server = SimpleNamespace(ensure_running=lambda: None)
            with self.subTest(code=code, login=login, reason=reason, listening=listening), patch.object(fixture, "port_open", return_value=listening):
                if accepted:
                    fixture.require_rejection(child, server, 1, "untrusted_ca", 1)
                else:
                    with self.assertRaises(SmokeFailure):
                        fixture.require_rejection(child, server, 1, "untrusted_ca", 1)

    def test_business_payload_is_exact(self):
        with fixture.transport_echo_server() as port:
            fixture.require_payload(port)

    def test_truncated_or_corrupt_payload_is_not_a_readiness_retry(self):
        for body in (PAYLOAD[:-1], b"x" * len(PAYLOAD)):
            class Connection:
                def __enter__(self): return self
                def __exit__(self, *_): pass
                def settimeout(self, _): pass
                def sendall(self, _): pass
                def recv(self, size): return buffer.read(size)
            buffer = io.BytesIO(body)
            with patch.object(socket, "create_connection", return_value=Connection()):
                with self.assertRaisesRegex(SmokeFailure, "truncated or altered"):
                    fixture.require_payload(1)

    def test_deadline_failure_cannot_become_authentication_success(self):
        with patch.object(socket, "create_connection", side_effect=TimeoutError()):
            with self.assertRaisesRegex(SmokeFailure, "deadline"):
                fixture.require_payload(1)

    def test_wss_requires_explicit_termination(self):
        with self.assertRaisesRegex(ValueError, "termination"):
            fixture.config_texts("wss", "v1", 10001, 10002, 10003, 10004, "test", Path("cert"), Path("key"))


if __name__ == "__main__":
    unittest.main()
