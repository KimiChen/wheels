"""examples/ 目录的防腐与脱敏测试。

与 test_sbpd.py 分开：那个文件用内联 fixture 验证渲染器本身的契约，通读即可看懂全部行为；
本模块读磁盘上的示例，验证「已提交的示例不会腐烂、也不会混入真实基础设施信息」。
两者失败时指向的是不同事故：渲染器坏了，还是示例坏了。
"""
import ast
import base64
import contextlib
import copy
import importlib.util
import io
import ipaddress
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest
import uuid

import sbpd


ROOT = Path(__file__).resolve().parent.parent
EXAMPLES = ROOT / "examples"

# examples/ 的完整内容清单。新增文件必须登记，否则测试变红——这是让
# 「每个示例都被覆盖」这句话在将来仍然为真的唯一结构性办法。
EXPECTED_FILES = [
    "inventory.example.json",
    "inventory.case-multi-region-chain.json",
    "identity-pool.example.json",
    "make_identity_pool.py",
]

# 示例只许出现文档保留地址（RFC 5737）与通配/回环。
DOC_NETWORKS = [ipaddress.ip_network(n) for n in
                ("192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "127.0.0.0/8", "0.0.0.0/32")]
IPV4 = re.compile(r"(?<![\d.])\d{1,3}(?:\.\d{1,3}){3}(?![\d.])")


def text(name):
    return (EXAMPLES / name).read_text(encoding="utf-8")


def load(name):
    return json.loads(text(name))


def inventories():
    """按内容而非文件名筛选：身份池示例不是清单，不能喂给 validate()。"""
    found = []
    for name in EXPECTED_FILES:
        if not name.endswith(".json"):
            continue
        data = load(name)
        if isinstance(data, dict) and "nodes" in data:
            found.append((name, data))
    return found


def addresses(inventory):
    """节点地址 → 节点名。chain.host 只有经它才能解析到节点。"""
    return {node["metadata"]["address"]: name
            for name, node in inventory["nodes"].items()
            if isinstance(node.get("metadata"), dict) and "address" in node["metadata"]}


def listener_on(node, host, port):
    for listener in node.get("listeners", []):
        listen = listener["listen"]
        if listen["port"] == port and listen["host"] in (host, "0.0.0.0"):
            return listener
    return None


def source_address(node):
    """流量离开该节点时对端看到的地址。入口机出网经上游 NAT 转换，与网卡地址不同。"""
    meta = node.get("metadata", {})
    return meta.get("egress_address", meta.get("address"))


def chain_hops(inventory):
    """从每条 NAT forward 出发走图，返回 (入口端口, [(节点名, 到达端口)])。

    relay 止于固定 target；若该 target 是本机回环且有监听器认领，它就是桥接，
    继续走桥接的 chain。无人认领则是本工具范围之外的本机 sing-box，路径到此为止。
    """
    nodes = inventory["nodes"]
    by_address = addresses(inventory)
    walks = []
    for node in nodes.values():
        for rule in node.get("nat", {}).get("forwards", []):
            host, port = rule["target"]["host"], rule["target"]["port"]
            hops, seen = [], set()
            for _ in range(len(nodes) + 1):
                name = by_address.get(host)
                if name is None or name in seen:
                    break
                seen.add(name)
                hops.append((name, port))
                listener = listener_on(nodes[name], host, port)
                if listener is None:
                    break
                if listener["mode"] == "relay":
                    listener = listener_on(nodes[name], "127.0.0.1", listener["target"]["port"])
                    if listener is None:
                        break
                chain = listener.get("chain")
                if chain is None:
                    break
                host, port = chain["host"], chain["port"]
            else:
                raise AssertionError("链路未在节点数内收敛，疑似成环: " + repr(hops))
            walks.append((rule["listen_port"], hops))
    return walks


def chain_paths(inventory):
    return [(port, [name for name, _ in hops]) for port, hops in chain_hops(inventory)]


class ExampleCorpusTests(unittest.TestCase):
    def test_examples_directory_contents_are_claimed(self):
        """清单只管会被提交的文件；字节码是构建产物，已由 .gitignore 排除。"""
        actual = sorted(p.relative_to(EXAMPLES).as_posix()
                        for p in EXAMPLES.rglob("*")
                        if p.is_file() and "__pycache__" not in p.parts
                        and p.suffix != ".pyc")
        self.assertEqual(actual, sorted(EXPECTED_FILES))

    def test_every_example_file_is_utf8_with_a_single_trailing_newline(self):
        for name in EXPECTED_FILES:
            with self.subTest(name=name):
                raw = (EXAMPLES / name).read_bytes()
                body = raw.decode("utf-8")
                self.assertFalse(body.startswith("\ufeff"), "不应有 BOM")
                self.assertNotIn("\r", body)
                self.assertTrue(body.endswith("\n"))
                self.assertFalse(body.endswith("\n\n"))

    def test_every_example_inventory_validates_and_renders(self):
        found = inventories()
        self.assertTrue(found, "至少应有一份示例清单")
        for name, data in found:
            with self.subTest(name=name):
                sbpd.validate(copy.deepcopy(data))
                files = sbpd.render(data)
                self.assertTrue(files)
                for relative, body in files.items():
                    self.assertFalse(relative.startswith("/"))
                    self.assertNotIn("..", Path(relative).parts)
                    self.assertTrue(body.endswith("\n"))
                    if relative.endswith("gost.json"):
                        json.loads(body)

    @unittest.skipUnless(shutil.which("git"), "无 git，跳过忽略规则检查")
    def test_examples_are_not_ignored_by_git(self):
        for name in EXPECTED_FILES:
            with self.subTest(name=name):
                result = subprocess.run(
                    ["git", "-C", str(ROOT), "check-ignore", "--", "examples/" + name],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                if result.returncode == 128:
                    self.skipTest("不是 git 工作树")
                self.assertEqual(result.returncode, 1, name + " 会被 .gitignore 静默吞掉")

    def test_examples_use_only_documentation_addresses(self):
        for name in EXPECTED_FILES:
            with self.subTest(name=name):
                for found in IPV4.findall(text(name)):
                    address = ipaddress.ip_address(found)
                    self.assertTrue(any(address in net for net in DOC_NETWORKS),
                                    name + " 出现非文档保留地址: " + found)

    def test_examples_contain_no_key_material(self):
        for name in EXPECTED_FILES:
            with self.subTest(name=name):
                body = text(name)
                self.assertNotIn("PRIVATE KEY", body)
                self.assertNotIn("BEGIN CERTIFICATE", body)

    def test_examples_only_reference_tls_files_inside_the_config_root(self):
        for _, data in inventories():
            for node in data["nodes"].values():
                for listener in node.get("listeners", []):
                    for key in ("ca_file", "server_name"):
                        chain_tls = listener.get("chain", {}).get("tls", {})
                        if key in chain_tls and key.endswith("_file"):
                            self.assertTrue(chain_tls[key].startswith(sbpd.CONFIG_ROOT + "/"))
                    if "server_name" in listener.get("chain", {}).get("tls", {}):
                        name = listener["chain"]["tls"]["server_name"]
                        self.assertTrue(name.endswith(".example") or name.endswith(".invalid"),
                                        "server_name 应使用保留域名: " + name)
                    for key in ("cert_file", "key_file"):
                        if key in listener.get("tls", {}):
                            self.assertTrue(listener["tls"][key].startswith(sbpd.CONFIG_ROOT + "/"))

    def test_no_rendered_chain_disables_certificate_verification(self):
        for name, data in inventories():
            for relative, body in sbpd.render(data).items():
                if not relative.endswith("gost.json"):
                    continue
                for chain in json.loads(body).get("chains", []):
                    for hop in chain["hops"]:
                        for node in hop["nodes"]:
                            with self.subTest(name=name, node=node["name"]):
                                self.assertEqual(node["connector"]["type"], "relay")
                                self.assertEqual(node["dialer"]["type"], "mtls")
                                tls = node["dialer"]["tls"]
                                self.assertIs(tls["secure"], True)
                                self.assertTrue(tls["caFile"].startswith(sbpd.CONFIG_ROOT + "/"))

    def test_rendering_an_example_twice_is_byte_identical(self):
        for name, _ in inventories():
            with self.subTest(name=name):
                self.assertEqual(sbpd.render(load(name)), sbpd.render(load(name)))

    def test_cli_renders_every_example_inventory(self):
        for name, _ in inventories():
            with self.subTest(name=name):
                with tempfile.TemporaryDirectory() as directory:
                    output = Path(directory) / "bundle"
                    with contextlib.redirect_stdout(io.StringIO()):
                        code = sbpd.main(["render", "--inventory", str(EXAMPLES / name),
                                          "--output", str(output)])
                    self.assertEqual(code, 0)
                    written = sorted(p.relative_to(output).as_posix()
                                     for p in output.rglob("*") if p.is_file())
                    self.assertEqual(written, sorted(sbpd.render(load(name))))
                    with contextlib.redirect_stdout(io.StringIO()), \
                            contextlib.redirect_stderr(io.StringIO()):
                        with self.assertRaises(SystemExit) as raised:
                            sbpd.main(["render", "--inventory", str(EXAMPLES / name),
                                       "--output", str(output)])
                    self.assertEqual(raised.exception.code, 2)


class SimpleExampleTests(unittest.TestCase):
    NAME = "inventory.example.json"

    def test_simple_example_emits_nat_only_on_edge_and_gost_on_each_hop(self):
        files = set(sbpd.render(load(self.NAME)))
        self.assertEqual(files, {
            "edge/etc/sing-box-plus-deploy/sbpd_nat.nft",
            "edge/etc/sing-box-plus-deploy/apply-nat.sh",
            "edge/etc/systemd/system/sing-box-plus-deploy-nat.service",
            "entry/etc/sing-box-plus-deploy/gost.json",
            "entry/etc/systemd/system/sing-box-plus-deploy-gost.service",
            "relay/etc/sing-box-plus-deploy/gost.json",
            "relay/etc/systemd/system/sing-box-plus-deploy-gost.service",
            "exit/etc/sing-box-plus-deploy/gost.json",
            "exit/etc/systemd/system/sing-box-plus-deploy-gost.service",
        })

    def test_simple_example_covers_the_explicit_snat_branch(self):
        body = sbpd.render(load(self.NAME))["edge/etc/sing-box-plus-deploy/sbpd_nat.nft"]
        self.assertIn("snat to 192.0.2.1", body)
        self.assertNotIn("masquerade", body)

    def test_simple_example_chain_walks_from_edge_to_exit(self):
        walked = dict(chain_paths(load(self.NAME)))
        self.assertEqual(walked[25002], ["entry", "relay", "exit"])
        self.assertEqual(walked[25001], ["exit"])

    def test_every_node_declares_an_address_so_chains_can_be_resolved(self):
        for name, data in inventories():
            with self.subTest(name=name):
                self.assertEqual(len(addresses(data)), len(data["nodes"]))


class ProductionCaseTests(unittest.TestCase):
    NAME = "inventory.case-multi-region-chain.json"

    def setUp(self):
        self.case = load(self.NAME)
        self.nodes = self.case["nodes"]
        self.entry = next(n for n in self.nodes.values() if "nat" in n)

    def terminals(self):
        return {name: node for name, node in self.nodes.items() if "nat" not in node}

    def relays(self):
        for name, node in self.terminals().items():
            for listener in node["listeners"]:
                if listener["mode"] == "relay":
                    yield name, node, listener

    def bridges(self):
        for name, node in self.terminals().items():
            for listener in node["listeners"]:
                if listener["mode"] == "forward" and listener["listen"]["host"] == "127.0.0.1":
                    yield name, node, listener

    def test_case_node_roles_determine_the_rendered_file_set(self):
        self.assertEqual(len(self.nodes), 5)
        self.assertEqual(sum("nat" in n for n in self.nodes.values()), 1)
        self.assertNotIn("listeners", self.entry)
        files = sbpd.render(self.case)
        self.assertEqual(len(files), 11)
        for name, node in self.nodes.items():
            produced = {f.split("/", 1)[1] for f in files if f.startswith(name + "/")}
            if "nat" in node:
                self.assertEqual(produced, {
                    "etc/sing-box-plus-deploy/sbpd_nat.nft",
                    "etc/sing-box-plus-deploy/apply-nat.sh",
                    "etc/systemd/system/sing-box-plus-deploy-nat.service"})
            else:
                self.assertEqual(produced, {
                    "etc/sing-box-plus-deploy/gost.json",
                    "etc/systemd/system/sing-box-plus-deploy-gost.service"})

    def test_entry_publishes_its_forwards_over_the_declared_interface(self):
        forwards = self.entry["nat"]["forwards"]
        self.assertEqual(len(forwards), 8)
        self.assertEqual(len({f["name"] for f in forwards}), 8)
        self.assertEqual(len({f["listen_port"] for f in forwards}), 8)
        body = sbpd.render(self.case)["entry/etc/sing-box-plus-deploy/sbpd_nat.nft"]
        # 每条 forward 的每个协议各产生一条 DNAT 与一条回程规则。
        self.assertEqual(body.count("counter"),
                         2 * sum(len(f["protocols"]) for f in forwards))
        interface = self.entry["nat"]["ingress_interface"]
        for line in body.splitlines():
            if "counter" in line:
                self.assertIn('iifname "%s"' % interface, line)
        self.assertIn("masquerade", body)
        self.assertNotIn("forward_chain_nat", body)

    def test_every_nat_forward_reaches_a_declared_listener_or_reserved_port(self):
        """渲染正确但指向无人监听的端口，是真实会发生的事故，而校验器看不见。"""
        known = addresses(self.case)
        external = self.case["metadata"]["external_targets"]
        for rule in self.entry["nat"]["forwards"]:
            host, port = rule["target"]["host"], rule["target"]["port"]
            with self.subTest(name=rule["name"]):
                if host in external:
                    continue
                self.assertIn(host, known, "目标地址既不是已知节点也未登记为外部目标")
                node = self.nodes[known[host]]
                reserved = {r["port"] for r in node.get("reserved_listeners", [])}
                self.assertTrue(listener_on(node, host, port) or port in reserved,
                                "目标端口上既没有本工具的监听器，也没有声明为既有监听")

    def test_chain_depths_grow_from_one_to_four_hops(self):
        walked = dict(chain_paths(self.case))
        self.assertEqual(walked[65001], [], "旧 SOCKS5 目标不属于本组节点")
        self.assertEqual({len(walked[p]) for p in (65002, 65003, 65004, 65005)}, {1})
        self.assertEqual(len(walked[65011]), 2)
        self.assertEqual(len(walked[65012]), 3)
        self.assertEqual(len(walked[65013]), 4)
        reached = {name for path in walked.values() for name in path}
        self.assertEqual(reached, set(self.terminals()), "每个落地节点都应被至少一条链到达")
        for port, path in walked.items():
            self.assertEqual(len(path), len(set(path)), "链路不应重复经过同一节点")

    def test_every_relay_is_tcp_only_with_server_tls_and_an_upstream_allowlist(self):
        seen = 0
        for name, _, listener in self.relays():
            seen += 1
            with self.subTest(node=name, listener=listener["name"]):
                self.assertEqual(listener["protocols"], ["tcp"])
                self.assertNotIn("chain", listener)
                self.assertEqual(listener["listen"]["host"], "0.0.0.0")
                self.assertEqual(listener["target"]["host"], "127.0.0.1")
                self.assertTrue(listener["allowed_sources"])
                for key in ("cert_file", "key_file"):
                    self.assertTrue(listener["tls"][key].startswith(sbpd.CONFIG_ROOT + "/"))
        self.assertEqual(seen, 6, "三条链在 region-b/c/d 上分别落 3、2、1 个 relay")

    def test_each_hop_admits_exactly_the_address_its_upstream_uses(self):
        """5 节点手写清单里最可能的人为错误，就是白名单复制后忘了改。"""
        for port, hops in chain_hops(self.case):
            for index, (name, arrival) in enumerate(hops):
                listener = listener_on(self.nodes[name], addresses(self.case) and
                                       self.nodes[name]["metadata"]["address"], arrival)
                if listener is None or not listener.get("allowed_sources"):
                    continue
                upstream = (source_address(self.entry) if index == 0
                            else self.nodes[hops[index - 1][0]]["metadata"]["address"])
                covered = any(ipaddress.ip_address(upstream) in
                              ipaddress.ip_network(entry, strict=False)
                              for entry in listener["allowed_sources"])
                with self.subTest(entry_port=port, node=name):
                    self.assertTrue(covered, "%s 的 %s 未放行上游 %s" %
                                    (name, listener["name"], upstream))

    def test_loopback_bridges_are_local_only_and_carry_no_admission(self):
        rendered = sbpd.render(self.case)
        seen = 0
        for name, node, listener in self.bridges():
            seen += 1
            with self.subTest(node=name, listener=listener["name"]):
                self.assertNotIn("allowed_sources", listener)
                self.assertIn("chain", listener)
                self.assertNotIn("target", listener)
                self.assertEqual(listener["protocols"], ["tcp", "udp"])
                # 桥接必须确实被某个 relay 的 target 指到，否则是死配置。
                owners = [r for r in node["listeners"] if r["mode"] == "relay"
                          and r["target"]["port"] == listener["listen"]["port"]]
                self.assertEqual(len(owners), 1)
                gost = json.loads(rendered[name + "/etc/sing-box-plus-deploy/gost.json"])
                for service in gost["services"]:
                    if service["name"].startswith("sbpd-%s-" % listener["name"]):
                        self.assertNotIn("admission", service)
                self.assertNotIn("sbpd-%s-allow" % listener["name"],
                                 {a["name"] for a in gost.get("admissions", [])})
        self.assertEqual(seen, 3)

    def test_each_terminal_reserves_its_existing_listeners_without_duplicates(self):
        for name, node in self.terminals().items():
            with self.subTest(node=name):
                reserved = node["reserved_listeners"]
                self.assertEqual(len(reserved), 7)
                triples = {(r["host"], r["port"], protocol)
                           for r in reserved for protocol in r["protocols"]}
                self.assertEqual(len(triples), sum(len(r["protocols"]) for r in reserved),
                                 "保留监听重复了——validate() 明确不检查这一条")

    def test_the_local_proxy_port_behind_each_relay_is_declared_reserved(self):
        for name, node, listener in self.relays():
            port = listener["target"]["port"]
            if listener_on(node, "127.0.0.1", port) is not None:
                continue  # 回环桥接，不是本机 sing-box
            with self.subTest(node=name, port=port):
                self.assertIn(port, {r["port"] for r in node["reserved_listeners"]},
                              "本机 sing-box 端口未声明，重叠检查就是空的")

    def test_no_rendered_port_is_privileged(self):
        """非特权 sbpd 账户绑不了 1024 以下；保留监听描述的是别人的服务，不受此限。"""
        for name, node in self.nodes.items():
            for listener in node.get("listeners", []):
                with self.subTest(node=name, listener=listener["name"]):
                    self.assertGreaterEqual(listener["listen"]["port"], 1024)
            for rule in node.get("nat", {}).get("forwards", []):
                with self.subTest(node=name, forward=rule["name"]):
                    self.assertGreaterEqual(rule["listen_port"], 1024)

    def test_case_declares_its_sanitization(self):
        meta = self.case["metadata"]
        for key in ("sanitized_addresses", "sanitized_names", "sanitized_credentials",
                    "kept_verbatim", "external_targets"):
            self.assertIn(key, meta)
        for name, node in self.nodes.items():
            with self.subTest(node=name):
                self.assertIn("address", node["metadata"])
                self.assertIn("role", node["metadata"])
        self.assertIn("egress_address", self.entry["metadata"])


def generator():
    spec = importlib.util.spec_from_file_location(
        "make_identity_pool", EXAMPLES / "make_identity_pool.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class IdentityPoolExampleTests(unittest.TestCase):
    NAME = "identity-pool.example.json"

    def setUp(self):
        self.pool = load(self.NAME)
        self.identities = self.pool["identities"]

    def test_pool_header_matches_the_documented_shape(self):
        self.assertEqual(self.pool["schema_version"], 1)
        self.assertEqual(self.pool["kind"], "sing-box-plus-identity-pool")
        self.assertEqual(self.pool["shadowsocks"]["method"], "2022-blake3-aes-128-gcm")
        self.assertEqual(self.pool["count"], len(self.identities))
        self.assertEqual(self.pool["count"], 300)
        self.assertRegex(self.pool["created_at"], r"\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\Z")
        marker = self.pool["_example"]
        self.assertIs(marker["sanitized"], True)
        self.assertTrue((EXAMPLES / marker["generator"]).is_file())

    def test_every_credential_is_marked_synthetic_and_format_exact(self):
        for index, item in enumerate(self.identities, 1):
            name = "u_example_%04d" % index
            with self.subTest(id=name):
                self.assertEqual(item["id"], name)
                self.assertEqual(item["ss"]["name"], name)
                self.assertEqual(item["vless"]["name"], name)
                self.assertEqual(item["status"], "staging")
                secret = item["ss"]["password"]
                self.assertTrue(secret.startswith("NOTAREAL"), "标记必须落在字面量开头")
                self.assertEqual(len(secret), 24)
                self.assertEqual(len(base64.b64decode(secret, validate=True)), 16)
                value = item["vless"]["uuid"]
                self.assertTrue(value.startswith("deadbeef-"))
                self.assertTrue(value.endswith("00000000%04d" % index), "尾号应等于身份编号")
                self.assertEqual(uuid.UUID(value).version, 4)
                self.assertEqual(str(uuid.UUID(value)), value, "应为规范小写写法")
        self.assertEqual(len({i["ss"]["password"] for i in self.identities}), 300)
        self.assertEqual(len({i["vless"]["uuid"] for i in self.identities}), 300)

    def test_pool_file_is_exactly_what_the_generator_produces(self):
        """让 58KB 的文件成为可验证产物：评审者不必逐条读，跑一次比对即可。"""
        module = generator()
        self.assertEqual(module.dumps(module.build()), text(self.NAME))

    def test_generator_is_seeded_and_never_uses_system_entropy(self):
        module = generator()
        self.assertEqual(module.build(), module.build(), "同种子必须可复现")
        self.assertNotEqual(module.build(seed=1), module.build(), "换种子应产生不同结果")
        # 只扫代码：文档字符串里正解释着「为什么不用 secrets」，不该被自己的说明绊倒。
        source = (EXAMPLES / "make_identity_pool.py").read_text(encoding="utf-8")
        docstring = ast.get_docstring(ast.parse(source))
        code = source.replace(docstring, "") if docstring else source
        for forbidden in ("secrets", "urandom", "uuid4", "time.time"):
            self.assertNotIn(forbidden, code,
                             "示例凭据必须不可用：" + forbidden + " 的输出与真实凭据无法区分")
        self.assertIn("random.Random(", code)

    def test_pool_is_not_an_inventory(self):
        with self.assertRaisesRegex(sbpd.InventoryError, "unknown fields"):
            sbpd.validate(load(self.NAME))

    def test_the_renderer_knows_nothing_about_identities(self):
        """职责边界：deploy 不得自持用户与凭据清单，这条要由测试守住而不是靠注释。"""
        source = (ROOT / "sbpd.py").read_text(encoding="utf-8").lower()
        for forbidden in ("identity", "password", "uuid", "shadowsocks", "vless", "credential"):
            self.assertNotIn(forbidden, source, "渲染器出现了身份/凭据概念: " + forbidden)


class DocumentationTests(unittest.TestCase):
    CASE_DOC = ROOT / "docs" / "CASE_MULTI_REGION_CHAIN.md"
    LINK = re.compile(r"\]\(([^)]+)\)")

    def test_readme_lists_every_example_file(self):
        body = (ROOT / "README.md").read_text(encoding="utf-8")
        for name in EXPECTED_FILES:
            with self.subTest(name=name):
                self.assertIn(name, body, "新增示例必须在 README 里有交代")

    def test_readme_states_the_synthetic_credential_rule(self):
        """钉住必须与数据一致的 token，不钉中文句子——散文应当保持可改。"""
        body = (ROOT / "README.md").read_text(encoding="utf-8")
        module = generator()
        for token in (module.PASSWORD_MARKER, module.UUID_MARKER, str(module.SEED)):
            with self.subTest(token=token):
                self.assertIn(token, body)

    def test_documentation_links_resolve(self):
        for source in ((ROOT / "README.md"), self.CASE_DOC):
            for target in self.LINK.findall(source.read_text(encoding="utf-8")):
                if target.startswith(("http://", "https://", "#")):
                    continue
                with self.subTest(source=source.name, target=target):
                    self.assertTrue((source.parent / target.split("#")[0]).exists(),
                                    "断链: " + target)

    def test_case_document_uses_only_documentation_addresses(self):
        for found in IPV4.findall(self.CASE_DOC.read_text(encoding="utf-8")):
            address = ipaddress.ip_address(found)
            self.assertTrue(any(address in net for net in DOC_NETWORKS),
                            "案例文档出现非文档保留地址: " + found)

    def test_case_document_names_no_real_node(self):
        body = self.CASE_DOC.read_text(encoding="utf-8")
        declared = set(load("inventory.case-multi-region-chain.json")["nodes"])
        for name in declared:
            self.assertIn(name, body, "案例文档应交代每个节点: " + name)


if __name__ == "__main__":
    unittest.main()
