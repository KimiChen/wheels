#!/usr/bin/env python3
"""参考 collector：取快照 → v2 校验 → 差分 → 幂等落地本地账本。

带 --quota-socket 时另外承担 §5.4 的下发职责：按自己累计的「本周期已用」算出剩余额度，
每轮重推一份全量表。

范围**不含** outbox、mTLS 与重试——那些属于下游控制面。
这里只把 §5.1 与 §5.4 的契约跑通，并且让「本次采集失败」与「本次没有流量」在账本里可区分。
"""

from __future__ import annotations

import argparse
import datetime
import json
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))

import http_unix  # noqa: E402
from settlement_model import (  # noqa: E402
    COUNTER_FIELDS,
    Collector,
    SettlementFailure,
    SnapshotRejected,
    parse_snapshot,
)

LEDGER_FILE = "ledger.jsonl"
STATE_FILE = "state.json"

# not_accepted 的四种成因。判据必须按成因分桶：「未知 runtime」与「序号未前进」
# 是两条彼此独立的判据，数进同一个计数等于两条都没测。
#
# 键是 settlement_model 给出的 reason 的稳定前缀。把分类放在这里、而不是让
# settlement_model 多返回一个字段，是因为那份文件同时锁定安装在控制器与四个节点上，
# 为一个只有长跑报告会读的字段去动那五份拷贝不划算。
# 代价是本表与 reason 的文案耦合——由 test_every_rejection_reason_maps_to_a_code
# 钉住：改了文案而不改这里，那个测试会红。
REJECTION_CODES = (
    ("started_at_unix_ms 变化", "unknown_runtime"),
    ("health 有位为真", "unhealthy"),
    ("sequence ", "stale_sequence"),  # 理由里嵌了序号，只能前缀匹配
    ("批次已入账", "duplicate_batch"),
)


def rejection_code(reason: str) -> str:
    """把 not_accepted 的文字理由归入一个稳定的桶名。

    认不出来的一律归入 unclassified，**且 unclassified 计入失败**——
    「出现了没见过的拒绝理由」与「有问题」在判据里必须同义。否则新增一条拒绝路径
    会让判据悄悄地少测一项而依然全绿，这正是旧报告的病根。
    """
    for prefix, code in REJECTION_CODES:
        if reason.startswith(prefix):
            return code
    return "unclassified"


def tripped_health(snapshot: dict) -> list[str]:
    """记下入账那一刻为真的 health 位。

    只有 audit_dropped 会出现在这里：health_ok 只闸断三个**计费**位，
    审计丢记录按设计不停止计费（settlement_model.py:191）。所以
    「有没有对不健康的快照计过费」不是一句结构保证就能答完的——
    三个计费位是结构保证，audit_dropped 不是，它必须被观测。
    旧账本不记这一项，于是那条判据只能写成字面量 0。
    """
    return sorted(key for key, value in snapshot["health"].items() if value)


class Ledger:
    """账本是一份 JSONL：每一次采集都留痕，包括失败。

    失败也写一行，是为了让长跑结束时的四项判据可判定：
    只记成功的话，「七天没有负增量」与「七天一次都没采到」看起来一模一样。
    """

    def __init__(self, directory: Path) -> None:
        self.directory = directory
        self.directory.mkdir(parents=True, exist_ok=True)
        self.path = directory / LEDGER_FILE
        self.state_path = directory / STATE_FILE

    def append(self, record: dict) -> None:
        line = json.dumps(record, ensure_ascii=False, sort_keys=True)
        with self.path.open("a", encoding="utf-8") as handle:
            handle.write(line + "\n")

    def load_state(self) -> dict:
        if not self.state_path.is_file():
            return {}
        return json.loads(self.state_path.read_text(encoding="utf-8"))

    def save_state(self, state: dict) -> None:
        temporary = self.state_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(state, ensure_ascii=False, sort_keys=True), encoding="utf-8")
        temporary.replace(self.state_path)


