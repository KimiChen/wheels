#!/usr/bin/env python3
"""公开仓库的敏感物扫描：私钥块、未声明的真实 IPv4，以及未声明的域名。

与上一版的区别有三点，都是为了让清单不会静默腐烂：

1. 按 token 匹配而不是按行过滤。旧实现用 `grep -v` 整行排除，一行里只要出现
   允许清单中的任一片段，同一行上的其他地址就一并被豁免。
2. 允许清单独立成文件，每条强制带理由，并区分「标准保留网段」与「真实公网分配」。
3. `[public]` 段的每一条都必须至少命中一次，否则失败。清单只增不减是它腐烂的方式。

版本号不需要排除：四段点分十进制的模式匹配不到 `1.14.0` 这类三段版本，
旧清单里那一批版本号片段是多余的，去掉后语义更清晰。

扫描范围是 git 跟踪的文件。未跟踪文件（如 .env）本就不进仓库，不在此列。
默认扫 sing-box-plus 子树；`--root` 可指向仓库根，用于覆盖同仓的其他项目——
2026-09-19 的一次推送前审计发现，104 个待推文件里有 71 个从未被任何检查看过，
因为本扫描的范围一直钉在自己的子树上。

域名检测是同一次审计补上的：AGENTS.md 把「域名」列在必须脱敏的第一位，
而此前的扫描器一个字符都不查。漏掉的正是最该查的那类——
真实生产 FQDN 与第三方接入方的鉴权端点，它们既不是 IP 也不是私钥块。

域名与「点分标识符」难以机械区分（`client.rs`、`check-sensitive.sh`、`1.14.0`
都长得像域名），所以用两条高精度规则而不是一条宽规则：

1. 出现在 URL 里的 host 一律检查——`https://` 前缀是极强的信号，不会误伤文件名；
2. 裸主机名只认 SAFE_TLDS 里的后缀，该表刻意**不含** sh/rs/py/go/md 这些
   与文件扩展名冲突的顶级域。代价是裸写的 `docs.rs` 查不到（写成 URL 就能查到），
   这是拿召回换精度的自觉取舍：一个天天误报的门禁很快会被人加 `|| true`。
"""
from __future__ import annotations

import argparse
import ipaddress
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
ALLOWLIST = ROOT / 'scripts/sensitive-allowlist.txt'
SKIP_NAMES = {'LICENSE', 'THIRD_PARTY_NOTICES.md'}
SKIP_SUFFIXES = ('.lock',)
SECTIONS = ('reserved', 'public', 'domain-reserved', 'domain', 'domain-exclude')
# 光认 BEGIN 标记会把两类东西报成私钥：一句「不要提交 -----BEGIN PRIVATE KEY-----」
# 的告诫，和正文写着 SECRET 的测试夹具。两者实测都出现在本仓里。
# 真正的 PEM 一定跟着 base64 正文，所以要求标记之后不远处有一段足够长的 base64。
PRIVATE_KEY = re.compile(
    r'BEGIN (?:RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY-----[\s\S]{0,80}?'
    r'[A-Za-z0-9+/]{40}')
