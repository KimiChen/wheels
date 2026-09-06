"""测试用的快照构造器。集中一处，避免每个用例各自捏一份形状不一样的 fixture。"""

from __future__ import annotations

import copy
import json

BASE_SNAPSHOT = {
    "schema_version": 2,
    "node_id": "node-example-01",
    "runtime_id": "0123456789abcdef0123456789abcdef",
    "started_at_unix_ms": 1787587200000,
    "sequence": 1,
    "health": {
        "counter_overflow": False,
        "sequence_overflow": False,
        "identity_limit_reached": False,
    },
    "inbounds": [
        {
            "tag": "ss-in",
            "type": "shadowsocks",
            "listen": "127.0.0.1",
            "listen_port": 8388,
            "generation": 1,
            "active": True,
            "tcp_sessions": 0,
            "udp_sessions": 0,
            "users": [
                {
                    "name": "s1",
                    "generation": 1,
                    "active": True,
                    "tcp_uplink_bytes": 0,
                    "tcp_downlink_bytes": 0,
                    "udp_uplink_bytes": 0,
                    "udp_downlink_bytes": 0,
                }
            ],
        },
        {
            "tag": "vless-entry-01",
            "type": "vless",
            "listen": "0.0.0.0",
            "listen_port": 8443,
            "generation": 1,
            "active": True,
            "tcp_sessions": 3,
            "udp_sessions": 1,
            "users": [
                {
                    "name": "u_example_01",
                    "generation": 1,
                    "active": True,
                    "tcp_uplink_bytes": 100,
                    "tcp_downlink_bytes": 200,
                    "udp_uplink_bytes": 0,
                    "udp_downlink_bytes": 0,
                },
                {
                    "name": "u_example_02",
                    "generation": 1,
                    "active": True,
                    "tcp_uplink_bytes": 0,
                    "tcp_downlink_bytes": 0,
                    "udp_uplink_bytes": 5,
                    "udp_downlink_bytes": 7,
                },
            ],
        },
    ],
}


def snapshot(**overrides) -> dict:
    value = copy.deepcopy(BASE_SNAPSHOT)
    value.update(overrides)
    return value


def encode(value: dict) -> bytes:
    return (json.dumps(value, ensure_ascii=False) + "\n").encode("utf-8")
