#!/usr/bin/env python3
"""敏感物扫描的契约测试。

这一组存在的理由很具体：上一版扫描器在 2026-09-15 起持续失败了三天而无人察觉，
而它失败与通过的差别只体现在退出码上。所以除了「该报的要报」，这里同样断言
「不该报的不报」与「清单腐烂要报」——一个永远通不过或永远通过的门禁都不是门禁。
"""
from __future__ import annotations

import importlib.util
from pathlib import Path
import shutil
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


scan = load_module('sensitive_scan_under_test', ROOT / 'scripts/sensitive-scan.py')

# 本文件的夹具必须拼装而成，不能写成字面量：否则扫描器会命中自己的测试数据。
# 给扫描器加一条「跳过自己的测试」的豁免更省事，但那会在唯一存放敏感样本的文件上
# 开一个洞——宁可让夹具写起来别扭一点。
UNLISTED_IP = "8.8." + "8.8"
# 域名夹具同理，而且拼接点要选在**每一个片段都不像主机名**的地方。
# 在「子域.」与「其余部分」之间断开是错的：第二个片段自己就是一个合法主机名，
# 而扫描器读的是文件内容，不是拼接结果。把顶级域单独拆出来才安全。
# 连注释也要守这条规矩——把一个反例写进注释，扫描器照样会命中它。
UNLISTED_URL_HOST = "account.example" + "." + "org"
UNLISTED_BARE_HOST = "proxy.somewhere" + "." + "work"
LOOKALIKE_HOST = "github" + ".com.attacker" + "." + "net"
MALFORMED_ENTRY = ".leading.dot" + "." + "com"


ALLOWLIST = """
[reserved]
127.0.0.0/8      # 回环
192.0.2.0/24     # 文档网段

[public]
1.1.1.0/24       # 公共解析器

[domain-reserved]
example.com      # 文档域名

[domain]
github.com       # 依赖的模块路径
"""


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.allowlist = self.tmp / 'allowlist.txt'
        self.allowlist.write_text(ALLOWLIST, encoding='utf-8')

    def write(self, name, text, binary=False):
        path = self.tmp / name
        path.write_bytes(text if binary else text.encode('utf-8'))
        return name, path

    def run_scan(self, *files, allowlist=None):
        return scan.scan(allowlist=allowlist or self.allowlist, files=list(files))


class Detection(Base):
    def test_reports_an_undeclared_public_address(self):
        findings, _ = self.run_scan(self.write('a.txt', 'dial %s first' % UNLISTED_IP))
        self.assertEqual(len(findings), 1)
        self.assertIn(UNLISTED_IP, findings[0])

    def test_reports_a_private_key_block(self):
        body = 'MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC' + 'x' * 20
        for kind in ('', 'RSA ', 'OPENSSH ', 'EC '):
            header = '-----' + 'BEGIN ' + kind + 'PRIVATE' + ' KEY-----'
            findings, _ = self.run_scan(self.write('k.txt', header + '\n' + body))
            self.assertTrue(any('私钥块' in item for item in findings), kind or '(无类型)')

    def test_a_bare_marker_without_a_body_is_not_a_key(self):
        """实测的两类误报：一句「不要提交这个标记」的告诫，和正文写着占位词的夹具。

        两者都不是私钥，而一个天天把它们报成私钥的门禁，很快会被人整条关掉。
        """
        marker = '-----' + 'BEGIN ' + 'PRIVATE' + ' KEY-----'
        warning = '不要提交 %s 这一标记或其后内容。' % marker
        placeholder = '%s\\nSECRET\\n-----END PRIVATE KEY-----' % marker
        for text in (warning, placeholder):
            findings, _ = self.run_scan(self.write('k.txt', text))
            self.assertEqual(findings, [], text[:40])

    def test_accepts_reserved_and_declared_addresses(self):
        findings, stale = self.run_scan(
            self.write('ok.txt', 'loopback 127.0.0.1, doc 192.0.2.5, resolver 1.1.1.1, https://github.com/x'))
        self.assertEqual(findings, [])
        self.assertEqual(stale, [])

    def test_matching_is_per_token_not_per_line(self):
        """旧实现按行过滤：同一行上出现允许地址就把该行整行豁免。"""
        findings, _ = self.run_scan(
            self.write('mixed.txt', 'from 127.0.0.1 to %s on one line' % UNLISTED_IP))
        self.assertEqual(len(findings), 1)
        self.assertIn(UNLISTED_IP, findings[0])

    def test_version_strings_are_not_addresses(self):
        findings, _ = self.run_scan(
            self.write('v.txt', 'sing-box 1.14.0 / go1.26.5 / schema 0.1.0'))
        self.assertEqual(findings, [])

    def test_impossible_octets_are_not_addresses(self):
        findings, _ = self.run_scan(self.write('n.txt', 'build 999.1.1.1 and 1.2.3.400'))
        self.assertEqual(findings, [])

    def test_binary_files_are_skipped(self):
        findings, _ = self.run_scan(
            self.write('b.bin', b'\x00\x01 ' + UNLISTED_IP.encode(), binary=True))
        self.assertEqual(findings, [])

    def test_each_address_is_reported_once_per_file(self):
        findings, _ = self.run_scan(self.write('r.txt', ' '.join([UNLISTED_IP] * 3)))
        self.assertEqual(len(findings), 1)