# 前后不接数字或点，避免把更长的点分串截成一个看似合法的地址。
IPV4 = re.compile(r'(?<![0-9.])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(?![0-9.])')
# URL 里的 host：协议前缀是强信号，不会误伤文件名。顺带剥掉 userinfo 与端口。
URL_HOST = re.compile(r'https?://(?:[^/@\s]*@)?([A-Za-z0-9._-]+)')
# 裸主机名。SAFE_TLDS 刻意不含 sh/rs/py/go/md/pl 等与文件扩展名冲突的顶级域。
# 这张表刻意很短。每加一个顶级域都要问：它会不会同时是个字段名或方法名？
# 实测踢掉的有 app（安卓包名 com.v2ray.core.app.*）、info（Go 的 log.Info）、
# dev/me/co/top/run/live/site（都是常见标识符后缀）、
# 以及 example/test/invalid/local（它们只出现在文档域名里，本来就要放行，
# 检测它们只会让 `server.example.json` 这类文件名变成误报）。
SAFE_TLDS = ('com', 'net', 'org', 'io', 'work', 'cn', 'xyz', 'cloud', 'gov', 'edu', 'biz')
# 先取**最长**的点分串，再从长到短找「最后一段是已知顶级域」的那个前缀。
#
# 用「后面不跟点+字母」的先行断言更省事，但它会漏掉真东西：一个文档路径形如
# 「目录/主机名.md」时，主机名后面正好跟着 `.md`，断言法直接放过——
# 这恰恰是一次真实审计里唯一漏掉的那一处。反过来，只截到第一个已知顶级域也不行，
# 那样 `server.example.json` 会被当成主机名。取最长串再回退，两个方向都对。
# （连这段注释都不能写出一个能匹配的主机名，否则扫描器会命中自己。）
DOTTED = re.compile(r'(?<![A-Za-z0-9._-])[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?![A-Za-z0-9._-])')
# RFC 2606/6761 保留的顶级域，按定义不是真实基础设施，不必逐条登记。
RESERVED_TLDS = ('example', 'invalid', 'test', 'localhost')


def longest_host(token):
    """从一个点分串里取出主机名，取不到返回 None。"""
    labels = token.split('.')
    for end in range(len(labels), 1, -1):
        if labels[end - 1].lower() in SAFE_TLDS:
            return '.'.join(labels[:end])
    return None


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
            if section not in SECTIONS:
                raise ScanError('第 %d 行：未知的段名 %r' % (number, section))
            continue
        if section is None:
            raise ScanError('第 %d 行：条目出现在任何段之前' % number)
        value, separator, reason = line.partition('#')
        if not separator or not reason.strip():
            raise ScanError('第 %d 行：条目缺少理由（%s）' % (number, value.strip()))
        value = value.strip()
        if section == 'domain-exclude':
            # 路径前缀，不是域名。只跳过**域名**这一项检查：私钥与 IP 在全仓始终有效，
            # 整份文件豁免会在唯一存放大量域名的地方开一个谁都看不见的洞。
            entries.append((value, section, reason.strip()))
            continue
        if section.startswith('domain'):
            if not value or value.strip('.') != value or ' ' in value:
                raise ScanError('第 %d 行：域名条目写法不合法（%s）' % (number, value))
            entries.append((value.lower(), section, reason.strip()))
            continue
        try:
            network = ipaddress.ip_network(value, strict=False)
        except ValueError as error:
            raise ScanError('第 %d 行：%s' % (number, error)) from None
        entries.append((network, section, reason.strip()))
    if not entries:
        raise ScanError('允许清单为空')
    # 冗余的域名条目必须在加载时就拒掉。条目按顺序匹配、首个命中即止，
    # 所以被更宽条目覆盖的窄条目永远拿不到命中，随后会被陈旧检查报成
    # 「在仓库中不再出现」——一句与事实不符、且会误导人去删错东西的话。
    # 只比对真正的域名条目：domain-exclude 装的是路径前缀，拿它当域名比毫无意义。
    domain_values = [(value, index) for index, (value, section, _) in enumerate(entries)
                     if section in ('domain', 'domain-reserved')]
    for value, index in domain_values:
        for other, other_index in domain_values:
            if other_index != index and value.endswith('.' + other):
                raise ScanError('域名条目 %s 已被更宽的 %s 覆盖，请删除它'
                                % (value, other))
    return entries


def tracked_files(root=ROOT):
    result = subprocess.run(['git', '-C', str(root), 'ls-files', '--', '.'],
                            capture_output=True, text=True)
    if result.returncode:
        raise ScanError('无法列出 git 跟踪文件；本扫描要求在工作副本内运行')
    for name in result.stdout.splitlines():
        path = root / name
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


