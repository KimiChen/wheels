#!/usr/bin/env python3
"""配额下发客户端：读取一份剩余额度清单后 PUT /v2/quota。

全量覆盖语义：清单里没有的计费身份被视为**无限额度**。这不是「保持不变」，
下发前请确认清单是完整的限额清单，而不是一次增量。

epoch 维护与 409 重推：进程侧的额度是纯内存的，重启即消失。
runtime_id 不符会拿到 409，本客户端据此重新读取快照里的 runtime_id 并重推一次。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import http_unix  # noqa: E402

SCHEMA_VERSION = 2
DEFAULT_QUOTA_SOCKET = "/run/sing-box-plus/quota.sock"
DEFAULT_STATS_SOCKET = "/run/sing-box-plus/user-stats.sock"


def load_entries(path: Path | None) -> list[dict]:
    raw = sys.stdin.read() if path is None else path.read_text(encoding="utf-8")
    payload = json.loads(raw)
    if not isinstance(payload, list):
        raise ValueError("清单必须是数组，每项形如 {inbound_tag, name, remaining_bytes}")
    entries = []
    for index, item in enumerate(payload):
        if not isinstance(item, dict) or set(item) != {"inbound_tag", "name", "remaining_bytes"}:
            raise ValueError(f"entries[{index}] 键集合错误：{item}")
        remaining = item["remaining_bytes"]
        if type(remaining) is not int or isinstance(remaining, bool) or remaining < 0:
            raise ValueError(f"entries[{index}].remaining_bytes 必须是非负整数")
        entries.append(item)
    return entries


def read_identity(stats_socket: str, timeout: float) -> tuple[str, str]:
    response = http_unix.request(stats_socket, "GET", "/v2/snapshot", timeout=timeout)
    if response.status != 200:
        raise RuntimeError(f"读取快照失败：HTTP {response.status}")
    snapshot = response.json()
    return snapshot["node_id"], snapshot["runtime_id"]


def put_quota(quota_socket: str, node_id: str, runtime_id: str, epoch: int, entries: list[dict], timeout: float):
    body = json.dumps(
        {
            "schema_version": SCHEMA_VERSION,
            "node_id": node_id,
            "runtime_id": runtime_id,
            "epoch": epoch,
            "entries": entries,
        },
        ensure_ascii=False,
    ).encode("utf-8")
    return http_unix.request(quota_socket, "PUT", "/v2/quota", body=body, timeout=timeout)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="向 sing-box-plus 下发全量剩余额度表")
    parser.add_argument("--quota-socket", default=DEFAULT_QUOTA_SOCKET)
    parser.add_argument("--stats-socket", default=DEFAULT_STATS_SOCKET)
    parser.add_argument("--entries", type=Path, help="清单 JSON 文件；省略则从 stdin 读")
    parser.add_argument("--epoch", type=int, required=True, help="单调递增的下发序号")
    parser.add_argument("--node-id", help="省略则从快照读取")
    parser.add_argument("--runtime-id", help="省略则从快照读取")
    parser.add_argument("--timeout", type=float, default=10.0)
    args = parser.parse_args(argv)

    if args.epoch <= 0:
        print("错误：epoch 必须是正整数", file=sys.stderr)
        return 2
    try:
        entries = load_entries(args.entries)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"错误：{error}", file=sys.stderr)
        return 2

    node_id, runtime_id = args.node_id, args.runtime_id
    try:
        if not node_id or not runtime_id:
            node_id, runtime_id = read_identity(args.stats_socket, args.timeout)
        response = put_quota(args.quota_socket, node_id, runtime_id, args.epoch, entries, args.timeout)
        if response.status == 409:
            # 409 可能是 runtime 变了（进程重启），也可能是 epoch 未前进。
            # 前者必须按新 runtime 重推一次；后者重推同样会 409，不会造成重复扣减。
            fresh_node, fresh_runtime = read_identity(args.stats_socket, args.timeout)
            if (fresh_node, fresh_runtime) != (node_id, runtime_id):
                print(
                    f"提示：runtime 已变化（{runtime_id} -> {fresh_runtime}），按新 runtime 重推。",
                    file=sys.stderr,
                )
                response = put_quota(
                    args.quota_socket, fresh_node, fresh_runtime, args.epoch, entries, args.timeout
                )
    except (http_unix.HTTPUnixError, RuntimeError) as error:
        print(f"错误：{error}", file=sys.stderr)
        if "reset" in str(error).lower() or "断开" in str(error):
            # 服务端在我们写完之前就判定超限并关闭，RST 会把它已写回的 413 一起丢掉。
            print(
                f"提示：清单共 {len(entries)} 项，可能超过服务端的 max_request_bytes；"
                "连接被重置与 413 是同一种拒绝，请拆分后重试。",
                file=sys.stderr,
            )
        return 2

    print(response.body.decode("utf-8", "replace").rstrip())
    if response.status == 200:
        return 0
    print(f"错误：下发失败，HTTP {response.status}", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