class QuotaPlan:
    """每个计费身份的额度。

    一个全局数字撑不起真实部署：不同用户的套餐不同，而改某一个人的额度不应该动到别人。
    计划文件与 systemd 单元解耦——改额度不需要编辑 unit、不需要 daemon-reload。
    """

    def __init__(self, default_bytes: int, overrides: dict[str, int] | None = None) -> None:
        if default_bytes < 0:
            raise ValueError("default_bytes 不能为负")
        self.default_bytes = default_bytes
        self.overrides = dict(overrides or {})

    @classmethod
    def load(cls, path: Path) -> "QuotaPlan":
        payload = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(payload, dict):
            raise ValueError("额度计划顶层必须是对象")
        allowed = {"schema_version", "default_bytes", "overrides"}
        unknown = set(payload) - allowed
        if unknown:
            # 与配置面同一纪律：未知字段硬失败。写错一个键名就静默按默认额度跑，
            # 是「看起来生效了、实际没生效」的典型。
            raise ValueError(f"额度计划含未知字段：{sorted(unknown)}")
        if payload.get("schema_version") != 1:
            raise ValueError("额度计划 schema_version 必须是 1")
        default_bytes = payload.get("default_bytes", 0)
        if type(default_bytes) is not int or isinstance(default_bytes, bool) or default_bytes < 0:
            raise ValueError("default_bytes 必须是非负整数")
        overrides = payload.get("overrides", {})
        if not isinstance(overrides, dict):
            raise ValueError("overrides 必须是对象")
        for key, value in overrides.items():
            if "/" not in key:
                raise ValueError(f"overrides 的键必须是 <inbound_tag>/<name>：{key}")
            if type(value) is not int or isinstance(value, bool) or value < 0:
                raise ValueError(f"overrides[{key}] 必须是非负整数")
        return cls(default_bytes, overrides)

    def quota_for(self, inbound_tag: str, name: str) -> int:
        return self.overrides.get(f"{inbound_tag}/{name}", self.default_bytes)


def push_quota(
    quota_socket: str,
    snapshot: dict,
    ledger: Ledger,
    collector: Collector,
    plan: QuotaPlan,
    timeout: float,
) -> int:
    """按 §5.4 重算并下发一份**全量**剩余额度表。

    三条义务在这里落地：
      1. 「本周期已用」取自采集端自己的累计（`collector.usage`），不是快照绝对值；
      2. 每次采集后都重推——进程侧的额度是纯内存的，重启即消失；
      3. 顺序是先入账、再算额度、再推送，因此本函数只在 ingest 成功之后被调用。

    未出现在表中的身份会被服务端视为**无限额度**，所以这里必须列出快照里的每一个身份。
    """
    entries = []
    for inbound in snapshot["inbounds"]:
        for user in inbound["users"]:
            entries.append(
                {
                    "inbound_tag": inbound["tag"],
                    "name": user["name"],
                    "remaining_bytes": collector.remaining_for(
                        snapshot["node_id"],
                        inbound["tag"],
                        user["name"],
                        plan.quota_for(inbound["tag"], user["name"]),
                    ),
                }
            )
    epoch = collector.next_quota_epoch()
    # epoch 在推送**之前**落盘。反过来的话，推送成功但存盘前崩溃会让 epoch 回退，
    # 下一轮复用同一个 epoch 被服务端以 409 拒绝，白白浪费一个下发周期。
    # epoch 只要求单调递增、不要求连续，因此「崩在推送前、白跳一个号」是无害的。
    ledger.save_state(collector.export_state())
    body = json.dumps(
        {
            "schema_version": 3,
            "node_id": snapshot["node_id"],
            "runtime_id": snapshot["runtime_id"],
            "epoch": epoch,
            "entries": entries,
        },
        ensure_ascii=False,
    ).encode("utf-8")
    now = int(time.time() * 1000)
    try:
        response = http_unix.request(quota_socket, "PUT", "/v3/quota", body=body, timeout=timeout)
    except http_unix.HTTPUnixError as error:
        ledger.append({"ts": now, "result": "quota_transport_error", "detail": str(error)})
        return 1
    exhausted = [entry["name"] for entry in entries if entry["remaining_bytes"] == 0]
    record = {
        "ts": now,
        "result": "quota_pushed" if response.status == 200 else "quota_rejected",
        "status": response.status,
        "epoch": epoch,
        "entries": len(entries),
        "exhausted": exhausted,
    }
    if response.status != 200:
        record["detail"] = response.body.decode("utf-8", "replace").strip()
    ledger.append(record)
    return 0 if response.status == 200 else 1


