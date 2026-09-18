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


ALLOWLIST = """
[reserved]
127.0.0.0/8      # 回环
192.0.2.0/24     # 文档网段

[public]
1.1.1.0/24       # 公共解析器
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
        for kind in ('', 'RSA ', 'OPENSSH ', 'EC '):
            header = '-----' + 'BEGIN ' + kind + 'PRIVATE' + ' KEY-----'
            findings, _ = self.run_scan(self.write('k.txt', header))
            self.assertTrue(any('私钥块' in item for item in findings), kind or '(无类型)')

    def test_accepts_reserved_and_declared_addresses(self):
        findings, stale = self.run_scan(
            self.write('ok.txt', 'loopback 127.0.0.1, doc 192.0.2.5, resolver 1.1.1.1'))
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


class AllowlistHygiene(Base):
    def test_unused_public_entry_is_reported_as_stale(self):
        _, stale = self.run_scan(self.write('ok.txt', 'only 127.0.0.1 here'))
        self.assertEqual(stale, ['1.1.1.0/24'])

    def test_unused_reserved_entry_is_not_stale(self):
        """保留网段按定义不是真实基础设施，允许长期留着。"""
        _, stale = self.run_scan(self.write('ok.txt', 'resolver 1.1.1.1 only'))
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
            self.assertIn(section, ('reserved', 'public'))
            self.assertTrue(reason, str(network))

    def test_the_repository_itself_is_clean(self):
        findings, stale = scan.scan()
        self.assertEqual(findings, [])
        self.assertEqual(stale, [])


if __name__ == '__main__':
    unittest.main(verbosity=2)
