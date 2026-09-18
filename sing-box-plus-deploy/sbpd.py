#!/usr/bin/env python3
"""Render isolated GOST and nftables bundles; never connects to a server."""
from __future__ import annotations

import argparse
import ipaddress
import json
from pathlib import Path
import re
import sys

CONFIG_ROOT = "/etc/sing-box-plus-deploy"
BINARY_ROOT = "/opt/sing-box-plus-deploy/bin"
NAT_TABLE = "sbpd_nat"
NAME = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}\Z")
OLD_NAMES = {"forward_chain_nat", "forward-chain", "forward-chain.service"}


class InventoryError(ValueError):
    """An inventory cannot safely be rendered."""


def fields(value, allowed, where, required=()):
    if not isinstance(value, dict):
        raise InventoryError(f"{where}: expected an object")
    if set(value) - set(allowed):
        raise InventoryError(f"{where}: unknown fields: {', '.join(sorted(set(value) - set(allowed)))}")
    if set(required) - set(value):
        raise InventoryError(f"{where}: missing fields: {', '.join(sorted(set(required) - set(value)))}")


def name(value, where):
    if not isinstance(value, str) or not NAME.fullmatch(value) or value in OLD_NAMES:
        raise InventoryError(f"{where}: invalid or reserved name")


def port(value, where):
    if type(value) is not int or not 1 <= value <= 65535:
        raise InventoryError(f"{where}: expected an integer port from 1 to 65535")


def ip(value, where):
    try:
        if not isinstance(value, str):
            raise ValueError()
        return ipaddress.IPv4Address(value)
    except ValueError as exc:
        raise InventoryError(f"{where}: expected an IPv4 literal") from exc


def endpoint(value, where):
    fields(value, {"host", "port"}, where, {"host", "port"})
    ip(value["host"], where + ".host")
    port(value["port"], where + ".port")


def protocols(value, where):
    if not isinstance(value, list) or not value or any(p not in ("tcp", "udp") for p in value) or len(set(value)) != len(value):
        raise InventoryError(f"{where}: use a nonempty unique list of tcp and/or udp")


def tls_path(value, where):
    if not isinstance(value, str) or not value.startswith(CONFIG_ROOT + "/") or ".." in Path(value).parts or any(c in value for c in "\r\n\0"):
        raise InventoryError(f"{where}: expected a file inside {CONFIG_ROOT}")


def tls_client(value, where):
    fields(value, {"ca_file", "server_name"}, where, {"ca_file", "server_name"})
    tls_path(value["ca_file"], where + ".ca_file")
    server_name = value["server_name"]
    if not isinstance(server_name, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.-]{0,252}", server_name):
        raise InventoryError(f"{where}.server_name: invalid TLS server name")


def add_binding(bindings, host, number, protocol, where):
    for old_host, old_port, old_protocol, old_where in bindings:
        if old_port == number and old_protocol == protocol and (host == old_host or "0.0.0.0" in (host, old_host)):
            raise InventoryError(f"{where}: {protocol} listener overlaps {old_where}")
    bindings.append((host, number, protocol, where))