def collect_once(
    socket_path: str,
    ledger: Ledger,
    collector: Collector,
    timeout: float,
    quota_socket: str | None = None,
    plan: QuotaPlan | None = None,
) -> int:
    now = int(time.time() * 1000)
    try:
        response = http_unix.request(socket_path, "GET", "/v3/snapshot", timeout=timeout)
    except http_unix.HTTPUnixError as error:
        ledger.append({"ts": now, "result": "transport_error", "detail": str(error)})
        return 1
    if response.status == 429:
        # 429 与连接被直接关闭视为可重试且不得入账。
        ledger.append({"ts": now, "result": "retryable", "status": 429})
        return 0
    if response.status != 200:
        ledger.append({"ts": now, "result": "rejected", "status": response.status})
        return 1
    try:
        snapshot = parse_snapshot(response.body)
    except SnapshotRejected as error:
        ledger.append({"ts": now, "result": "schema_rejected", "detail": str(error)})
        return 1
    try:
        settlement = collector.ingest(snapshot)
    except SettlementFailure as error:
        # 失败关闭：不得猜测并继续收费。
        ledger.append({"ts": now, "result": "failed_closed", "detail": str(error)})
        return 2
    if not settlement.accepted:
        # 即使本轮没入账也要落状态：ingest 可能已经登记了新的 runtime（例如
        # started_at 变化那一支），丢掉它会让下一轮又从头开始。同样是状态在前。
        ledger.save_state(collector.export_state())
        ledger.append(
            {
                "ts": now,
                "result": "not_accepted",
                "code": rejection_code(settlement.reason),
                "detail": settlement.reason,
                "sequence": snapshot["sequence"],
            }
        )
        return 0
    total = {name: 0 for name in COUNTER_FIELDS}
    entries = []
    for key, delta in sorted(settlement.deltas.items()):
        for name in COUNTER_FIELDS:
            total[name] += delta[name]
        entries.append({"key": list(key), "delta": delta})
    # 写序是「先原子落状态，再追加账本」，不能反过来。
    #
    # 反过来（先写账本、再存状态）时，两步之间崩溃会让下游看到已计费的一批，
    # 而采集端的基线还停在上一个 sequence——重启后下一次采集会把这段范围重新算成一批发出去，
    # 同一批字节被计费两次。batch id 挡不住它：重放出来的批次 sequence 与增量都不同，
    # batch id 自然也不同。
    #
    # 现在这个顺序把失败窗口换成了另一种：状态已推进但账本少一行，
    # 即那一批的字节已计入基线与用量，下游却拿不到对应的批次记录——**漏记一批，不是重复计费**。
    # 这是与 §5.1 的 baseline / include 同一性质的显式取舍：宁可漏记，不可重复。
    ledger.save_state(collector.export_state())
    ledger.append(
        {
            "ts": now,
            "result": "accepted",
            "batch": settlement.batch,
            "sequence": snapshot["sequence"],
            "node_id": snapshot["node_id"],
            "runtime_id": snapshot["runtime_id"],
            "first_snapshot": settlement.first_snapshot,
            "health": tripped_health(snapshot),
            "total": total,
            "entries": entries,
        }
    )
    if quota_socket and plan is not None:
        # §5.4 第 3 条：先持久化入账，再算额度，再推送。
        return push_quota(quota_socket, snapshot, ledger, collector, plan, timeout)
    return 0


def moment(text: str) -> int:
    """把命令行上的时刻解析成毫秒时间戳。

    不带时区的写法一律按**本机本地时区**解释：账本里的 ts 是 time.time()，
    而报告是人按当地钟点划窗口的，这里不做转换才是会错的那一种。
    """
    if text.isdigit():
        return int(text)
    try:
        parsed = datetime.datetime.fromisoformat(text)
    except ValueError as error:
        raise argparse.ArgumentTypeError(f"无法解析时刻 {text!r}：{error}") from error
    if parsed.tzinfo is None:
        parsed = parsed.astimezone()
    return int(parsed.timestamp() * 1000)


