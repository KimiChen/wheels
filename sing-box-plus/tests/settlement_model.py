#!/usr/bin/env python3
"""v2 快照的校验、差分与幂等结算模型（README §4.5、§5.1）。

与 `shadowsocks-rust-plus` 的结算语义同构，但**不能原样复用它的校验器**：
那三个校验器硬校验 schema_version == 1 与 identity_kind，并对 health 做整字典相等比较，
三处都会整份拒绝本项目的快照。
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from typing import Any, Iterable

SCHEMA_VERSION = 2

SNAPSHOT_KEYS = {
    "schema_version",
    "node_id",
    "runtime_id",
    "started_at_unix_ms",
    "sequence",
    "health",
    "inbounds",
}
HEALTH_KEYS = {"counter_overflow", "sequence_overflow", "identity_limit_reached"}
INBOUND_KEYS = {
    "tag",
    "type",
    "listen",
    "listen_port",
    "generation",
    "active",
    "tcp_sessions",
    "udp_sessions",
    "users",
}
USER_KEYS = {
    "name",
    "generation",
    "active",
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
}
COUNTER_FIELDS = (
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
)
MAX_U64 = (1 << 64) - 1


class SnapshotRejected(Exception):
    """快照不可入账。调用方必须丢弃整份快照且不推进基线。"""


class SettlementFailure(Exception):
    """差分失败关闭。不得猜测并继续收费。"""


def _strict_object(pairs: Iterable[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise SnapshotRejected(f"重复字段：{key}")
        result[key] = value
    return result


def _require_u64(container: dict[str, Any], key: str) -> int:
    value = container[key]
    # bool 是 int 的子类：不排除它，true 会被当成 1 静默入账。
    if type(value) is not int or isinstance(value, bool):
        raise SnapshotRejected(f"{key} 必须是 JSON 非负整数，实际 {type(value).__name__}")
    if value < 0 or value > MAX_U64:
        raise SnapshotRejected(f"{key} 超出 u64 范围：{value}")
    return value


def _require_bool(container: dict[str, Any], key: str) -> bool:
    value = container[key]
    if type(value) is not bool:
        raise SnapshotRejected(f"{key} 必须是布尔值")
    return value


def _require_str(container: dict[str, Any], key: str) -> str:
    value = container[key]
    if type(value) is not str or not value:
        raise SnapshotRejected(f"{key} 必须是非空字符串")
    if len(value.encode("utf-8")) > 128:
        raise SnapshotRejected(f"{key} 超过 128 字节")
    for offset, char in enumerate(value):
        if not (0x20 < ord(char) < 0x7F):
            raise SnapshotRejected(f"{key} 含非 ASCII 可显示字符（偏移 {offset}）")
    return value


def parse_snapshot(payload: bytes) -> dict[str, Any]:
    """严格解析并校验一份快照。任何一处不符即整份拒绝。

    u64 字段禁止经 IEEE754 double 解析：Python 的 int 是任意精度，
    但必须显式拒绝 float/bool/str，否则 `1e19` 这类值会被当成合法计数。
    """
    try:
        text = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise SnapshotRejected(f"快照不是合法 UTF-8：{error}") from error
    try:
        snapshot = json.loads(text, object_pairs_hook=_strict_object)
    except json.JSONDecodeError as error:
        raise SnapshotRejected(f"快照不是合法 JSON：{error}") from error
    if not isinstance(snapshot, dict):
        raise SnapshotRejected("快照顶层必须是对象")
    if set(snapshot) != SNAPSHOT_KEYS:
        missing = SNAPSHOT_KEYS - set(snapshot)
        extra = set(snapshot) - SNAPSHOT_KEYS
        raise SnapshotRejected(f"快照顶层键集合错误：缺少 {sorted(missing)}，多余 {sorted(extra)}")
    if snapshot["schema_version"] != SCHEMA_VERSION or isinstance(snapshot["schema_version"], bool):
        raise SnapshotRejected(f"schema_version 必须是 {SCHEMA_VERSION}")

    _require_str(snapshot, "node_id")
    runtime_id = snapshot["runtime_id"]
    if type(runtime_id) is not str or len(runtime_id) != 32 or any(
        char not in "0123456789abcdef" for char in runtime_id
    ):
        raise SnapshotRejected("runtime_id 必须是 32 位小写 hex")
    started = _require_u64(snapshot, "started_at_unix_ms")
    sequence = _require_u64(snapshot, "sequence")
    if started == 0 or sequence == 0:
        raise SnapshotRejected("started_at_unix_ms 与 sequence 必须是正整数")

    health = snapshot["health"]
    # health 是闭集：出现额外键即整份拒绝；新增 health 键必须同时提升 schema_version。
    if not isinstance(health, dict) or set(health) != HEALTH_KEYS:
        raise SnapshotRejected("health 必须是恰好三键的闭集")
    for key in HEALTH_KEYS:
        _require_bool(health, key)

    inbounds = snapshot["inbounds"]
    if not isinstance(inbounds, list):
        raise SnapshotRejected("inbounds 必须是数组")
    previous_key: tuple[str, int] | None = None
    for inbound in inbounds:
        if not isinstance(inbound, dict) or set(inbound) != INBOUND_KEYS:
            raise SnapshotRejected("inbounds[] 键集合错误")
        tag = _require_str(inbound, "tag")
        _require_str(inbound, "type")
        listen = inbound["listen"]
        if type(listen) is not str or ":" in listen and "]" not in listen and listen.count(":") == 1:
            raise SnapshotRejected("listen 必须是纯 host，不得是 host:port 合成串")
        port = _require_u64(inbound, "listen_port")
        if not 1 <= port <= 65535:
            raise SnapshotRejected(f"listen_port 越界：{port}")
        generation = _require_u64(inbound, "generation")
        _require_bool(inbound, "active")
        for gauge in ("tcp_sessions", "udp_sessions"):
            value = inbound[gauge]
            if type(value) is not int or isinstance(value, bool) or value < 0:
                raise SnapshotRejected(f"{gauge} 必须是非负整数")
        current_key = (tag, generation)
        if previous_key is not None and current_key <= previous_key:
            raise SnapshotRejected("inbounds 未按 (tag, generation) 的 ASCII 字节升序排列")
        previous_key = current_key

        users = inbound["users"]
        if not isinstance(users, list):
            raise SnapshotRejected("users 必须是数组")
        previous_user: tuple[str, int] | None = None
        for user in users:
            if not isinstance(user, dict) or set(user) != USER_KEYS:
                raise SnapshotRejected("users[] 键集合错误")
            name = _require_str(user, "name")
            user_generation = _require_u64(user, "generation")
            _require_bool(user, "active")
            for counter in COUNTER_FIELDS:
                _require_u64(user, counter)
            user_key = (name, user_generation)
            if previous_user is not None and user_key <= previous_user:
                raise SnapshotRejected("users 未按 (name, generation) 的 ASCII 字节升序排列")
            previous_user = user_key
    return snapshot


def health_ok(snapshot: dict[str, Any]) -> bool:
    return not any(snapshot["health"].values())


def baseline_key(snapshot: dict[str, Any], inbound: dict[str, Any], user: dict[str, Any]) -> tuple:
    """§5.1 的完整基线键。

    generation 恒为 1，但不得从采集键中省略，以保持 schema 与未来兼容性。
    """
    return (
        snapshot["node_id"],
        inbound["tag"],
        inbound["generation"],
        user["name"],
        user["generation"],
        snapshot["runtime_id"],
    )


def batch_id(snapshot: dict[str, Any], deltas: dict[tuple, dict[str, int]]) -> str:
    """幂等批次 ID 必须包含快照 sequence 与本次增量。

    只用 sequence 做 ID，会让「同一序号重投但增量不同」的实现错误变成静默重复入账。
    """
    digest = hashlib.sha256()
    digest.update(f"{snapshot['node_id']}\0{snapshot['runtime_id']}\0{snapshot['sequence']}\0".encode())
    for key in sorted(deltas):
        digest.update("\x1f".join(str(part) for part in key).encode("utf-8"))
        digest.update(b"\0")
        for field_name in COUNTER_FIELDS:
            digest.update(f"{field_name}={deltas[key][field_name]}\0".encode())
    return digest.hexdigest()


@dataclass
class RuntimeState:
    node_id: str
    runtime_id: str
    started_at_unix_ms: int
    last_sequence: int = 0
    baselines: dict[tuple, dict[str, int]] = field(default_factory=dict)


@dataclass
class Settlement:
    """差分结果。accepted 为假时调用方不得入账，也不得推进基线。"""

    accepted: bool
    reason: str = ""
    batch: str = ""
    deltas: dict[tuple, dict[str, int]] = field(default_factory=dict)
    first_snapshot: bool = False


class Collector:
    """按 §5.1 实现差分与幂等的最小结算器。

    first_snapshot 策略必须显式选择，不留隐式行为：
      * baseline —— 只建基线，降低重复风险；
      * include  —— 首次累计全部计入，降低漏记风险。
    """

    def __init__(self, first_snapshot: str = "baseline") -> None:
        if first_snapshot not in ("baseline", "include"):
            raise ValueError("first_snapshot 必须显式为 baseline 或 include")
        self.first_snapshot = first_snapshot
        self.runtimes: dict[tuple[str, str], RuntimeState] = {}
        self.applied_batches: set[str] = set()
        self.stats = {
            "accepted": 0,
            "rejected_health": 0,
            "rejected_stale_sequence": 0,
            "rejected_unknown_runtime": 0,
            "failed_regression": 0,
            "duplicate_batch": 0,
        }

    def ingest(self, snapshot: dict[str, Any]) -> Settlement:
        key = (snapshot["node_id"], snapshot["runtime_id"])
        state = self.runtimes.get(key)
        first = state is None
        if first:
            state = RuntimeState(
                node_id=snapshot["node_id"],
                runtime_id=snapshot["runtime_id"],
                started_at_unix_ms=snapshot["started_at_unix_ms"],
            )
            self.runtimes[key] = state
        elif state.started_at_unix_ms != snapshot["started_at_unix_ms"]:
            # 同一 (node_id, runtime_id) 内 started_at_unix_ms 必须恒定，变化即视为未知 runtime。
            self.stats["rejected_unknown_runtime"] += 1
            return Settlement(False, "started_at_unix_ms 变化：视为未知 runtime")

        if not health_ok(snapshot):
            self.stats["rejected_health"] += 1
            return Settlement(False, "health 有位为真：拒绝入账")
        if snapshot["sequence"] <= state.last_sequence:
            # 重复或乱序响应：丢弃整份快照且不推进基线。
            self.stats["rejected_stale_sequence"] += 1
            return Settlement(False, f"sequence {snapshot['sequence']} 未前进")

        deltas: dict[tuple, dict[str, int]] = {}
        pending: dict[tuple, dict[str, int]] = {}
        for inbound in snapshot["inbounds"]:
            for user in inbound["users"]:
                key3 = baseline_key(snapshot, inbound, user)
                current = {name: user[name] for name in COUNTER_FIELDS}
                pending[key3] = current
                previous = state.baselines.get(key3)
                if previous is None:
                    if first and self.first_snapshot == "baseline":
                        continue
                    previous = {name: 0 for name in COUNTER_FIELDS}
                delta = {}
                for name in COUNTER_FIELDS:
                    difference = current[name] - previous[name]
                    if difference < 0:
                        # 单调性只适用于四个 *_bytes 与 sequence；会话数是瞬时 gauge。
                        self.stats["failed_regression"] += 1
                        raise SettlementFailure(
                            f"四向累计值倒退：{key3} 的 {name} 从 {previous[name]} 降到 {current[name]}"
                        )
                    delta[name] = difference
                if any(delta.values()):
                    deltas[key3] = delta

        batch = batch_id(snapshot, deltas)
        if batch in self.applied_batches:
            self.stats["duplicate_batch"] += 1
            return Settlement(False, "批次已入账（幂等丢弃）", batch=batch)

        # active=false 的已观察 lineage 仍会出现在后续快照中，采集端必须继续保留其基线，
        # 因此这里是 update 而不是整表替换。
        state.baselines.update(pending)
        state.last_sequence = snapshot["sequence"]
        self.applied_batches.add(batch)
        self.stats["accepted"] += 1
        return Settlement(True, batch=batch, deltas=deltas, first_snapshot=first)