def validate(inventory):
    fields(inventory, {"version", "nodes", "metadata"}, "inventory", {"version", "nodes"})
    if type(inventory["version"]) is not int or inventory["version"] != 1:
        raise InventoryError("inventory.version: only version 1 is supported")
    nodes = inventory["nodes"]
    if not isinstance(nodes, dict) or not nodes:
        raise InventoryError("inventory.nodes: expected a nonempty object")
    for node_name, node in nodes.items():
        name(node_name, "node name")
        where = "nodes." + node_name
        fields(node, {"listeners", "reserved_listeners", "nat", "metadata"}, where)
        bindings = []
        reserved = node.get("reserved_listeners", [])
        if not isinstance(reserved, list):
            raise InventoryError(where + ".reserved_listeners: expected an array")
        for index, binding in enumerate(reserved):
            loc = f"{where}.reserved_listeners[{index}]"
            fields(binding, {"host", "port", "protocols"}, loc, {"host", "port", "protocols"})
            ip(binding["host"], loc + ".host")
            port(binding["port"], loc + ".port")
            protocols(binding["protocols"], loc + ".protocols")
            # Existing reservations may overlap; only new bindings are checked.
            bindings.extend((binding["host"], binding["port"], p, loc) for p in binding["protocols"])
        listeners = node.get("listeners", [])
        if not isinstance(listeners, list):
            raise InventoryError(where + ".listeners: expected an array")
        names = set()
        for index, listener in enumerate(listeners):
            loc = f"{where}.listeners[{index}]"
            fields(listener, {"name", "mode", "listen", "protocols", "target", "chain", "tls", "allowed_sources"}, loc, {"name", "mode", "listen", "protocols"})
            name(listener["name"], loc + ".name")
            if listener["name"] in names:
                raise InventoryError(loc + ": duplicate listener name")
            names.add(listener["name"])
            endpoint(listener["listen"], loc + ".listen")
            protocols(listener["protocols"], loc + ".protocols")
            if listener["mode"] not in ("forward", "relay"):
                raise InventoryError(loc + ".mode: use forward or relay")
            if "target" in listener:
                endpoint(listener["target"], loc + ".target")
            if "chain" in listener:
                chain = listener["chain"]
                fields(chain, {"host", "port", "tls"}, loc + ".chain", {"host", "port", "tls"})
                endpoint({k: chain[k] for k in ("host", "port")}, loc + ".chain")
                tls_client(chain["tls"], loc + ".chain.tls")
            if listener["mode"] == "relay":
                if listener["protocols"] != ["tcp"] or "chain" in listener or "target" not in listener or "tls" not in listener:
                    raise InventoryError(loc + ": relay requires TCP MTLS, a fixed target, server TLS, and no chain")
                fields(listener["tls"], {"cert_file", "key_file"}, loc + ".tls", {"cert_file", "key_file"})
                for field in ("cert_file", "key_file"):
                    tls_path(listener["tls"][field], loc + ".tls." + field)
            elif "tls" in listener or ("chain" not in listener and "target" not in listener):
                raise InventoryError(loc + ": forward requires a target or chain and cannot have server TLS")
            allowed = listener.get("allowed_sources", [])
            if not isinstance(allowed, list):
                raise InventoryError(loc + ".allowed_sources: expected an array")
            if not allowed and (listener["mode"] == "relay" or not ipaddress.ip_address(listener["listen"]["host"]).is_loopback):
                raise InventoryError(loc + ": externally reachable listeners require allowed_sources")
            for source in allowed:
                try:
                    if not isinstance(source, str) or ipaddress.ip_network(source, strict=False).version != 4:
                        raise ValueError()
                except ValueError as exc:
                    raise InventoryError(loc + ".allowed_sources: expected IPv4 addresses or CIDRs") from exc
            for protocol in listener["protocols"]:
                add_binding(bindings, listener["listen"]["host"], listener["listen"]["port"], protocol, loc)
        if "nat" in node:
            validate_nat(node["nat"], where + ".nat", bindings)
    return inventory


