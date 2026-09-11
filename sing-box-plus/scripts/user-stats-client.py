#!/usr/bin/env python3
"""带 v2 schema 与健康校验的快照读取客户端。

运维用：确认节点在产出可入账的快照。它不是采集器——不做差分、不落账本，
那是 tests/reference_collector.py 与下游控制面的职责。
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "tests"))

import http_unix  # noqa: E402
from settlement_model import SnapshotRejected, health_ok, parse_snapshot  # noqa: E402

DEFAULT_SOCKET = "/run/sing-box-plus/user-stats.sock"


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="读取并校验 sing-box-plus 快照")
    parser.add_argument("--socket", default=DEFAULT_SOCKET)
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--json", action="store_true", help="原样输出快照 JSON")
    parser.add_argument("--healthz", action="store_true", help="改为请求 /healthz")
    args = parser.parse_args(argv)

    target = "/healthz" if args.healthz else "/v2/snapshot"
    try:
        response = http_unix.request(args.socket, "GET", target, timeout=args.timeout)
    except http_unix.HTTPUnixError as error:
        print(f"错误：{error}", file=sys.stderr)
        return 2
    if args.healthz:
        print(json.dumps(response.json(), ensure_ascii=False))
        return 0 if response.status == 200 else 1
    if response.status != 200:
        print(f"错误：快照返回 {response.status}：{response.body.decode('utf-8', 'replace')}", file=sys.stderr)
        return 2
    try:
        snapshot = parse_snapshot(response.body)
    except SnapshotRejected as error:
        print(f"错误：快照不符合 v2 契约：{error}", file=sys.stderr)
        return 2
    if args.json:
        print(json.dumps(snapshot, ensure_ascii=False, indent=2, sort_keys=True))
        return 0

    print(f"node_id={snapshot['node_id']} runtime_id={snapshot['runtime_id']} sequence={snapshot['sequence']}")
    flags = [name for name, value in sorted(snapshot["health"].items()) if value]
    print("health=" + ("ok" if not flags else "UNHEALTHY:" + ",".join(flags)))
    for inbound in snapshot["inbounds"]:
        print(
            f"  [{inbound['tag']}] type={inbound['type']} "
            f"listen={inbound['listen']}:{inbound['listen_port']} "
            f"active={inbound['active']} tcp_sessions={inbound['tcp_sessions']} "
            f"udp_sessions={inbound['udp_sessions']}"
        )
        for user in inbound["users"]:
            print(
                f"    {user['name']:<24} active={str(user['active']):<5} "
                f"tcp={user['tcp_uplink_bytes']}/{user['tcp_downlink_bytes']} "
                f"udp={user['udp_uplink_bytes']}/{user['udp_downlink_bytes']}"
            )
    # 健康位为真时快照不可入账，退出码必须能被编排工具区分。
    return 0 if health_ok(snapshot) else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
