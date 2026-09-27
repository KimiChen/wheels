#!/usr/bin/env python3
"""Read-only, same-host Linux collector/API acceptance; emit only sanitized JSON."""
import argparse
import datetime
import ipaddress
import json
import math
import os
from pathlib import Path
import platform
import re
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


SKIP_FS = set("tmpfs devtmpfs proc sysfs cgroup cgroup2 devpts mqueue hugetlbfs debugfs tracefs securityfs pstore bpf configfs fusectl binfmt_misc autofs squashfs ramfs efivarfs nsfs overlay ecryptfs fuse rpc_pipefs nfs nfs4 cifs smb3 ceph glusterfs 9p".split())
SKIP_IFACE = tuple("lo docker veth br- virbr tap tun wg tailscale cni flannel podman fwbr fwpr fwln ifb gretap erspan kube cali nerdctl lxc cilium zt".split())
UINT_FIELDS = "mem_total mem_used swap_total swap_used disk_total disk_used net_rx net_tx net_rx_total net_tx_total uptime tcp udp procs".split()
METRIC_FIELDS = set(UINT_FIELDS + ["scope", "cpu", "load"])
PUBLIC_FIELDS = set("id name session freshness last_seen metrics_at interval_seconds frp hardware metrics public_note traffic_today accounting_state".split())
PUBLIC_OPTIONAL_FIELDS = {"billing", "traffic_plan"}
MAX_U64 = 2**64 - 1


class AcceptanceError(Exception):
    """Only fixed, non-sensitive codes may be supplied."""


def read(path):
    with open(path, encoding="utf-8") as stream:
        result = stream.read(4 * 1024 * 1024 + 1)
    if len(result) > 4 * 1024 * 1024:
        raise AcceptanceError("reference_file_too_large")
    return result


def meminfo(text):
    values = {}
    for line in text.splitlines():
        key, _, rest = line.partition(":")
        if key in {"MemTotal", "MemAvailable", "MemFree", "Buffers", "Cached", "SwapTotal", "SwapFree"}:
            columns = rest.split()
            if len(columns) != 2 or columns[1] != "kB" or key in values:
                raise AcceptanceError("invalid_memory_reference")
            values[key] = int(columns[0]) * 1024
    available = values.get("MemAvailable")
    if available is None:
        available = sum(values[k] for k in ("MemFree", "Buffers", "Cached"))
    return {"mem_total": values["MemTotal"], "mem_used": max(0, values["MemTotal"] - available),
            "swap_total": values["SwapTotal"], "swap_used": max(0, values["SwapTotal"] - values["SwapFree"])}


def cpu_counters(text):
    columns = text.splitlines()[0].split()
    if columns[0] != "cpu" or len(columns) < 6:
        raise AcceptanceError("invalid_cpu_reference")
    values = [int(x) for x in columns[1:9]]
    return sum(values), values[3] + values[4]


def mount_points(text):
    rows, last, seen, result = [], {}, set(), []
    decode = lambda value: re.sub(r"\\(040|011|012|134)", lambda match: chr(int(match[1], 8)), value)
    for line in text.splitlines():
        if not line.strip():
            continue
        fields = line.split()
        if len(fields) < 6:
            raise AcceptanceError("invalid_mount_reference")
        source, point, kind = decode(fields[0]), decode(fields[1]), fields[2]
        if not point.startswith("/") or "\0" in point:
            raise AcceptanceError("invalid_mount_reference")
        last[point] = len(rows)
        rows.append((source, point, kind))
    for index, (source, point, kind) in enumerate(rows):
        if last[point] != index or kind in SKIP_FS or kind.split(".")[0] in SKIP_FS:
            continue
        if not source.startswith("/") and kind not in {"btrfs", "zfs"}:
            continue
        key = "zfs:" + source.split("/")[0] if kind == "zfs" else source
        if key not in seen:
            seen.add(key)
            result.append(point)
    return result