def validate_nat(nat, where, bindings):
    fields(nat, {"ingress_interface", "source_nat", "forwards"}, where, {"ingress_interface", "source_nat", "forwards"})
    interface = nat["ingress_interface"]
    if not isinstance(interface, str) or not re.fullmatch(r"[a-zA-Z0-9_.:-]{1,15}", interface):
        raise InventoryError(where + ".ingress_interface: invalid interface")
    snat = nat["source_nat"]
    fields(snat, {"mode", "address"}, where + ".source_nat", {"mode"})
    if snat["mode"] == "snat":
        if "address" not in snat:
            raise InventoryError(where + ".source_nat: snat requires address")
        ip(snat["address"], where + ".source_nat.address")
    elif snat["mode"] != "masquerade" or "address" in snat:
        raise InventoryError(where + ".source_nat: use snat with address, or masquerade without address")
    forwards = nat["forwards"]
    if not isinstance(forwards, list) or not forwards:
        raise InventoryError(where + ".forwards: expected a nonempty array")
    names = set()
    for index, rule in enumerate(forwards):
        loc = f"{where}.forwards[{index}]"
        fields(rule, {"name", "listen_port", "protocols", "target"}, loc, {"name", "listen_port", "protocols", "target"})
        name(rule["name"], loc + ".name")
        if rule["name"] in names:
            raise InventoryError(loc + ": duplicate forwarding name")
        names.add(rule["name"])
        port(rule["listen_port"], loc + ".listen_port")
        protocols(rule["protocols"], loc + ".protocols")
        endpoint(rule["target"], loc + ".target")
        for protocol in rule["protocols"]:
            add_binding(bindings, "0.0.0.0", rule["listen_port"], protocol, loc)


def addr(endpoint):
    return f"{endpoint['host']}:{endpoint['port']}"


def render_chain(base, chain):
    return {
        "name": base + "-chain",
        "hops": [{"name": base + "-hop", "nodes": [{
            "name": base + "-next",
            "addr": addr(chain),
            "connector": {"type": "relay", "metadata": {"udpBufferSize": 65535}},
            "dialer": {
                "type": "mtls",
                "tls": {"caFile": chain["tls"]["ca_file"], "serverName": chain["tls"]["server_name"], "secure": True},
                "metadata": {"mux.keepaliveDisabled": False, "mux.keepaliveInterval": "10s", "mux.keepaliveTimeout": "30s"},
            },
        }]}],
    }


def render_gost(node):
    config = {"services": [], "chains": [], "admissions": []}
    for listener in node.get("listeners", []):
        base = "sbpd-" + listener["name"]
        if listener.get("allowed_sources"):
            config["admissions"].append({"name": base + "-allow", "whitelist": True, "matchers": listener["allowed_sources"]})
        if "chain" in listener:
            config["chains"].append(render_chain(base, listener["chain"]))
        for protocol in listener["protocols"]:
            relay = listener["mode"] == "relay"
            service = {
                "name": base + "-" + ("relay" if relay else protocol),
                "addr": addr(listener["listen"]),
                "handler": {"type": "relay" if relay else protocol},
                "listener": {"type": "mtls" if relay else protocol},
            }
            if relay:
                service["handler"]["metadata"] = {"udpBufferSize": 65535}
                service["listener"]["tls"] = {"certFile": listener["tls"]["cert_file"], "keyFile": listener["tls"]["key_file"]}
            else:
                service["listener"]["metadata"] = {"keepalive": True}
                if protocol == "udp":
                    service["listener"]["metadata"].update({"ttl": "60s", "readBufferSize": 65535})
            if "chain" in listener:
                service["handler"]["chain"] = base + "-chain"
            if "target" in listener:
                service["forwarder"] = {"nodes": [{"name": base + "-target", "addr": addr(listener["target"])}]}
            if listener.get("allowed_sources"):
                service["admission"] = base + "-allow"
            config["services"].append(service)
    return {key: value for key, value in config.items() if value}


