#!/usr/bin/env python3
"""从 sing-box-plus 的锁定入口导出计量样例，供主控的假节点夹具消费。

为什么是「导出」而不是把样例复制进本仓库：
`docs/integration-contract.md` §2.3 要求**消费节点侧的 fixtures，不要转抄文档**，
理由是两份定义总会漂移，唯一的问题是什么时候、以及谁先发现。
样例一旦 checked-in，上游改了形状这边不会有任何反应；而经这个导出器走一遍，
上游的 `parse_snapshot` 会先拒绝掉不合法的样例，摘要也会变，锁定用例随即变红。

只读取，不修改上游任何文件。两个模块按**文件路径**显式装载并放进独立的
命名空间，避免与本仓库或标准库里的同名模块互相覆盖。

用法：
    export-node-fixtures.py [--sing-box-plus DIR]
输出：stdout 上的一份 JSON 文档（见末尾 `document` 的结构）。
失败时以非零退出码结束并在 stderr 说明原因——**缺依赖要判失败，不是跳过**。
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

# 要消费的上游文件。计量链路对应 integration-contract.md §3 的第一行。
CONSUMED = [
    "tests/fixtures.py",
    "tests/settlement_model.py",
    "scripts/http_unix.py",
    "internal/userstats/snapshot.go",
    "internal/userstats/quota_endpoint.go",
    "internal/userstats/quota.go",
    # 审计链路。docs/integration-contract.md §3 一直列着这四个文件，而这张清单
    # 从来没有包含它们——于是计量与配额链路有锁，审计链路没有。
    # 不补上的话，JSONL 形状漂移时**没有任何机械门禁会红**，
    # 而出站目标那一页的每一行都建立在那 13 个键上。
    "internal/userstats/audit.go",
    "internal/userstats/audit_file.go",
    "internal/userstats/audit_test.go",
    "internal/userstats/audit_integration_test.go",
]


def load_module(path: Path, name: str):
    """按文件路径装载模块，放进独立命名空间。

    不用 `from fixtures import *`：上游 tests/ 与 scripts/ 下有同名文件，
    依赖搜索顺序会变成自我导入（上游 tests/http_unix.py 的注释里写了这个坑）。
    """
    spec = importlib.util.spec_from_file_location(name, path)
    if spec is None or spec.loader is None:
        raise SystemExit(f"无法装载 {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--sing-box-plus", dest="root", default=None)
    args = parser.parse_args()

    root = Path(args.root) if args.root else Path(__file__).resolve().parents[2] / "sing-box-plus"
    root = root.resolve()
    if not root.is_dir():
        print(f"找不到节点侧仓库：{root}", file=sys.stderr)
        return 2

    missing = [rel for rel in CONSUMED if not (root / rel).is_file()]
    if missing:
        print(f"节点侧缺少应消费的文件：{missing}", file=sys.stderr)
        return 2

    fixtures = load_module(root / "tests" / "fixtures.py", "sbp_fixtures")
    settlement = load_module(root / "tests" / "settlement_model.py", "sbp_settlement_model")

    base: dict[str, Any] = fixtures.snapshot()

    # 交叉自检：样例必须能通过**上游自己的**校验器。
    # 这一步是整个导出链路上唯一能机械捕获 schema 漂移的地方——
    # 上游一改形状，这里先炸，而不是等主控在 M1 第一天拒绝每一份真实快照。
    parsed = settlement.parse_snapshot(fixtures.encode(base))
    if parsed["schema_version"] != settlement.SCHEMA_VERSION:
        print("上游样例与上游 SCHEMA_VERSION 不一致", file=sys.stderr)
        return 3

    # 版本号以**节点侧 Go 常量**为准，不以任何 README 为准。
    # 这里顺手核一次 Python 侧常量与 Go 常量是否一致；不一致要回报上游。
    go_source = (root / "internal" / "userstats" / "snapshot.go").read_text(encoding="utf-8")
    marker = "const SchemaVersion = "
    index = go_source.find(marker)
    if index < 0:
        print("snapshot.go 里找不到 SchemaVersion 常量", file=sys.stderr)
        return 3
    go_schema_version = int(go_source[index + len(marker):].split()[0].split("\n")[0])
    if go_schema_version != settlement.SCHEMA_VERSION:
        print(
            f"节点侧 Go 常量（{go_schema_version}）与 Python 模型（{settlement.SCHEMA_VERSION}）不一致，"
            "以 Go 为准并回报上游",
            file=sys.stderr,
        )
        return 3

    document = {
        "schema_version": go_schema_version,
        "health_keys": sorted(settlement.HEALTH_KEYS),
        "billing_health_keys": sorted(settlement.BILLING_HEALTH_KEYS),
        "counter_fields": list(settlement.COUNTER_FIELDS),
        "base_snapshot": base,
        "consumed": {rel: sha256_file(root / rel) for rel in CONSUMED},
    }
    # 紧凑且确定：Python dict 保持插入顺序，因此同一份上游必然导出同一串字节。
    sys.stdout.write(json.dumps(document, ensure_ascii=False, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
