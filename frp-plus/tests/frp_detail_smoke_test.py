"""Validate the bounded detail-smoke capture, without launching FRP binaries."""

import json
import unittest

from frp_detail_smoke import Capture, ClientFrames, MAX_FRAME, check_private_fields, item
from smoke import SmokeFailure


UPGRADE = b"GET /agent/v1/ws HTTP/1.1\r\nAuthorization: Bearer synthetic-private-token\r\n\r\n"


def ws(payload, opcode=1, final=True):
    data = payload if isinstance(payload, bytes) else json.dumps(payload).encode()
    mask = b"mask"
    header = bytes([(128 if final else 0) | opcode])
    if len(data) < 126:
        header += bytes([128 | len(data)])
    elif len(data) < 65536:
        header += b"\xfe" + len(data).to_bytes(2, "big")
    else:
        header += b"\xff" + len(data).to_bytes(8, "big")
    return header + mask + bytes(value ^ mask[i % 4] for i, value in enumerate(data))


def rpc(method, sequence, session="test-session", **fields):
    return {"jsonrpc": "2.0", "method": method,
            "params": {"session_id": session, "sequence": sequence, **fields}}


class CaptureTests(unittest.TestCase):
    def test_incremental_fragmented_json_and_interleaved_control(self):
        seen = []
        parser = ClientFrames(seen.append)
        message = {"message": "分片详情" * 100}
        encoded = json.dumps(message, ensure_ascii=False).encode()
        stream = (UPGRADE + ws(encoded[:71], final=False) + ws(b"ping", opcode=9)
                  + ws(encoded[71:], opcode=0) + ws({"next": True}))
        for index in range(0, len(stream), 7):
            parser.feed(stream[index:index + 7])
        self.assertEqual(seen, [message, {"next": True}])
        self.assertEqual(parser.buffer, b"")
        self.assertIsNone(parser.fragment)
        self.assertNotIn(b"synthetic-private-token", parser.buffer)

    def test_large_frame_with_64_bit_length(self):
        seen = []
        parser = ClientFrames(seen.append)
        message = {"text": "x" * 70000}
        stream = UPGRADE + ws(message)
        for index in range(0, len(stream), 4096):
            parser.feed(stream[index:index + 4096])
        self.assertEqual(seen, [message])

    def test_rejects_oversized_headers_frames_and_joined_messages(self):
        with self.assertRaisesRegex(SmokeFailure, "header exceeded"):
            ClientFrames(lambda _: None).feed(b"x" * 16385)
        parser = ClientFrames(lambda _: None)
        with self.assertRaisesRegex(SmokeFailure, "frame exceeded"):
            parser.feed(UPGRADE + b"\x81\xff" + (MAX_FRAME + 1).to_bytes(8, "big"))
        parser = ClientFrames(lambda _: None)
        parser.feed(UPGRADE + ws(b"x" * (MAX_FRAME // 2), final=False))
        with self.assertRaisesRegex(SmokeFailure, "message exceeded"):
            parser.feed(ws(b"x" * (MAX_FRAME // 2 + 1), opcode=0))

    def test_rejects_unmasked_continuations_and_invalid_json_without_echo(self):
        for frame in (b"\x81\x01x", ws(b"private-value", opcode=0), ws(b"private-value")):
            with self.subTest(frame_size=len(frame)):
                with self.assertRaises(SmokeFailure) as caught:
                    ClientFrames(lambda _: None).feed(UPGRADE + frame)
                self.assertNotIn("private-value", str(caught.exception))

    def test_metrics_and_detail_share_sequence_and_snapshot_owns_data(self):
        capture = Capture()
        capture.observe(rpc("hello", 1, capabilities=["frp.detail.v1"]))
        capture.observe(rpc("report", 2, metrics={"cpu": {"value": 1}}))
        capture.observe(rpc("frp.detail", 3, detail={"state": "ready", "proxies": []}))
        capture.observe(rpc("ping", 4))
        state = capture.snapshot()
        self.assertEqual((state["reports"], state["details"]), (1, 1))
        state["detail"]["proxies"].append({"name": "mutation"})
        self.assertEqual(capture.snapshot()["detail"]["proxies"], [])
        with self.assertRaisesRegex(SmokeFailure, "sequence did not increase"):
            capture.observe(rpc("report", 4, metrics={}))
        capture.observe(rpc("hello", 1, session="new-session", capabilities=["frp.detail.v1"]))
        self.assertIsNone(capture.snapshot()["detail"])
        with self.assertRaisesRegex(SmokeFailure, "sequence did not increase"):
            capture.observe(rpc("frp.detail", 5, detail={}))

    def test_capture_requires_negotiation_and_surfaces_relay_fault(self):
        capture = Capture()
        with self.assertRaisesRegex(SmokeFailure, "capability"):
            capture.observe(rpc("hello", 1, capabilities=["frp.v1"]))
        capture.fail("capture failed safely")
        with self.assertRaisesRegex(SmokeFailure, "failed safely"):
            capture.snapshot()

    def test_busy_is_a_valid_transition_without_reusing_old_ready_items(self):
        capture = Capture()
        capture.observe(rpc("hello", 1, capabilities=["frp.detail.v1"]))
        proxy = {"name": "example", "status": "running"}
        capture.observe(rpc("frp.detail", 2, detail={"state": "ready", "proxies": [proxy]}))
        self.assertEqual(item(capture.snapshot()["detail"], "proxies", "example"), proxy)
        capture.observe(rpc("frp.detail", 3, detail={"state": "busy", "proxies": []}))
        self.assertEqual(item(capture.snapshot()["detail"], "proxies", "example"), {})
        capture.observe(rpc("report", 4, metrics={}))
        capture.observe(rpc("frp.detail", 5, detail={"state": "ready", "proxies": [proxy]}))
        state = capture.snapshot()
        self.assertEqual(item(state["detail"], "proxies", "example"), proxy)
        self.assertEqual(state["reports"], 1)

    def test_public_privacy_check_finds_nested_detail_and_secret(self):
        check_private_fields({"nodes": [{"metrics": {"cpu": {"value": 2}}}]}, ("synthetic-secret",))
        for payload in ({"nodes": [{"frp_detail": {}}]}, {"nested": [{"visitors": []}]},
                        {"note": "synthetic-secret"}):
            with self.subTest(keys=list(payload)):
                with self.assertRaises(SmokeFailure):
                    check_private_fields(payload, ("synthetic-secret",))


if __name__ == "__main__":
    unittest.main()