def included_interface(name, only, excluded):
    if name in excluded:
        return False
    if only:
        return name in only
    if name.startswith(SKIP_IFACE + ("bond", "br", "vlan", "vmbr", "pppoe-")) or "." in name:
        return False
    folder = Path("/sys/class/net") / name
    try:
        if any(item.name.startswith("lower_") for item in folder.iterdir()):
            return False
    except OSError:
        pass
    try:
        uevent = set(read(folder / "uevent").splitlines())
    except OSError:
        uevent = set()
    if uevent & {"DEVTYPE=bridge", "DEVTYPE=bond"}:
        return False
    if (folder / "device").exists():
        return True
    if (folder / "brport").exists():
        return False
    try:
        kind = read(folder / "type").strip()
    except OSError:
        kind = ""
    return kind not in {"65534", "768", "769", "776", "778", "823"} and not uevent & {"DEVTYPE=vxlan", "DEVTYPE=geneve"}


def socket_counts():
    values = {}
    for path in ("/proc/net/sockstat", "/proc/net/sockstat6"):
        for line in read(path).splitlines():
            columns = line.split()
            values[columns[0]] = dict(zip(columns[1::2], map(int, columns[2::2])))
    return {"tcp": values["TCP:"]["inuse"] + values["TCP:"]["tw"] + values["TCP6:"]["inuse"],
            "udp": values["UDP:"]["inuse"] + values["UDP6:"]["inuse"]}


def reference(iface, previous=None):
    values = meminfo(read("/proc/meminfo"))
    values["load"] = list(map(float, read("/proc/loadavg").split()[:3]))
    values["uptime"] = int(float(read("/proc/uptime").split()[0]))
    values["procs"] = sum(name.isdecimal() and name.isascii() for name in os.listdir("/proc"))
    values.update(socket_counts())
    mounts = mount_points(read("/proc/self/mounts"))
    values["disk_total"] = values["disk_used"] = 0 if mounts else None
    for point in mounts:
        stat = os.statvfs(point)
        size = stat.f_frsize or stat.f_bsize
        values["disk_total"] = min(MAX_U64, values["disk_total"] + stat.f_blocks * size)
        values["disk_used"] = min(MAX_U64, values["disk_used"] + max(0, stat.f_blocks - stat.f_bfree) * size)
    entries = [x.strip() for x in iface.split(",") if x.strip()]
    only = {x for x in entries if not x.startswith("-")}
    excluded = {x[1:] for x in entries if x.startswith("-")}
    counters = {}
    for line in read("/proc/net/dev").splitlines()[2:]:
        name, rest = line.split(":", 1)
        name = name.strip()
        if included_interface(name, only, excluded):
            columns = rest.split()
            counters[name] = (int(columns[0]), int(columns[8]))
    values["net_rx_total"] = min(MAX_U64, sum(pair[0] for pair in counters.values()))
    values["net_tx_total"] = min(MAX_U64, sum(pair[1] for pair in counters.values()))
    cpu = cpu_counters(read("/proc/stat"))
    now = time.monotonic()
    values["cpu"] = values["net_rx"] = values["net_tx"] = None
    if previous:
        delta = cpu[0] - previous["cpu"][0]
        idle = cpu[1] - previous["cpu"][1]
        if delta > 0 and idle >= 0:
            values["cpu"] = max(0, delta - idle) * 100 / delta
        elapsed = now - previous["at"]
        if counters.keys() == previous["counters"].keys() and elapsed > 0:
            differences = [[pair[i] - previous["counters"][name][i] for name, pair in counters.items()] for i in range(2)]
            if all(x >= 0 for group in differences for x in group):
                values["net_rx"], values["net_tx"] = [int(sum(group) / elapsed) for group in differences]
    return {"at": now, "values": values, "cpu": cpu, "counters": counters, "mounts": mounts}