def hostnames(text):
    """从一段文本里取出待检查的主机名，小写、去掉端口与末尾的点。"""
    found = set(URL_HOST.findall(text))
    for token in DOTTED.findall(text):
        host = longest_host(token)
        if host:
            found.add(host)
    result = set()
    for host in found:
        host = host.split(':')[0].strip('.').lower()
        if '.' not in host:
            continue    # localhost、占位的 host 之类，没有信息量
        # 纯数字点分串是 IPv4，交给 IP 那条规则，不要在这里重复报一次。
        if all(part.isdigit() for part in host.split('.')):
            continue
        if host.rsplit('.', 1)[-1] in RESERVED_TLDS:
            continue    # example.com 之外的 RFC 保留域，按定义安全
        result.add(host)
    return result


def domain_allowed(host, allowed):
    """条目匹配自身与其全部子域：github.com 覆盖 api.github.com。"""
    for index, value in allowed:
        if host == value or host.endswith('.' + value):
            return index
    return None


def scan(allowlist=ALLOWLIST, files=None, root=ROOT):
    """files 可注入 [(展示名, 路径)]，供单元测试绕开 git 与真实仓库。"""
    entries = load_allowlist(allowlist)
    domains = [(index, value) for index, (value, section, _) in enumerate(entries)
               if section in ('domain', 'domain-reserved')]
    excluded = [(index, value) for index, (value, section, _) in enumerate(entries)
                if section == 'domain-exclude']
    # 两类条目必须分开遍历：域名条目的值是字符串，拿它去做 `address in network`
    # 会直接抛错。合在一起遍历正是加域名支持时最容易留下的那个坑。
    networks = [(index, value) for index, (value, section, _) in enumerate(entries)
                if not section.startswith('domain')]
    hits = {index: 0 for index, _ in enumerate(entries)}
    findings = []
    for name, path in (tracked_files(root) if files is None else files):
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
            for index, network in networks:
                if address in network:
                    hits[index] += 1
                    break
            else:
                unexplained.add(token)
        for token in sorted(unexplained):
            findings.append('%s 含未声明的真实 IPv4 %s'
                            '（改用 192.0.2.0/24 等文档网段，或在 %s 中登记并说明理由）'
                            % (name, token, ALLOWLIST.name))
        skip = None
        for index, prefix in excluded:
            if name.startswith(prefix):
                hits[index] += 1
                skip = prefix
                break
        for host in ([] if skip else sorted(hostnames(text))):
            index = domain_allowed(host, domains)
            if index is None:
                findings.append('%s 含未声明的域名 %s'
                                '（改用 example.com 等文档域名，或在 %s 中登记并说明理由）'
                                % (name, host, Path(allowlist).name))
            else:
                hits[index] += 1
    # 保留段与文档域名按定义不是真实基础设施，允许长期留着；只有「真实公网」
    # 那两类会腐烂——清单只增不减正是它腐烂的方式。
    stale = [str(value) for index, (value, section, _) in enumerate(entries)
             if section in ('public', 'domain', 'domain-exclude') and hits[index] == 0]
    return findings, stale


def main(argv=None):
    parser = argparse.ArgumentParser(description='公开仓库的敏感物扫描')
    parser.add_argument('--root', type=Path, default=ROOT,
                        help='扫描范围的根目录；给仓库根即覆盖同仓的其他项目')
    parser.add_argument('--allowlist', type=Path, default=None,
                        help='允许清单，缺省为 <root>/scripts/sensitive-allowlist.txt')
    args = parser.parse_args(argv)
    root = args.root.resolve()
    allowlist = args.allowlist or (root / 'scripts/sensitive-allowlist.txt')
    try:
        findings, stale = scan(allowlist=allowlist, root=root)
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
    print('敏感信息扫描通过（范围：%s）。' % root)
    return 0


if __name__ == '__main__':
    sys.exit(main())
