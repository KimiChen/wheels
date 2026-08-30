#!/usr/bin/env python3
"""`scripts/lib.sh` 开关变量的三态解析必须真的拒绝非法值。

`M-72`：`${VAR:-default} == 1` 把所有无法识别的取值折叠到「关闭」分支，于是
`SHADOWSOCKS_REQUIRE_AUDIT_TARGET=yes` 这样的笔误等同于操作员显式写了 `=0`，
本应 fail-closed 的门禁被静默放开。这里执行真正的 shell 助手来绑定三态语义。
"""

from __future__ import annotations

from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[1]
LIB = ROOT / "scripts" / "lib.sh"
# 三态解析必须覆盖到的每一个开关；新增开关时一并列进来。
SWITCHES = {
    "SHADOWSOCKS_REQUIRE_AUDIT_TARGET": "1",
    "SHADOWSOCKS_RUN_FUZZ": "0",
    "SHADOWSOCKS_RUST_PLUS_STRICT_FMT": "0",
    "SHADOWSOCKS_RUST_PLUS_NO_DOTENV": "0",
}


def parse(name: str, default: str, value: str | None) -> subprocess.CompletedProcess[str]:
    script = f'source "{LIB}"; require_bool_env {name} {default}'
    environment = {"PATH": "/usr/bin:/bin", "HOME": str(ROOT)}
    if value is not None:
        environment[name] = value
    return subprocess.run(
        ["bash", "-c", script],
        capture_output=True,
        text=True,
        env=environment,
        cwd=ROOT,
    )


class BooleanSwitchTests(unittest.TestCase):
    def test_unset_and_empty_fall_back_to_the_declared_default(self) -> None:
        for name, default in SWITCHES.items():
            for value in (None, ""):
                result = parse(name, default, value)
                self.assertEqual(result.returncode, 0, f"{name}={value!r}: {result.stderr}")
                self.assertEqual(result.stdout.strip(), default, f"{name}={value!r}")

    def test_zero_and_one_pass_through(self) -> None:
        for name, default in SWITCHES.items():
            for value in ("0", "1"):
                result = parse(name, default, value)
                self.assertEqual(result.returncode, 0, f"{name}={value}: {result.stderr}")
                self.assertEqual(result.stdout.strip(), value, f"{name}={value}")

    def test_any_other_value_fails_closed_instead_of_degrading(self) -> None:
        """一个笔误不得等同于显式放弃覆盖面。"""

        for name, default in SWITCHES.items():
            for value in ("yes", "true", "false", "2", "01", " 1", "1 ", "no", "off"):
                result = parse(name, default, value)
                self.assertNotEqual(
                    result.returncode, 0, f"{name}={value!r} 被接受了：{result.stdout!r}"
                )
                self.assertIn(name, result.stderr, f"{name}={value!r} 的错误信息未点名该变量")
                self.assertEqual(result.stdout.strip(), "", f"{name}={value!r} 仍然产出了取值")

    def test_every_switch_is_parsed_through_the_shared_helper(self) -> None:
        """没有哪个开关可以绕过三态解析退回到 `${VAR:-x} == 1`。"""

        for script_name in ("lib.sh", "test.sh", "verify.sh"):
            body = (ROOT / "scripts" / script_name).read_text(encoding="utf-8")
            for name in SWITCHES:
                # 不用 assertNotIn：失败信息会把整份脚本倒灌进输出。
                self.assertFalse(
                    f"${{{name}:-" in body,
                    f"{script_name} 仍在用默认值展开直接判定 {name}",
                )


if __name__ == "__main__":
    unittest.main()