def utility_checks(sample):
    env = {"PATH": "/usr/bin:/bin", "LC_ALL": "C"}
    result = subprocess.run(["free", "--bytes"], env=env, capture_output=True, timeout=5, check=True)
    rows = {parts[0]: parts[1:] for parts in (line.split() for line in result.stdout.decode().splitlines()) if parts}
    checks = {"free_mem_total": int(rows["Mem:"][0]) == sample["values"]["mem_total"],
              "free_swap_total": int(rows["Swap:"][0]) == sample["values"]["swap_total"]}
    if sample["mounts"]:
        result = subprocess.run(["df", "--block-size=1", "--output=size,used", "--"] + sample["mounts"],
                                env=env, capture_output=True, timeout=5, check=True)
        rows = [list(map(int, line.split())) for line in result.stdout.decode().splitlines()[1:]]
        checks["df_filtered_total"] = sum(row[0] for row in rows) == sample["values"]["disk_total"]
        tolerance = max(4 * 1024**2, sample["values"]["disk_total"] // 10000)
        checks["df_filtered_used"] = abs(sum(row[1] for row in rows) - sample["values"]["disk_used"]) <= tolerance
    return checks


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise AcceptanceError("api_redirect_rejected")


def api_client(url, ca):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in {"https", "http"} or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in {"", "/"}:
        raise AcceptanceError("invalid_api_url")
    if parsed.scheme == "http":
        try:
            if not ipaddress.ip_address(parsed.hostname).is_loopback:
                raise AcceptanceError("plaintext_requires_literal_loopback")
        except ValueError:
            raise AcceptanceError("plaintext_requires_literal_loopback") from None
    context = ssl.create_default_context(cafile=ca)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(), urllib.request.HTTPSHandler(context=context))

    def fetch():
        with opener.open(url.rstrip("/") + "/api/public/v1/nodes", timeout=5) as response:
            raw = response.read(4 * 1024 * 1024 + 1)
        if len(raw) > 4 * 1024 * 1024:
            raise AcceptanceError("api_payload_too_large")
        return json.loads(raw)
    return fetch


def metric_value(name, field):
    if not isinstance(field, dict) or set(field) - {"value", "quality", "reason"}:
        raise AcceptanceError("invalid_metric_shape")
    quality, value = field.get("quality"), field.get("value")
    if quality != "ok":
        if quality not in {"warming_up", "unavailable", "unsupported"} or value is not None:
            raise AcceptanceError("invalid_metric_quality")
        return None
    if name in UINT_FIELDS:
        if not isinstance(value, str) or not re.fullmatch(r"0|[1-9][0-9]*", value) or int(value) > MAX_U64:
            raise AcceptanceError("invalid_decimal_uint64")
        return int(value)
    if name == "cpu":
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or not 0 <= value <= 100:
            raise AcceptanceError("invalid_cpu_range")
    elif name == "load":
        if not isinstance(value, list) or len(value) != 3 or any(isinstance(x, bool) or not isinstance(x, (float, int)) or not math.isfinite(x) or x < 0 for x in value):
            raise AcceptanceError("invalid_load_range")
    return value


def validate_public_node(node):
    if not isinstance(node, dict) or not PUBLIC_FIELDS <= node.keys() or node.keys() - PUBLIC_FIELDS - PUBLIC_OPTIONAL_FIELDS:
        raise AcceptanceError("invalid_public_node_fields")


def compare(metrics, refs, results):
    if not isinstance(metrics, dict) or set(metrics) != METRIC_FIELDS or metrics["scope"] not in {"host", "namespace", "unknown"}:
        raise AcceptanceError("invalid_public_metric_fields")
    parsed = {name: metric_value(name, metrics[name]) for name in METRIC_FIELDS - {"scope"}}
    for resource in ("mem", "swap", "disk"):
        used, total = parsed[resource + "_used"], parsed[resource + "_total"]
        if used is not None and total is not None and used > total:
            raise AcceptanceError("used_exceeds_total")
    for name, value in parsed.items():
        expected = [sample["values"][name] for sample in refs if sample["values"][name] is not None]
        status = results.setdefault(name, {"observations": 0, "failures": 0, "max_deviation": 0, "tolerance": 0})
        status["observations"] += 1
        if value is None or not expected:
            status["failures"] += int(value is not None or bool(expected) or metrics[name]["quality"] != "unavailable")
            continue
        tolerance = 0
        if name == "cpu":
            tolerance = 5  # Percentage points, in addition to the local temporal envelope.
        elif name == "load":
            tolerance = 0.05
        elif name.endswith("_used"):
            total = max(sample["values"][name.replace("_used", "_total")] or 0 for sample in refs)
            tolerance = max(8 * 1024**2, total * (0.005 if name == "mem_used" else 0.0001))
        elif name in {"net_rx", "net_tx"}:
            tolerance = max(1024, max(expected) * 0.05)
        elif name == "uptime":
            tolerance = 1
        elif name in {"procs", "tcp", "udp"}:
            tolerance = 5
        if name.endswith("_total"):
            tolerance = 0
        if name == "load":
            deviation = max(max(min(row[i] for row in expected) - value[i], value[i] - max(row[i] for row in expected), 0) for i in range(3))
        else:
            deviation = max(min(expected) - value, value - max(expected), 0)
        status["max_deviation"] = round(max(status["max_deviation"], deviation), 3)
        status["tolerance"] = round(max(status["tolerance"], tolerance), 3)
        status["failures"] += int(deviation > tolerance)


