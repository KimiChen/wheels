#!/usr/bin/env python3
"""公开仓库的敏感物扫描：私钥块与未声明的真实 IPv4。

与上一版的区别有三点，都是为了让清单不会静默腐烂：

1. 按 token 匹配而不是按行过滤。旧实现用 `grep -v` 整行排除，一行里只要出现
   允许清单中的任一片段，同一行上的其他地址就一并被豁免。
2. 允许清单独立成文件，每条强制带理由，并区分「标准保留网段」与「真实公网分配」。
3. `[public]` 段的每一条都必须至少命中一次，否则失败。清单只增不减是它腐烂的方式。

版本号不需要排除：四段点分十进制的模式匹配不到 `1.14.0` 这类三段版本，
旧清单里那一批版本号片段是多余的，去掉后语义更清晰。

扫描范围是 git 跟踪的文件。未跟踪文件（如 .env）本就不进仓库，不在此列。
"""
from __future__ import annotations

import ipaddress
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
ALLOWLIST = ROOT / 'scripts/sensitive-allowlist.txt'
SKIP_NAMES = {'LICENSE', 'THIRD_PARTY_NOTICES.md'}
SKIP_SUFFIXES = ('.lock',)
PRIVATE_KEY = re.compile(r'BEGIN (RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY')
# 前后不接数字或点，避免把更长的点分串截成一个看似合法的地址。
IPV4 = re.compile(r'(?<![0-9.])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(?![0-9.])')


class ScanError(Exception):
    """允许清单本身有问题，与扫描结果无关。"""


def load_allowlist(path):
    """返回 [(network, section, reason)]，顺序即文件顺序。"""
    if not path.is_file():
        raise ScanError('允许清单不存在：' + str(path))
    entries, section = [], None
    for number, raw in enumerate(path.read_text(encoding='utf-8').splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith('#'):
            continue
        if line.startswith('[') and line.endswith(']'):
            section = line[1:-1]
            if section not in ('reserved', 'public'):
                raise ScanError('第 %d 行：未知的段名 %r' % (number, section))
            continue
        if section is None:
            raise ScanError('第 %d 行：条目出现在任何段之前' % number)
        value, separator, reason = line.partition('#')
        if not separator or not reason.strip():
            raise ScanError('第 %d 行：条目缺少理由（%s）' % (number, value.strip()))
        try:
            network = ipaddress.ip_network(value.strip(), strict=False)
        except ValueError as error:
            raise ScanError('第 %d 行：%s' % (number, error)) from None
        entries.append((network, section, reason.strip()))
    if not entries:
        raise ScanError('允许清单为空')
    return entries


def tracked_files():
    result = subprocess.run(['git', '-C', str(ROOT), 'ls-files', '--', '.'],
                            capture_output=True, text=True)
    if result.returncode:
        raise ScanError('无法列出 git 跟踪文件；本扫描要求在工作副本内运行')
    for name in result.stdout.splitlines():
        path = ROOT / name
        if name in SKIP_NAMES or name.endswith(SKIP_SUFFIXES) or not path.is_file():
            continue
        yield name, path


def read_text(path):
    try:
        data = path.read_bytes()
    except OSError:
        return ''
    if b'\0' in data[:8192]:
        return ''
    return data.decode('utf-8', errors='replace')


def scan(allowlist=ALLOWLIST, files=None):
    """files 可注入 [(展示名, 路径)]，供单元测试绕开 git 与真实仓库。"""
    entries = load_allowlist(allowlist)
    hits = {index: 0 for index, _ in enumerate(entries)}
    findings = []
    for name, path in (tracked_files() if files is None else files):
        text = read_text(path)
        if not text:
            continue
        if PRIVATE_KEY.search(text):
            findings.append(name + ' 含私钥块')
        unexplained = set()
        for token in IPV4.findall(text):
            try:
                address = ipaddress.ip_address(token)
            except ValueError:
                continue  # 形如 999.1.1.1，不是地址
            for index, (network, _, _) in enumerate(entries):
                if address in network:
                    hits[index] += 1
                    break
            else:
                unexplained.add(token)
        for token in sorted(unexplained):
            findings.append('%s 含未声明的真实 IPv4 %s'
                            '（改用 192.0.2.0/24 等文档网段，或在 %s 中登记并说明理由）'
                            % (name, token, ALLOWLIST.name))
    stale = [str(network) for index, (network, section, _) in enumerate(entries)
             if section == 'public' and hits[index] == 0]
    return findings, stale


def main():
    try:
        findings, stale = scan()
    except ScanError as error:
        print('允许清单无效：%s' % error, file=sys.stderr)
        return 2
    for finding in findings:
        print('敏感信息疑似命中：%s' % finding, file=sys.stderr)
    for network in stale:
        print('允许清单已陈旧：%s 在仓库中不再出现，请删除该条目' % network, file=sys.stderr)
    if findings or stale:
        print('错误：敏感信息扫描未通过：%d 处命中、%d 条陈旧条目'
              % (len(findings), len(stale)), file=sys.stderr)
        return 1
    print('敏感信息扫描通过。')
    return 0


if __name__ == '__main__':
    sys.exit(main())
