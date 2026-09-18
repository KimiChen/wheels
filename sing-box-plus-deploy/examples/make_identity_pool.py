#!/usr/bin/env python3
"""生成 identity-pool.example.json：300 条合成身份，用于说明真实部署的规模与形状。

本工具（sbpd.py）不消费这个文件，也不认识用户与凭据——身份归属属于 manager，节点配置由
私有执行器渲染。这里只是把「私有执行器读进去的是什么形状」摆出来，好让案例文档能引用它。

**刻意使用固定种子的 random，而不是 secrets。** 这与平常的直觉相反，理由是这些值必须*不可用*：
secrets 的输出与真实凭据在统计上不可区分，恰是公开示例最不该有的性质。公开种子喂伪随机数
生成器，任何人一条命令即可重算，由构造决定它毫无保密价值，也使得「这份文件确实是合成的」
可以靠重跑比对来证明。Python 文档明确说 random 不适用于安全用途——这里正是要这个性质。

标记直接出现在字面量本身，而不是解码之后：
  - 密码前 6 字节固定为 base64 "NOTAREAL" 的原文，base64 按 3 字节一组编码，
    因此每条密码都以可读的 NOTAREAL 开头，同时仍是合法的 16 字节 base64。
  - UUID 前 4 字节固定为 deadbeef，末 2 字节是编号的 BCD，经 uuid.UUID(version=4)
    构造以保证 version 与 variant 合规。

牺牲的只是「前导字节均匀随机」——也就是让假值与真密钥无法区分的那个性质，而那正是要故意
破坏的。长度、字母表、填充、解码字节数、UUID 版本与变体全部与真实值一致，任何解析器都视之
为格式良好的凭据。熵的降低无关紧要，因为这些值永远不可用于任何真实部署。
"""
import argparse
import base64
import json
import random
import sys
import uuid


SEED = 20260918
COUNT = 300
METHOD = "2022-blake3-aes-128-gcm"
POOL_ID = "deadbeef-0000-4000-8000-000000000000"
CREATED_AT = "2026-01-01T00:00:00Z"
PASSWORD_MARKER = "NOTAREAL"
UUID_MARKER = "deadbeef"
# 6 字节恰好编码成 8 个 base64 字符，标记因此落在字面量开头而不会被切碎。
_MARKER_BYTES = base64.b64decode(PASSWORD_MARKER)


def _bytes(rng, count):
    """用 getrandbits 而非 randbytes：跨 CPython 版本稳定，且不需要 3.9 以上。"""
    return rng.getrandbits(8 * count).to_bytes(count, "big")


def password(rng):
    return base64.b64encode(_MARKER_BYTES + _bytes(rng, 16 - len(_MARKER_BYTES))).decode()


def identity_uuid(rng, index):
    raw = (bytes.fromhex(UUID_MARKER) + _bytes(rng, 6) + b"\x00\x00\x00\x00"
           + int("%04d" % index, 16).to_bytes(2, "big"))
    return str(uuid.UUID(bytes=raw, version=4))


def build(seed=SEED, count=COUNT):
    rng = random.Random(seed)
    identities = []
    for index in range(1, count + 1):
        name = "u_example_%04d" % index
        identities.append({
            "id": name,
            "status": "staging",
            "ss": {"name": name, "password": password(rng)},
            "vless": {"name": name, "uuid": identity_uuid(rng, index)},
        })
    return {
        "schema_version": 1,
        "kind": "sing-box-plus-identity-pool",
        "pool_id": POOL_ID,
        "created_at": CREATED_AT,
        "count": count,
        "shadowsocks": {"method": METHOD},
        "_example": {
            "sanitized": True,
            "generator": "make_identity_pool.py",
            "seed": seed,
            "password_marker": PASSWORD_MARKER,
            "uuid_marker": UUID_MARKER,
            "note": ("合成示例：全部凭据由固定种子的伪随机数生成器重新生成，与任何真实部署无关，"
                     "不可用于生产。本工具不读取本文件；身份归属属于 manager，节点配置由私有执行器渲染。"),
        },
        "identities": identities,
    }


def dumps(pool):
    """每条身份单独一行：评审者翻 diff 时每一行都能看到 NOTAREAL 与 deadbeef。"""
    head = json.dumps({k: v for k, v in pool.items() if k != "identities"},
                      ensure_ascii=False, indent=2)
    rows = [json.dumps(item, ensure_ascii=False, separators=(", ", ": "))
            for item in pool["identities"]]
    body = ",\n".join("    " + row for row in rows)
    return head[:-2] + ',\n  "identities": [\n' + body + "\n  ]\n}\n"


def main(argv=None):
    parser = argparse.ArgumentParser(description="生成合成身份池示例")
    parser.add_argument("--output", help="写入文件；默认输出到标准输出，便于直接比对")
    args = parser.parse_args(argv)
    text = dumps(build())
    if args.output:
        with open(args.output, "w", encoding="utf-8") as stream:
            stream.write(text)
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
