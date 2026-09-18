import copy
import json
from pathlib import Path
import tempfile
import unittest

import sbpd


def inventory():
    return {
        "version": 1,
        "nodes": {
            "edge": {
                "nat": {
                    "ingress_interface": "eth0",
                    "source_nat": {"mode": "masquerade"},
                    "forwards": [
                        {"name": "direct", "listen_port": 25001, "protocols": ["tcp", "udp"], "target": {"host": "192.0.2.10", "port": 24000}},
                        {"name": "chain", "listen_port": 25002, "protocols": ["tcp", "udp"], "target": {"host": "192.0.2.20", "port": 25002}},
                    ],
                },
                "reserved_listeners": [{"host": "0.0.0.0", "port": 19000, "protocols": ["tcp", "udp"]}],
            },
            "entry": {
                "listeners": [{
                    "name": "chain-entry",
                    "mode": "forward",
                    "listen": {"host": "0.0.0.0", "port": 25002},
                    "protocols": ["tcp", "udp"],
                    "allowed_sources": ["192.0.2.1"],
                    "chain": {"host": "192.0.2.30", "port": 25002, "tls": {"ca_file": "/etc/sing-box-plus-deploy/tls/ca.pem", "server_name": "relay.example"}},
                }],
            },
            "relay": {
                "listeners": [{
                    "name": "relay",
                    "mode": "relay",
                    "listen": {"host": "0.0.0.0", "port": 25002},
                    "protocols": ["tcp"],
                    "allowed_sources": ["192.0.2.20"],
                    "target": {"host": "127.0.0.1", "port": 24000},
                    "tls": {"cert_file": "/etc/sing-box-plus-deploy/tls/server.crt", "key_file": "/etc/sing-box-plus-deploy/tls/server.key"},
                }],
            },
        },
    }


class ValidationTests(unittest.TestCase):
    def test_invalid_ports(self):
        for value in (0, 65536, -1, True, "25001"):
            with self.subTest(port=value):
                inv = inventory()
                inv["nodes"]["edge"]["nat"]["forwards"][0]["listen_port"] = value
                with self.assertRaises(sbpd.InventoryError):
                    sbpd.render(inv)

    def test_wildcard_overlaps_loopback_on_same_protocol(self):
        inv = inventory()
        entry = inv["nodes"]["entry"]
        other = copy.deepcopy(entry["listeners"][0])
        other["name"] = "local-bridge"
        other["listen"]["host"] = "127.0.0.1"
        entry["listeners"].append(other)
        with self.assertRaisesRegex(sbpd.InventoryError, "overlaps"):
            sbpd.render(inv)

    def test_conflict_with_existing_listener(self):
        inv = inventory()
        inv["nodes"]["entry"]["reserved_listeners"] = [{"host": "0.0.0.0", "port": 25002, "protocols": ["tcp"]}]
        with self.assertRaisesRegex(sbpd.InventoryError, "reserved_listeners"):
            sbpd.render(inv)

    def test_nat_collision_with_existing_or_new_listener(self):
        inv = inventory()
        inv["nodes"]["edge"]["nat"]["forwards"][0]["listen_port"] = 19000
        with self.assertRaisesRegex(sbpd.InventoryError, "overlaps"):
            sbpd.render(inv)
        inv = inventory()
        inv["nodes"]["edge"]["listeners"] = copy.deepcopy(inv["nodes"]["entry"]["listeners"])
        with self.assertRaisesRegex(sbpd.InventoryError, "overlaps"):
            sbpd.render(inv)

    def test_same_port_tcp_and_udp_are_distinct(self):
        inv = inventory()
        entry = inv["nodes"]["entry"]
        tcp = entry["listeners"][0]
        udp = copy.deepcopy(tcp)
        tcp["protocols"] = ["tcp"]
        udp.update(name="entry-udp", protocols=["udp"])
        entry["listeners"].append(udp)
        sbpd.validate(inv)

    def test_direct_forward_requires_fixed_target(self):
        inv = inventory()
        del inv["nodes"]["entry"]["listeners"][0]["chain"]
        with self.assertRaisesRegex(sbpd.InventoryError, "requires a target"):
            sbpd.render(inv)

    def test_relay_requires_fixed_target(self):
        inv = inventory()
        del inv["nodes"]["relay"]["listeners"][0]["target"]
        with self.assertRaisesRegex(sbpd.InventoryError, "fixed target"):
            sbpd.render(inv)

    def test_external_listener_requires_admission(self):
        inv = inventory()
        del inv["nodes"]["entry"]["listeners"][0]["allowed_sources"]
        with self.assertRaisesRegex(sbpd.InventoryError, "allowed_sources"):
            sbpd.render(inv)

    def test_loopback_bridge_needs_no_admission(self):
        inv = inventory()
        entry = inv["nodes"]["entry"]["listeners"][0]
        entry["listen"]["host"] = "127.0.0.1"
        del entry["allowed_sources"]
        sbpd.validate(inv)

    def test_no_legacy_table_or_unit_override(self):
        for key, value in (("table", "forward_chain_nat"), ("service", "forward-chain.service")):
            inv = inventory()
            inv["nodes"]["edge"]["nat"][key] = value
            with self.assertRaisesRegex(sbpd.InventoryError, "unknown fields"):
                sbpd.render(inv)
        inv = inventory()
        inv["nodes"]["forward-chain"] = inv["nodes"].pop("edge")
        with self.assertRaisesRegex(sbpd.InventoryError, "reserved name"):
            sbpd.render(inv)

    def test_path_escape_rejected(self):
        inv = inventory()
        inv["nodes"]["relay"]["listeners"][0]["tls"]["key_file"] = "/etc/sing-box-plus-deploy/../gost/server.key"
        with self.assertRaisesRegex(sbpd.InventoryError, "expected a file inside"):
            sbpd.render(inv)