def run(args):
    if platform.system() != "Linux":
        raise AcceptanceError("requires_same_host_linux")
    fetch = api_client(args.url, args.ca)
    initial = reference(args.iface)
    checks = utility_checks(initial)
    refs, results, seen, scopes = [initial], {}, set(), set()
    first, deadline = time.monotonic(), time.monotonic() + args.seconds
    interval = None
    while time.monotonic() < deadline:
        sample = reference(args.iface, refs[-1])
        refs.append(sample)
        document = fetch()
        if not isinstance(document, dict) or set(document) != {"nodes", "generated_at"} or not isinstance(document["nodes"], list):
            raise AcceptanceError("invalid_public_snapshot")
        nodes = [node for node in document["nodes"] if not args.node_id or node.get("id") == args.node_id]
        if len(nodes) != 1:
            raise AcceptanceError("select_exactly_one_local_node")
        node = nodes[0]
        validate_public_node(node)
        if node["session"] != "online" or node["freshness"] != "fresh":
            raise AcceptanceError("node_not_online_and_fresh")
        interval = node["interval_seconds"]
        if isinstance(interval, bool) or not isinstance(interval, int) or not 1 <= interval <= 10 or args.seconds < 4 * interval + 4:
            raise AcceptanceError("observation_too_short_for_report_interval")
        window = 3 * interval + 2
        stamp = node["metrics_at"]
        if not isinstance(stamp, str):
            raise AcceptanceError("missing_metric_timestamp")
        age = time.time() - datetime.datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp()
        if not -1 <= age <= window:
            raise AcceptanceError("metric_timestamp_outside_window")
        refs = [item for item in refs if sample["at"] - item["at"] <= window]
        if sample["at"] - first >= window and stamp not in seen:
            # Fetch can overlap the next agent collection; a post-fetch observation
            # prevents claiming a legitimate newer counter is outside the envelope.
            after = reference(args.iface, sample)
            refs.append(after)
            compare(node["metrics"], refs, results)
            scopes.add(node["metrics"]["scope"])
            seen.add(stamp)
        time.sleep(min(0.5, max(0, deadline - time.monotonic())))
    if len(seen) < 3:
        raise AcceptanceError("insufficient_distinct_api_samples")
    for value in results.values():
        value["state"] = "pass" if not value["failures"] else "fail"
    return {"schema": 1, "passed": all(checks.values()) and all(value["state"] == "pass" for value in results.values()),
            "platform": "linux", "architecture": platform.machine(), "seconds": args.seconds,
            "distinct_api_samples": len(seen), "report_interval_seconds": interval,
            "observed_scopes": sorted(scopes),
            "reference_window_seconds": 3 * interval + 2,
            "references": checks, "metrics": dict(sorted(results.items())),
            "notes": ["same_host_and_same_namespace_required", "unknown_scope_is_conservative_not_a_collection_failure",
                      "host_scope_evidence_is_deployment_context_not_inferred_from_metrics",
                      "local_temporal_envelope_not_clock_synchronized_equality",
                      "cpu_iowait_is_idle_guest_not_double_counted", "memory_used_is_total_minus_available_not_free_used",
                      "df_only_filtered_deduplicated_local_mounts", "network_totals_require_matching_interface_policy",
                      "output_omits_node_identity_interface_names_paths_and_raw_errors"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", required=True, help="Monitor origin on the same Linux host")
    parser.add_argument("--ca", help="Trusted PEM CA; system trust when omitted")
    parser.add_argument("--node-id", help="Select the locally running agent if several nodes exist")
    parser.add_argument("--iface", default="", help="Match the agent's explicit interface include/exclude policy")
    parser.add_argument("--seconds", type=int, default=15, choices=range(8, 121), metavar="8..120")
    args = parser.parse_args()
    try:
        result = run(args)
    except AcceptanceError as exc:
        result = {"schema": 1, "passed": False, "error": str(exc)}
    except (OSError, ValueError, KeyError, TypeError, IndexError, subprocess.SubprocessError, urllib.error.URLError):
        result = {"schema": 1, "passed": False, "error": "reference_or_api_read_failed"}
    print(json.dumps(result, ensure_ascii=True, separators=(",", ":"), sort_keys=True))
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
