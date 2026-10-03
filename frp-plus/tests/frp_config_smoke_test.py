"""Security/boundary checks for the synthetic config controller, no FRP process."""

import base64
import copy
import json
import threading
import time
import unittest

from frp_config_smoke import (CAPABILITIES, MAX_FRAME, MAX_HEADER, MAX_PENDING,
                              ControlFrames, Controller, authenticate_upgrade,
                              change, expect_operation, make_command, operation_fields,
                              ws_frame)
from smoke import SmokeFailure


TOKEN = "disposable-private-controller-token"


def upgrade(token=TOKEN, extra=""):
    return ("GET /agent/v1/ws HTTP/1.1\r\nHost: 127.0.0.1\r\n"
            f"Authorization: Bearer {token}\r\nUpgrade: websocket\r\nConnection: keep-alive,Upgrade\r\n"
            "Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" + extra + "\r\n").encode()


def client_frame(payload, opcode=1, final=True):
    raw = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
    header = bytearray(ws_frame(raw, opcode))
    header[0] = (header[0] & 127) | (128 if final else 0)
    header[1] |= 128
    offset = 2 + (2 if header[1] & 127 == 126 else 8 if header[1] & 127 == 127 else 0)
    mask = b"mask"
    return bytes(header[:offset]) + mask + bytes(value ^ mask[index % 4] for index, value in enumerate(raw))


def rpc(method, sequence, session="test-session", **fields):
    return {"jsonrpc": "2.0", "method": method,
            "params": {"schema": 1, "session_id": session, "sequence": sequence, **fields}}


class Sink:
    def __init__(self):
        self.data = []

    def sendall(self, data):
        self.data.append(data)


def controller_fixture():
    controller = Controller.__new__(Controller)
    controller.lock, controller.write_lock = threading.RLock(), threading.Lock()
    controller.hidden = (TOKEN,)
    controller.connection = Sink()
    controller.session, controller.sequence, controller.down_sequence = None, 0, 1
    controller.sessions, controller.reports, controller.results_seen = 0, 0, 0
    controller.detail, controller.pending = None, {}
    controller.fault, controller.last_sent = None, 0
    return controller