class DomainDetection(Base):
    """AGENTS.md 把「域名」列在必须脱敏的第一位，而扫描器在 2026-09-19 之前
    一个字符都不查。那次推送前审计发现的四处泄露里有三处是域名。

    难点不在于「能不能找到域名」，而在于**不把标识符当成域名**：
    `server.example.json`、`com.v2ray.core.app.Foo`、`log.Info(` 都长得像。
    下面一半用例盯的是这个方向——一个天天误报的门禁很快会被人加 `|| true`。
    """

    def test_reports_an_undeclared_domain_in_a_url(self):
        findings, _ = self.run_scan(
            self.write('a.md', 'see https://%s/sso' % UNLISTED_URL_HOST))
        self.assertEqual(len(findings), 1)
        self.assertIn(UNLISTED_URL_HOST, findings[0])

    def test_reports_a_bare_undeclared_hostname(self):
        findings, _ = self.run_scan(
            self.write('a.toml', 'host = "%s"' % UNLISTED_BARE_HOST))
        self.assertEqual(len(findings), 1)
        self.assertIn(UNLISTED_BARE_HOST, findings[0])

    def test_an_entry_covers_its_subdomains(self):
        findings, _ = self.run_scan(self.write('a.go', 'import "github.com/x/y"\n// api.github.com'))
        self.assertEqual(findings, [])

    def test_suffix_matching_is_not_fooled_by_a_lookalike(self):
        """github.com.attacker.net 不是 github.com 的子域。"""
        findings, _ = self.run_scan(self.write('a.go', 'https://%s/x' % LOOKALIKE_HOST))
        self.assertEqual(len(findings), 1)
        self.assertIn(LOOKALIKE_HOST, findings[0])

    def test_a_filename_is_not_a_hostname(self):
        """旧的宽正则会把 server.example.json 截成主机名 server.example。"""
        findings, _ = self.run_scan(
            self.write('a.md', 'config/server.example.json 与 cmd/client.rs 与 check.sh'))
        self.assertEqual(findings, [])

    def test_a_dotted_identifier_is_not_a_hostname(self):
        """log.Info( 和安卓包名 com.v2ray.core.app.Foo 都不是主机。"""
        findings, _ = self.run_scan(
            self.write('a.go', 'log.Info("x")\npkg := "com.v2ray.core.app.Activity"'))
        self.assertEqual(findings, [])

    def test_a_version_string_is_not_a_hostname(self):
        findings, _ = self.run_scan(self.write('v.md', 'sing-box 1.14.0 / go1.26.5'))
        self.assertEqual(findings, [])

    def test_an_ipv4_is_not_reported_twice(self):
        """地址走 IP 那条规则，不要在域名规则里再报一次。"""
        findings, _ = self.run_scan(self.write('a.md', 'http://127.0.0.1:8080/x'))
        self.assertEqual(findings, [])