def render_nat(nat):
    interface = json.dumps(nat["ingress_interface"])
    prerouting, postrouting = [], []
    snat = nat["source_nat"]
    action = "masquerade" if snat["mode"] == "masquerade" else "snat to " + snat["address"]
    for rule in nat["forwards"]:
        host, number = rule["target"]["host"], rule["target"]["port"]
        for protocol in rule["protocols"]:
            comment = json.dumps("sbpd:" + rule["name"] + ":" + protocol)
            prerouting.append(f"    iifname {interface} fib daddr type local {protocol} dport {rule['listen_port']} counter dnat to {host}:{number} comment {comment}")
            postrouting.append(f"    iifname {interface} ct status dnat meta l4proto {protocol} ct original proto-dst {rule['listen_port']} ip daddr {host} {protocol} dport {number} counter {action} comment {comment}")
    return "\n".join([
        f"table ip {NAT_TABLE} {{",
        "  chain prerouting {",
        "    type nat hook prerouting priority -101; policy accept;",
        *prerouting,
        "  }",
        "  chain postrouting {",
        "    type nat hook postrouting priority 99; policy accept;",
        *postrouting,
        "  }",
        "}",
        "",
    ])


GOST_UNIT = f"""[Unit]
Description=Isolated GOST forwarding for sing-box-plus-deploy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sbpd
Group=sbpd
ExecStart={BINARY_ROOT}/gost -C {CONFIG_ROOT}/gost.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=1048576
MemoryHigh=96M
MemoryMax=160M

[Install]
WantedBy=multi-user.target
"""

NAT_UNIT = f"""[Unit]
Description=Isolated NAT forwarding for sing-box-plus-deploy
After=network-online.target nftables.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart={CONFIG_ROOT}/apply-nat.sh
ExecReload={CONFIG_ROOT}/apply-nat.sh

[Install]
WantedBy=multi-user.target
"""

NAT_LOADER = f"""#!/bin/sh
# Applies one atomic transaction to this project's table only.
# Stop retains rules; remove only this project's table for rollback.
set -eu
umask 077
exec 9>/run/sing-box-plus-deploy-nat.lock
flock -x 9
transaction=$(mktemp /run/sing-box-plus-deploy-nat.XXXXXX)
trap 'test ! -f "$transaction" || unlink "$transaction"' EXIT HUP INT TERM
if nft list table ip {NAT_TABLE} >/dev/null 2>&1; then
    printf '%s\\n' 'delete table ip {NAT_TABLE}' > "$transaction"
fi
cat {CONFIG_ROOT}/sbpd_nat.nft >> "$transaction"
nft --check --file "$transaction"
nft --file "$transaction"
"""


def render(inventory):
    """Return path -> text, validating the whole inventory before any writes."""
    validate(inventory)
    result = {}
    for node_name, node in inventory["nodes"].items():
        config = f"{node_name}{CONFIG_ROOT}"
        units = f"{node_name}/etc/systemd/system"
        if node.get("listeners"):
            result[config + "/gost.json"] = json.dumps(render_gost(node), ensure_ascii=False, indent=2) + "\n"
            result[units + "/sing-box-plus-deploy-gost.service"] = GOST_UNIT
        if "nat" in node:
            result[config + "/sbpd_nat.nft"] = render_nat(node["nat"])
            result[config + "/apply-nat.sh"] = NAT_LOADER
            result[units + "/sing-box-plus-deploy-nat.service"] = NAT_UNIT
    return result


def write_bundle(inventory, output):
    files = render(inventory)
    root = Path(output).resolve()
    # Refuse reuse so files removed from an inventory cannot linger.
    if root.exists() and any(root.iterdir()):
        raise InventoryError("output directory must be empty")
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    for relative, text in files.items():
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        path.write_text(text, encoding="utf-8")
        path.chmod(0o700 if path.suffix == ".sh" else 0o600)
    return files


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    command = commands.add_parser("render", help="validate inventory and render a new local bundle")
    command.add_argument("--inventory", required=True, type=Path)
    command.add_argument("--output", required=True, type=Path)
    args = parser.parse_args(argv)
    try:
        with args.inventory.open(encoding="utf-8") as stream:
            inventory = json.load(stream)
        files = write_bundle(inventory, args.output)
    except (OSError, ValueError) as exc:
        parser.exit(2, f"error: {exc}\n")
    print(f"Rendered {len(files)} files into {args.output.resolve()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
