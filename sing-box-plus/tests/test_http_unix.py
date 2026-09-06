"""HTTP/UDS 传输层的失败关闭测试。

每一条都对应一种「宽容解析会把残缺响应当成成功采集」的情形。
"""

from __future__ import annotations

import os
import socket
import sys
import tempfile
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import http_unix  # noqa: E402


class RawServer:
    """按固定字节回应一次请求的最小 UDS 服务端。"""

    def __init__(self, payload: bytes, close_early: bool = False) -> None:
        self.directory = tempfile.mkdtemp(prefix="sbp", dir="/tmp")
        self.path = os.path.join(self.directory, "s.sock")
        self.payload = payload
        self.close_early = close_early
        self.socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.socket.bind(self.path)
        self.socket.listen(4)
        self.thread = threading.Thread(target=self._serve, daemon=True)
        self.thread.start()

    def _serve(self) -> None:
        try:
            connection, _ = self.socket.accept()
        except OSError:
            return
        with connection:
            try:
                connection.recv(65536)
                if not self.close_early:
                    connection.sendall(self.payload)
            except OSError:
                pass

    def close(self) -> None:
        self.socket.close()
        try:
            os.unlink(self.path)
        except OSError:
            pass
        os.rmdir(self.directory)


class HTTPUnixTest(unittest.TestCase):
    def _run(self, payload: bytes, close_early: bool = False):
        server = RawServer(payload, close_early)
        self.addCleanup(server.close)
        return http_unix.request(server.path, "GET", "/v2/snapshot")

    def test_normal_response(self):
        body = b'{"schema_version":2}\n'
        payload = (
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
            + f"Content-Length: {len(body)}\r\n".encode()
            + b"Connection: close\r\n\r\n"
            + body
        )
        response = self._run(payload)
        self.assertEqual(response.status, 200)
        self.assertEqual(response.json(), {"schema_version": 2})

    def test_content_length_mismatch_is_rejected(self):
        # 截断的 JSON 有可能仍然解析成功，那会让采集端把残缺快照当成完整快照入账。
        body = b'{"schema_version":2}\n'
        payload = (
            b"HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\n" + body
        )
        with self.assertRaises(http_unix.HTTPUnixError):
            self._run(payload)

    def test_missing_content_length_is_rejected(self):
        payload = b"HTTP/1.1 200 OK\r\n\r\n{}"
        with self.assertRaises(http_unix.HTTPUnixError):
            self._run(payload)

    def test_chunked_is_rejected(self):
        payload = b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 2\r\n\r\n{}"
        with self.assertRaises(http_unix.HTTPUnixError):
            self._run(payload)

    def test_connection_closed_without_response(self):
        with self.assertRaises(http_unix.HTTPUnixError):
            self._run(b"", close_early=True)

    def test_bad_status_line(self):
        payload = b"HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n"
        with self.assertRaises(http_unix.HTTPUnixError):
            self._run(payload)

    def test_query_is_refused_locally(self):
        server = RawServer(b"")
        self.addCleanup(server.close)
        with self.assertRaises(http_unix.HTTPUnixError):
            http_unix.request(server.path, "GET", "/v2/snapshot?a=1")

    def test_missing_socket(self):
        with self.assertRaises(http_unix.HTTPUnixError):
            http_unix.request("/tmp/sbp-nonexistent.sock", "GET", "/v2/snapshot")


class ShimTest(unittest.TestCase):
    def test_tests_shim_delegates_to_single_implementation(self):
        # tests/http_unix.py 是再导出 shim，不是第二份实现：
        # 只要 request 的定义位置落在 scripts/http_unix.py，两处就不可能漂移。
        self.assertEqual(
            Path(http_unix.request.__code__.co_filename).resolve(),
            http_unix.IMPLEMENTATION_PATH,
        )


if __name__ == "__main__":
    unittest.main()
