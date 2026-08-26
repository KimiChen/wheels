"""Reference-only settlement model used to verify exporter snapshot semantics."""

from __future__ import annotations

from dataclasses import dataclass
from hashlib import sha256
import json
from typing import Any, Literal


COUNTER_FIELDS = (
    "tcp_uplink_bytes",
    "tcp_downlink_bytes",
    "udp_uplink_bytes",
    "udp_downlink_bytes",
)
U64_MAX = (1 << 64) - 1


class SnapshotError(ValueError):
    """The snapshot violates the versioned settlement contract."""


@dataclass(frozen=True)
class IncrementBatch:
    batch_id: str
    node_id: str
    server_id: str
    server_generation: int
    identity_name: str
    identity_generation: int
    runtime_id: str
    sequence: int
    counters: dict[str, int]


class SettlementModel:
    """In-memory model; production persistence is intentionally out of scope."""

    def __init__(self, first_snapshot: Literal["baseline", "include"] = "baseline") -> None:
        if first_snapshot not in ("baseline", "include"):
            raise ValueError("first_snapshot must be baseline or include")
        self.first_snapshot = first_snapshot
        self._last_sequence: dict[tuple[str, str], int] = {}
        self._started_at_by_runtime: dict[tuple[str, str], int] = {}
        self._observed_servers: dict[tuple[str, str], set[tuple[str, int]]] = {}
        self._observed_identities: dict[
            tuple[str, str], set[tuple[str, int, str, int]]
        ] = {}
        self._baselines: dict[tuple[str, str, int, str, int, str], dict[str, int]] = {}
        self._batch_ids: set[str] = set()

    def ingest(self, snapshot: dict[str, Any]) -> list[IncrementBatch]:
        if not isinstance(snapshot, dict):
            raise SnapshotError("snapshot must be an object")
        self._validate_envelope(snapshot)
        node_id = snapshot["node_id"]
        runtime_id = snapshot["runtime_id"]
        sequence = snapshot["sequence"]
        sequence_key = (node_id, runtime_id)
        started_at = snapshot["started_at_unix_ms"]
        previous_started_at = self._started_at_by_runtime.get(sequence_key)
        if previous_started_at is not None and started_at != previous_started_at:
            raise SnapshotError("started_at_unix_ms changed within one runtime")
        previous_sequence = self._last_sequence.get(sequence_key)
        if previous_sequence is not None and sequence <= previous_sequence:
            return []

        pending_baselines: dict[tuple[str, str, int, str, int, str], dict[str, int]] = {}
        seen_baseline_keys: set[tuple[str, str, int, str, int, str]] = set()
        seen_server_keys: set[tuple[str, int]] = set()
        batches: list[IncrementBatch] = []
        for server in snapshot["servers"]:
            if not isinstance(server, dict):
                raise SnapshotError("server entry must be an object")
            server_id = self._required_string(server, "server_id")
            server_generation = self._positive_integer(server, "generation", "server generation")
            server_key = (server_id, server_generation)
            if server_key in seen_server_keys:
                raise SnapshotError("duplicate server generation in snapshot")
            seen_server_keys.add(server_key)
            if not isinstance(server.get("users"), list):
                raise SnapshotError("server users must be an array")
            for user in server["users"]:
                if not isinstance(user, dict):
                    raise SnapshotError("user entry must be an object")
                if user.get("identity_kind") != "user":
                    raise SnapshotError("unsupported identity_kind")
                name = self._required_string(user, "name")
                generation = self._positive_integer(user, "generation", "user generation")

                current = self._read_counters(user)
                baseline_key = (node_id, server_id, server_generation, name, generation, runtime_id)
                if baseline_key in seen_baseline_keys:
                    raise SnapshotError("duplicate server/user generation in snapshot")
                seen_baseline_keys.add(baseline_key)
                previous = self._baselines.get(baseline_key)
                if previous is None:
                    increments = current if self.first_snapshot == "include" else {field: 0 for field in COUNTER_FIELDS}
                else:
                    increments = {}
                    for field in COUNTER_FIELDS:
                        if current[field] < previous[field]:
                            raise SnapshotError(f"counter regressed: {server_id}/{name}/{field}")
                        increments[field] = current[field] - previous[field]

                pending_baselines[baseline_key] = current
                if any(increments.values()):
                    batch = self._make_batch(
                        node_id,
                        server_id,
                        server_generation,
                        name,
                        generation,
                        runtime_id,
                        sequence,
                        increments,
                    )
                    if batch.batch_id not in self._batch_ids:
                        batches.append(batch)

        observed_servers = self._observed_servers.get(sequence_key, set())
        if not observed_servers.issubset(seen_server_keys):
            raise SnapshotError("previously observed server lineage disappeared")
        observed_identities = self._observed_identities.get(sequence_key, set())
        seen_identity_keys = {
            (server_id, server_generation, name, generation)
            for _, server_id, server_generation, name, generation, _ in seen_baseline_keys
        }
        if not observed_identities.issubset(seen_identity_keys):
            raise SnapshotError("previously observed identity lineage disappeared")

        self._baselines.update(pending_baselines)
        self._last_sequence[sequence_key] = sequence
        self._started_at_by_runtime[sequence_key] = started_at
        self._observed_servers[sequence_key] = observed_servers | seen_server_keys
        self._observed_identities[sequence_key] = observed_identities | seen_identity_keys
        self._batch_ids.update(batch.batch_id for batch in batches)
        return batches

    @staticmethod
    def _validate_envelope(snapshot: dict[str, Any]) -> None:
        if type(snapshot.get("schema_version")) is not int or snapshot["schema_version"] != 1:
            raise SnapshotError("unsupported schema_version")
        SettlementModel._required_string(snapshot, "node_id")
        SettlementModel._required_string(snapshot, "runtime_id")
        started_at = snapshot.get("started_at_unix_ms")
        if not isinstance(started_at, int) or isinstance(started_at, bool) or not 0 <= started_at <= U64_MAX:
            raise SnapshotError("started_at_unix_ms must be a u64 integer")
        SettlementModel._positive_integer(snapshot, "sequence", "sequence")
        if not isinstance(snapshot.get("servers"), list):
            raise SnapshotError("servers must be an array")
        health = snapshot.get("health")
        if not isinstance(health, dict) or health != {
            "counter_overflow": False,
            "sequence_overflow": False,
        }:
            raise SnapshotError("unhealthy, missing, or unknown health state")

    @staticmethod
    def _positive_integer(value: dict[str, Any], field: str, label: str) -> int:
        field_value = value.get(field)
        if (
            not isinstance(field_value, int)
            or isinstance(field_value, bool)
            or not 1 <= field_value <= U64_MAX
        ):
            raise SnapshotError(f"{label} must be a positive integer")
        return field_value

    @staticmethod
    def _required_string(value: dict[str, Any], field: str) -> str:
        field_value = value.get(field)
        if not isinstance(field_value, str) or not field_value:
            raise SnapshotError(f"{field} must be a non-empty string")
        return field_value

    @staticmethod
    def _read_counters(user: dict[str, Any]) -> dict[str, int]:
        counters: dict[str, int] = {}
        for field in COUNTER_FIELDS:
            value = user.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0 or value > U64_MAX:
                raise SnapshotError(f"{field} must be a u64 integer")
            counters[field] = value
        return counters

    @staticmethod
    def _make_batch(
        node_id: str,
        server_id: str,
        server_generation: int,
        name: str,
        generation: int,
        runtime_id: str,
        sequence: int,
        counters: dict[str, int],
    ) -> IncrementBatch:
        identity = {
            "node_id": node_id,
            "server_id": server_id,
            "server_generation": server_generation,
            "identity_name": name,
            "identity_generation": generation,
            "runtime_id": runtime_id,
            "sequence": sequence,
            "counters": counters,
        }
        batch_id = sha256(json.dumps(identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        return IncrementBatch(batch_id=batch_id, **identity)