class ControllerTests(unittest.TestCase):
    def test_authentication_precedes_upgrade_and_capabilities(self):
        status, accept = authenticate_upgrade(upgrade(), TOKEN)
        self.assertEqual(status, 101)
        self.assertEqual(accept, "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=")
        for raw in (upgrade("wrong"), upgrade().replace(b"Authorization:", b"Absent:")):
            self.assertEqual(authenticate_upgrade(raw, TOKEN), (401, ""))
        for raw in (upgrade(extra="Authorization: Bearer " + TOKEN + "\r\n"),
                    upgrade().replace(b"/agent/v1/ws", b"/other"),
                    upgrade().replace(b"dGhlIHNhbXBsZSBub25jZQ==", b"bad-key"),
                    upgrade().replace(b"Version: 13", b"Version: 12"),
                    b"x" * (MAX_HEADER + 1), b"\xff\r\n\r\n"):
            self.assertEqual(authenticate_upgrade(raw, TOKEN), (400, ""))

    def test_fragmentation_utf8_and_interleaved_ping_are_lossless(self):
        observed, controls = [], []
        parser = ControlFrames(observed.append, lambda op, data: controls.append((op, data)))
        value = {"text": "配置\u0000" * 150}
        raw = json.dumps(value, ensure_ascii=False).encode()
        stream = client_frame(raw[:71], final=False) + client_frame(b"ping", 9) + client_frame(raw[71:], opcode=0)
        for offset in range(0, len(stream), 7):
            parser.feed(stream[offset:offset + 7])
        self.assertEqual(observed, [value])
        self.assertEqual(controls, [(9, b"ping")])
        self.assertEqual(parser.buffer, b"")
        self.assertIsNone(parser.fragment)

    def test_frames_messages_and_buffers_have_independent_bounds(self):
        value = {"value": "x" * 70000}
        observed = []
        parser = ControlFrames(observed.append, lambda *_: None)
        raw = client_frame(value)
        for offset in range(0, len(raw), 4096):
            parser.feed(raw[offset:offset + 4096])
        self.assertEqual(observed, [value])
        with self.assertRaisesRegex(SmokeFailure, "frame exceeded"):
            ControlFrames(lambda _: None, lambda *_: None).feed(b"\x81\xff" + (MAX_FRAME + 1).to_bytes(8, "big"))
        with self.assertRaisesRegex(SmokeFailure, "buffer exceeded"):
            ControlFrames(lambda _: None, lambda *_: None).feed(b"x" * (MAX_FRAME + 15))
        parser = ControlFrames(lambda _: None, lambda *_: None)
        parser.feed(client_frame(b"x" * (MAX_FRAME // 2), final=False))
        with self.assertRaisesRegex(SmokeFailure, "message exceeded"):
            parser.feed(client_frame(b"x" * (MAX_FRAME // 2 + 1), opcode=0))

    def test_invalid_payload_diagnostics_never_echo_private_data(self):
        frames = [b"\x81\x01x", client_frame(TOKEN.encode()), client_frame(TOKEN.encode(), opcode=0),
                  client_frame(b"x", opcode=9, final=False), client_frame(b"x", opcode=2)]
        for frame in frames:
            with self.subTest(size=len(frame)):
                with self.assertRaises(SmokeFailure) as caught:
                    ControlFrames(lambda _: None, lambda *_: None).feed(frame)
                self.assertNotIn(TOKEN, str(caught.exception))

    def test_negotiation_common_sequence_results_and_snapshot_ownership(self):
        controller = controller_fixture()
        sink = controller.connection
        controller._observe(rpc("hello", 1, capabilities=CAPABILITIES), sink)
        self.assertEqual(controller.snapshot()["sessions"], 1)
        self.assertIn(b'"config.manage.v1"', sink.data[0])
        controller._observe(rpc("report", 2, metrics={}), sink)
        controller._observe(rpc("frp.detail", 4, detail={"state": "busy", "proxies": []}), sink)
        controller.pending["request"] = None
        controller._observe(rpc("config.result", 5, request_id="request", code="ok"), sink)
        self.assertEqual(controller.snapshot()["reports"], 1)
        self.assertEqual(controller.snapshot()["results"], 1)
        state = controller.snapshot()
        state["detail"]["proxies"].append("mutation")
        self.assertEqual(controller.snapshot()["detail"]["proxies"], [])
        with self.assertRaisesRegex(SmokeFailure, "increasing sequence"):
            controller._observe(rpc("report", 5, metrics={}), sink)
        with self.assertRaisesRegex(SmokeFailure, "increasing sequence"):
            controller._observe(rpc("report", 6, session="previous", metrics={}), sink)

    def test_rejects_missing_capability_duplicate_unsolicited_and_private_result(self):
        controller = controller_fixture()
        with self.assertRaisesRegex(SmokeFailure, "capabilities"):
            controller._observe(rpc("hello", 1, capabilities=["metrics.v1"]), controller.connection)
        controller._observe(rpc("hello", 1, capabilities=CAPABILITIES), controller.connection)
        with self.assertRaisesRegex(SmokeFailure, "unexpected or duplicate"):
            controller._observe(rpc("config.result", 2, request_id="unknown"), controller.connection)
        controller.pending["request"] = None
        controller._observe(rpc("config.result", 3, request_id="request"), controller.connection)
        with self.assertRaisesRegex(SmokeFailure, "unexpected or duplicate"):
            controller._observe(rpc("config.result", 4, request_id="request"), controller.connection)
        with self.assertRaises(SmokeFailure) as caught:
            controller._observe(rpc("config.result", 5, value=TOKEN), controller.connection)
        self.assertNotIn(TOKEN, str(caught.exception))

    def test_pending_queue_is_bounded_before_any_command_is_written(self):
        controller = controller_fixture()
        controller._observe(rpc("hello", 1, capabilities=CAPABILITIES), controller.connection)
        controller.pending = {str(index): None for index in range(MAX_PENDING)}
        count = len(controller.connection.data)
        with self.assertRaisesRegex(SmokeFailure, "pending result limit"):
            controller.send("inspect")
        self.assertEqual(len(controller.connection.data), count)

    def test_command_metadata_uses_distinct_ids_array_changes_and_local_deadline(self):
        before = int(time.time() * 1000)
        first = make_command("inspect", "", "session", 2, command_seconds=5)
        second = make_command("inspect", "", "session", 3)
        self.assertEqual(first["changes"], [])
        self.assertNotEqual(first["request_id"], second["request_id"])
        self.assertGreaterEqual(first["deadline_at_ms"], before + 5000)
        self.assertLess(first["deadline_at_ms"], before + 5500)
        self.assertEqual(first["operation_deadline_at_ms"], 0)
        self.assertNotIn("secret", first)
        secret = change("private", type_="stcp", reference="reference")
        self.assertEqual(secret["secrets"], [{"path": "secretKey", "mode": "reference", "reference": "reference"}])
        self.assertNotIn("value", secret["secrets"][0])

    def test_expected_terminal_facts_distinguish_old_from_candidate(self):
        result = {"code": "ok", "operation": {"state": "confirmed", "store_persisted": True,
                  "runtime_applied": True, "runtime_loaded": True, "resources_ready": True, "business_checked": False}}
        expect_operation(result, "confirmed")
        for field in ("store_persisted", "runtime_applied", "runtime_loaded", "resources_ready"):
            bad = copy.deepcopy(result)
            bad["operation"][field] = False
            with self.assertRaises(SmokeFailure):
                expect_operation(bad, "confirmed")
        result["operation"].update(state="rolled_back", store_persisted=False, runtime_applied=False)
        expect_operation(result, "rolled_back")
        result["operation"]["business_checked"] = True
        with self.assertRaisesRegex(SmokeFailure, "business verification"):
            expect_operation(result, "rolled_back")
        operation = dict(operation_id="operation", base_revision="base", context_revision="context", candidate_digest="candidate", private="ignored")
        self.assertEqual(operation_fields({"operation": operation}), {key: value for key, value in operation.items() if key != "private"})
        expect_operation({"code": "conflict", "operation": {"state": "conflict"}}, "conflict")
        with self.assertRaises(SmokeFailure):
            expect_operation({"code": "ok", "operation": {"state": "conflict"}}, "conflict")


if __name__ == "__main__":
    unittest.main()
