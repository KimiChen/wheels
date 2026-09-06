#!/usr/bin/env python3
"""HTTP/1.1-over-Unix-stream 客户端：单请求单响应、严格解析、全程有界。

这是本项目唯一一份 HTTP/UDS 传输实现。参考实现把同一份解析器复制成了两份并靠门禁
维持一致，本项目改为单实现 + 一个再导出 shim（tests/http_unix.py），
从结构上消除漂移的可能，而不是靠纪律去追。

严格的理由：本客户端读的是计费数据。宽容解析在这里等于「把半截响应当成一次成功采集」，
而那正是漏账与重复入账的来源。
"""

from __future__ import annotations

import json
import socket
from typing import Mapping

DEFAULT_TIMEOUT = 10.0
MAX_STATUS_LINE = 8 * 1024
MAX_HEADER_BYTES = 64 * 1024
MAX_BODY_BYTES = 32 * 1024 * 1024


class HTTPUnixError(Exception):
    """传输层或协议层错误。调用方必须把它当作「本次采集失败」，不得当作空快照。"""


class Response:
    __slots__ = ("status", "headers", "body")

    def __init__(self, status: int, headers: Mapping[str, str], body: bytes) -> None:
        self.status = status
        self.headers = headers
        self.body = body

    def json(self) -> object:
        try:
            return json.loads(self.body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise HTTPUnixError(f"响应体不是合法 UTF-8 JSON：{error}") from error


def request(
    socket_path: str,
    method: str,
    target: str,
    body: bytes | None = None,
    timeout: float = DEFAULT_TIMEOUT,
) -> Response:
    if "?" in target or "#" in target:
        raise HTTPUnixError("本协议禁 query：请求目标不得含 ? 或 #")
    connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    connection.settimeout(timeout)
    try:
        try:
            connection.connect(socket_path)
        except OSError as error:
            raise HTTPUnixError(f"连接 {socket_path} 失败：{error}") from error
        lines = [f"{method} {target} HTTP/1.1", "Host: localhost", "Connection: close"]
        if body is not None:
            lines.append("Content-Type: application/json")
            lines.append(f"Content-Length: {len(body)}")
        head = ("\r\n".join(lines) + "\r\n\r\n").encode("ascii")
        try:
            connection.sendall(head)
            if body:
                connection.sendall(body)
        except OSError as error:
            # 服务端可能已经用 413 回绝并关闭连接；此时仍应把它已写回的响应读完。
            if body is None:
                raise HTTPUnixError(f"发送请求失败：{error}") from error
        raw = _read_all(connection)
    finally:
        connection.close()
    return _parse_response(raw)


def _read_all(connection: socket.socket) -> bytes:
    chunks: list[bytes] = []
    total = 0
    while True:
        try:
            chunk = connection.recv(64 * 1024)
        except socket.timeout as error:
            raise HTTPUnixError("读取响应超时") from error
        except OSError as error:
            raise HTTPUnixError(f"读取响应失败：{error}") from error
        if not chunk:
            break
        total += len(chunk)
        if total > MAX_BODY_BYTES + MAX_HEADER_BYTES:
            raise HTTPUnixError("响应超过大小上限")
        chunks.append(chunk)
    return b"".join(chunks)


def _parse_response(raw: bytes) -> Response:
    separator = raw.find(b"\r\n\r\n")
    if separator < 0:
        raise HTTPUnixError("响应缺少头体分隔（连接可能被中途关闭）")
    head = raw[:separator]
    body = raw[separator + 4 :]
    lines = head.split(b"\r\n")
    if not lines:
        raise HTTPUnixError("响应为空")
    if len(lines[0]) > MAX_STATUS_LINE:
        raise HTTPUnixError("状态行过长")
    try:
        status_line = lines[0].decode("ascii")
    except UnicodeDecodeError as error:
        raise HTTPUnixError("状态行不是 ASCII") from error
    parts = status_line.split(" ", 2)
    if len(parts) < 2 or parts[0] != "HTTP/1.1":
        raise HTTPUnixError(f"状态行异常：{status_line!r}")
    try:
        status = int(parts[1])
    except ValueError as error:
        raise HTTPUnixError(f"状态码异常：{status_line!r}") from error

    headers: dict[str, str] = {}
    if len(head) > MAX_HEADER_BYTES:
        raise HTTPUnixError("响应头过长")
    for line in lines[1:]:
        if not line:
            continue
        try:
            text = line.decode("ascii")
        except UnicodeDecodeError as error:
            raise HTTPUnixError("响应头不是 ASCII") from error
        if ":" not in text:
            raise HTTPUnixError(f"响应头异常：{text!r}")
        name, _, value = text.partition(":")
        headers[name.strip().lower()] = value.strip()

    if "transfer-encoding" in headers:
        raise HTTPUnixError("本协议不支持分块传输")
    if "content-length" not in headers:
        raise HTTPUnixError("响应缺少 Content-Length")
    try:
        declared = int(headers["content-length"])
    except ValueError as error:
        raise HTTPUnixError("Content-Length 不是整数") from error
    # 长度不符必须失败：截断的 JSON 有可能仍然解析成功（例如尾部对象被砍掉），
    # 那会让采集端把一份残缺快照当成完整快照入账。
    if declared != len(body):
        raise HTTPUnixError(f"Content-Length 与实际长度不符：{declared} != {len(body)}")
    return Response(status, headers, body)