class RenderingTests(unittest.TestCase):
    def test_tcp_udp_share_chain_and_validate_remote_tls(self):
        files = sbpd.render(inventory())
        config = json.loads(files["entry/etc/sing-box-plus-deploy/gost.json"])
        self.assertEqual({s["handler"]["type"] for s in config["services"]}, {"tcp", "udp"})
        self.assertEqual({s["handler"]["chain"] for s in config["services"]}, {config["chains"][0]["name"]})
        self.assertTrue(all("forwarder" not in s for s in config["services"]))
        next_node = config["chains"][0]["hops"][0]["nodes"][0]
        self.assertEqual(next_node["connector"]["type"], "relay")
        self.assertEqual(next_node["dialer"]["type"], "mtls")
        self.assertIs(next_node["dialer"]["tls"]["secure"], True)
        self.assertEqual(next_node["dialer"]["tls"]["serverName"], "relay.example")
        self.assertIs(config["admissions"][0]["whitelist"], True)
        self.assertEqual(config["admissions"][0]["matchers"], ["192.0.2.1"])

    def test_relay_is_fixed_target_over_tcp(self):
        files = sbpd.render(inventory())
        config = json.loads(files["relay/etc/sing-box-plus-deploy/gost.json"])
        self.assertEqual(len(config["services"]), 1)
        service = config["services"][0]
        self.assertEqual(service["handler"]["type"], "relay")
        self.assertEqual(service["listener"]["type"], "mtls")
        self.assertEqual(service["forwarder"]["nodes"][0]["addr"], "127.0.0.1:24000")

    def test_nat_scope_includes_interface_original_port_and_target(self):
        text = sbpd.render(inventory())["edge/etc/sing-box-plus-deploy/sbpd_nat.nft"]
        rules = [line.strip() for line in text.splitlines() if "counter" in line]
        self.assertEqual(len(rules), 8)
        self.assertTrue(all('iifname "eth0"' in line for line in rules))
        for line in rules:
            if "dnat to" in line:
                self.assertIn("fib daddr type local", line)
            else:
                self.assertIn("ct status dnat", line)
                self.assertIn("ct original proto-dst", line)
                self.assertIn("ip daddr", line)
        self.assertIn("ct original proto-dst 25001 ip daddr 192.0.2.10 tcp dport 24000", text)
        self.assertNotIn("forward_chain_nat", text)

    def test_tcp_only_and_explicit_snat(self):
        inv = inventory()
        nat = inv["nodes"]["edge"]["nat"]
        nat["forwards"] = [nat["forwards"][0]]
        nat["forwards"][0]["protocols"] = ["tcp"]
        nat["source_nat"] = {"mode": "snat", "address": "192.0.2.1"}
        text = sbpd.render(inv)["edge/etc/sing-box-plus-deploy/sbpd_nat.nft"]
        self.assertIn("snat to 192.0.2.1", text)
        self.assertNotIn("udp", text)
        self.assertNotIn("masquerade", text)

    def test_loader_is_atomic_and_only_manages_own_table(self):
        text = sbpd.NAT_LOADER
        self.assertNotIn("flush", text)
        self.assertEqual(text.count("delete table"), 1)
        self.assertIn("delete table ip sbpd_nat", text)
        self.assertIn('nft --file "$transaction"', text)
        self.assertLess(text.index('nft --check --file'), text.index('nft --file'))
        self.assertIn("flock -x", text)
        self.assertNotIn("ExecStop", sbpd.NAT_UNIT)

    def test_new_service_uses_independent_binary_account_and_config(self):
        self.assertIn("User=sbpd", sbpd.GOST_UNIT)
        self.assertIn("Group=sbpd", sbpd.GOST_UNIT)
        self.assertIn("/opt/sing-box-plus-deploy/bin/gost -C /etc/sing-box-plus-deploy/gost.json", sbpd.GOST_UNIT)

    def test_invalid_inventory_writes_nothing_and_output_cannot_be_reused(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "bundle"
            inv = inventory()
            inv["nodes"]["relay"]["listeners"][0]["listen"]["port"] = 0
            with self.assertRaises(sbpd.InventoryError):
                sbpd.write_bundle(inv, output)
            self.assertFalse(output.exists())
            files = sbpd.write_bundle(inventory(), output)
            for relative in files:
                self.assertTrue((output / relative).is_file())
                self.assertEqual((output / relative).stat().st_mode & 0o077, 0)
            with self.assertRaisesRegex(sbpd.InventoryError, "empty"):
                sbpd.write_bundle(inventory(), output)


if __name__ == "__main__":
    unittest.main()