class DomainAllowlistHygiene(Base):
    def test_unused_domain_entry_is_stale(self):
        _, stale = self.run_scan(self.write('a.md', 'only 127.0.0.1 here'))
        self.assertIn('github.com', stale)

    def test_documentation_domain_is_never_stale(self):
        _, stale = self.run_scan(self.write('a.md', 'https://github.com/x'))
        self.assertNotIn('example.com', stale)

    def test_a_redundant_domain_entry_is_refused_at_load_time(self):
        """窄条目被宽条目覆盖时永远拿不到命中，随后会被报成「在仓库中不再出现」——
        一句与事实不符、还会误导人去删错东西的话。所以在加载时就拒掉。
        """
        # 用真实换行拼装，不要写 \n 转义：那会让文件里出现一个由 'n' 与后面的
        # 主机名粘成的未登记域名，而扫描器命中它是完全正确的。
        self.allowlist.write_text(
            '\n'.join(['[domain]', 'github.com  # 宽', 'api.github.com  # 窄，被覆盖', '']),
            encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '已被更宽的'):
            self.run_scan(self.write('a.md', 'x'))

    def test_a_malformed_domain_entry_is_refused(self):
        self.allowlist.write_text('[domain]\n%s  # 理由\n' % MALFORMED_ENTRY, encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '写法不合法'):
            self.run_scan(self.write('a.md', 'x'))


class AllowlistHygiene(Base):
    def test_unused_public_entry_is_reported_as_stale(self):
        _, stale = self.run_scan(self.write('ok.txt', 'only 127.0.0.1 and https://github.com/x'))
        self.assertEqual(stale, ['1.1.1.0/24'])

    def test_unused_reserved_entry_is_not_stale(self):
        """保留网段按定义不是真实基础设施，允许长期留着。"""
        _, stale = self.run_scan(self.write('ok.txt', 'resolver 1.1.1.1 and https://github.com/x'))
        self.assertEqual(stale, [])

    def test_entry_without_a_reason_is_rejected(self):
        self.allowlist.write_text('[public]\n1.1.1.0/24\n', encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '缺少理由'):
            self.run_scan(self.write('a.txt', 'x'))

    def test_entry_before_any_section_is_rejected(self):
        self.allowlist.write_text('1.1.1.0/24  # 无段\n', encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '任何段之前'):
            self.run_scan(self.write('a.txt', 'x'))

    def test_unknown_section_is_rejected(self):
        self.allowlist.write_text('[whatever]\n1.1.1.0/24  # r\n', encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '未知的段名'):
            self.run_scan(self.write('a.txt', 'x'))

    def test_malformed_network_is_rejected(self):
        self.allowlist.write_text('[public]\nnot-an-ip  # r\n', encoding='utf-8')
        with self.assertRaises(scan.ScanError):
            self.run_scan(self.write('a.txt', 'x'))

    def test_empty_allowlist_is_rejected(self):
        self.allowlist.write_text('# 只有注释\n', encoding='utf-8')
        with self.assertRaisesRegex(scan.ScanError, '为空'):
            self.run_scan(self.write('a.txt', 'x'))


class RealRepository(unittest.TestCase):
    def test_the_shipped_allowlist_parses_and_every_entry_has_a_reason(self):
        entries = scan.load_allowlist(scan.ALLOWLIST)
        self.assertTrue(entries)
        for network, section, reason in entries:
            self.assertIn(section, scan.SECTIONS)
            self.assertTrue(reason, str(network))

    def test_the_repository_itself_is_clean(self):
        findings, stale = scan.scan()
        self.assertEqual(findings, [])
        self.assertEqual(stale, [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