def report(ledger: Ledger, since: int | None = None, until: int | None = None) -> int:
    """输出长跑判据（README §7 里程碑 5）。

    2026-09 之前这个函数的四项判据有三项是假的，而且三项的假法各不相同：

    * unknown_runtime 数的是 schema_rejected。未知 runtime 走的是 not_accepted，
      永远进不了那个桶——这一项恒为 0，与实际发生了多少次无关。
    * duplicate_sequence 只从**已入账**的行里推导。而 ingest 早就拒绝了不前进的
      sequence，于是能进入推导的行按定义不可能重号——同样恒为 0。
    * unhealthy_accepted 是个字面量 0，一次都没查过。

    退出码只看这三项，所以 transport_error 与 not_accepted 再多也返回 0，
    连一份空账本都返回 0。这里逐条改掉：按成因分桶、把传输与拒绝计入失败、
    并要求 accepted > 0（否则「七天全绿」与「七天一次没采到」仍然同形）。

    since/until 是毫秒时间戳，闭区间，用于对已归档的账本按窗口重算。
    """
    counters: dict[str, int] = {}
    codes: dict[str, int] = {}
    sequences: dict[str, set[int]] = {}
    duplicate_sequences = 0
    billing_unhealthy = 0
    audit_dropped = 0
    health_not_recorded = 0
    rows = 0
    first_ts: int | None = None
    last_ts: int | None = None
    if ledger.path.is_file():
        for line in ledger.path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            record = json.loads(line)
            ts = record.get("ts")
            if since is not None and (ts is None or ts < since):
                continue
            if until is not None and (ts is None or ts > until):
                continue
            rows += 1
            if ts is not None:
                first_ts = ts if first_ts is None else min(first_ts, ts)
                last_ts = ts if last_ts is None else max(last_ts, ts)
            result = record.get("result", "")
            counters[result] = counters.get(result, 0) + 1
            if result == "not_accepted":
                # 新账本自带 code；2026-09 之前的账本只有中文 detail，回推同一张表。
                # 两条路径必须落到同一组桶名，否则「重算历史」与「监控当期」
                # 就不是同一个判据。
                code = record.get("code") or rejection_code(record.get("detail", ""))
                codes[code] = codes.get(code, 0) + 1
            elif result == "accepted":
                bucket = sequences.setdefault(record["runtime_id"], set())
                if record["sequence"] in bucket:
                    duplicate_sequences += 1
                bucket.add(record["sequence"])
                health = record.get("health")
                if health is None:
                    health_not_recorded += 1
                else:
                    if "audit_dropped" in health:
                        audit_dropped += 1
                    if [key for key in health if key != "audit_dropped"]:
                        billing_unhealthy += 1

    verdict = {
        # —— 四项判据 ——
        "negative_delta": counters.get("failed_closed", 0),
        "unknown_runtime": codes.get("unknown_runtime", 0),
        # 两个来源都算：not_accepted 里被挡下的，以及万一真有两行已入账撞号的。
        # 后者若非零说明 ingest 的互斥出了问题，比前者严重得多。
        "duplicate_sequence": codes.get("stale_sequence", 0) + duplicate_sequences,
        "billing_unhealthy_accepted": billing_unhealthy,
        # —— 判据之外，但必须与判据一起看 ——
        "unhealthy_rejected": codes.get("unhealthy", 0),
        # 按设计允许：审计丢记录不停止计费，但它是证据链缺口，要看得见。
        "audit_dropped_accepted": audit_dropped,
        "health_not_recorded": health_not_recorded,
        "unclassified_rejection": codes.get("unclassified", 0),
        "duplicate_batch": codes.get("duplicate_batch", 0),
        "accepted": counters.get("accepted", 0),
        "runtimes": len(sequences),
    }

    # 逐条列出不合格项，而不是只给一个退出码：一个说不出自己为什么红的判据，
    # 下一次也就只会被人按「大概是环境问题」处理掉。
    checks = (
        ("negative_delta", verdict["negative_delta"]),
        ("unknown_runtime", verdict["unknown_runtime"]),
        ("duplicate_sequence", verdict["duplicate_sequence"]),
        ("billing_unhealthy_accepted", verdict["billing_unhealthy_accepted"]),
        ("unhealthy_rejected", verdict["unhealthy_rejected"]),
        ("unclassified_rejection", verdict["unclassified_rejection"]),
        ("transport_error", counters.get("transport_error", 0)),
        ("quota_transport_error", counters.get("quota_transport_error", 0)),
        ("quota_rejected", counters.get("quota_rejected", 0)),
        ("schema_rejected", counters.get("schema_rejected", 0)),
        ("rejected", counters.get("rejected", 0)),
    )
    failing = [name for name, value in checks if value]
    if not verdict["accepted"]:
        # 空账本、或窗口切歪了。旧实现对这两种情况都返回 0。
        failing.append("no_accepted_rows")

    print(
        json.dumps(
            {
                "window": {"since": since, "until": until,
                           "first_ts": first_ts, "last_ts": last_ts, "rows": rows},
                "counters": counters,
                "verdict": verdict,
                "failing": failing,
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 1 if failing else 0


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="sing-box-plus 参考 collector")
    parser.add_argument("--socket", help="快照 socket 路径")
    parser.add_argument("--ledger", required=True, type=Path, help="本地账本目录")
    parser.add_argument(
        "--first-snapshot",
        choices=("baseline", "include"),
        default="baseline",
        help="首次看到新 runtime_id 时的策略，必须显式选择",
    )
    parser.add_argument("--quota-socket", help="启用 §5.4 的额度下发：每次采集后重推全量表")
    parser.add_argument(
        "--quota-bytes",
        type=int,
        default=0,
        help="所有身份统一的周期额度（四向之和，字节）；与 --quota-plan 二选一",
    )
    parser.add_argument(
        "--quota-plan",
        type=Path,
        help="额度计划 JSON：{schema_version:1, default_bytes, overrides:{\"tag/name\": bytes}}",
    )
    parser.add_argument("--timeout", type=float, default=10.0)
    parser.add_argument("--report", action="store_true", help="只输出长跑判据，不采集")
    parser.add_argument(
        "--since",
        type=moment,
        help="只统计此刻之后的行：本地时间 YYYY-MM-DD[ HH:MM[:SS]]，或直接给毫秒时间戳",
    )
    parser.add_argument("--until", type=moment, help="只统计此刻之前的行（闭区间），格式同 --since")
    parser.add_argument(
        "--reset-period",
        action="store_true",
        help="开一个新的计费周期：清零本周期已用，保留基线与 runtime 状态",
    )
    args = parser.parse_args(argv)

    ledger = Ledger(args.ledger)
    if args.report:
        return report(ledger, since=args.since, until=args.until)
    if args.reset_period:
        # 只清「本周期已用」，**不动基线**：基线清掉的话，下一次采集会把快照里的
        # 全部历史累计值当成一次巨额增量重新入账。
        collector = Collector(first_snapshot=args.first_snapshot)
        collector.restore_state(ledger.load_state())
        cleared = sum(collector.usage.values())
        collector.usage.clear()
        ledger.save_state(collector.export_state())
        ledger.append({"ts": int(time.time() * 1000), "result": "period_reset", "cleared_bytes": cleared})
        print(f"已开始新计费周期：清零 {cleared:,} 字节的已用量（基线与 runtime 状态保留）")
        return 0
    if not args.socket:
        parser.error("采集模式需要 --socket")
    collector = Collector(first_snapshot=args.first_snapshot)
    # 必须先恢复上一轮的基线，否则「每次采集起一个进程」的用法会把每一轮都当成首快照，
    # 于是永远只建基线、永不入账——而四项判据依然全绿。
    try:
        collector.restore_state(ledger.load_state())
    except SnapshotRejected as error:
        print(f"错误：无法沿用已有采集状态：{error}", file=sys.stderr)
        return 2
    plan = None
    if args.quota_plan is not None and args.quota_bytes > 0:
        parser.error("--quota-plan 与 --quota-bytes 互斥")
    if args.quota_plan is not None:
        try:
            plan = QuotaPlan.load(args.quota_plan)
        except (OSError, ValueError, json.JSONDecodeError) as error:
            print(f"错误：额度计划不可用：{error}", file=sys.stderr)
            return 2
    elif args.quota_bytes > 0:
        plan = QuotaPlan(args.quota_bytes)
    if bool(args.quota_socket) != (plan is not None):
        parser.error("--quota-socket 必须与 --quota-bytes 或 --quota-plan 同时给出")
    return collect_once(args.socket, ledger, collector, args.timeout, args.quota_socket, plan)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
