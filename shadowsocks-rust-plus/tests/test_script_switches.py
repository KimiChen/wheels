#!/usr/bin/env python3
"""`scripts/lib.sh` 开关变量的三态解析必须真的拒绝非法值。

`M-72`：`${VAR:-default} == 1` 把所有无法识别的取值折叠到「关闭」分支，于是
`SHADOWSOCKS_REQUIRE_AUDIT_TARGET=yes` 这样的笔误等同于操作员显式写了 `=0`，
本应 fail-closed 的门禁被静默放开。这里执行真正的 shell 助手来绑定三态语义。
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
LIB = ROOT / "scripts" / "lib.sh"
TEST_SCRIPT = ROOT / "scripts" / "test.sh"
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

    def run_load_dotenv(
        self, *, switch_value: str, dotenv: str | None = None
    ) -> subprocess.CompletedProcess[str]:
        """在隔离根目录中运行 lib.sh 的真实初始化路径。"""

        with tempfile.TemporaryDirectory() as directory:
            isolated_root = Path(directory)
            isolated_scripts = isolated_root / "scripts"
            isolated_scripts.mkdir()
            isolated_lib = isolated_scripts / "lib.sh"
            shutil.copy2(LIB, isolated_lib)
            if dotenv is not None:
                (isolated_root / ".env").write_text(dotenv, encoding="utf-8")
            environment = {
                "PATH": "/usr/bin:/bin",
                "HOME": str(isolated_root),
                "SHADOWSOCKS_RUST_PLUS_NO_DOTENV": switch_value,
            }
            return subprocess.run(
                ["bash", "-c", f'source "{isolated_lib}"; printf AFTER\\n'],
                capture_output=True,
                text=True,
                env=environment,
                cwd=isolated_root,
            )

    def test_load_dotenv_rejects_invalid_switch_without_dotenv(self) -> None:
        result = self.run_load_dotenv(switch_value="yes")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHADOWSOCKS_RUST_PLUS_NO_DOTENV", result.stderr)
        self.assertNotIn("AFTER", result.stdout)

    def test_load_dotenv_rejects_invalid_switch_before_importing_dotenv(self) -> None:
        result = self.run_load_dotenv(
            switch_value="yes", dotenv="NOT_ALLOWED=1\n"
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("SHADOWSOCKS_RUST_PLUS_NO_DOTENV", result.stderr)
        self.assertNotIn(".env 不允许的键", result.stderr)
        self.assertNotIn("AFTER", result.stdout)

    def run_coverage_status(
        self,
        *,
        run_audit: int,
        run_integration: int,
        crate_checked: int,
        runtime_available: int,
        runtime_executed: int,
        payload_override: str | None = None,
    ) -> tuple[subprocess.CompletedProcess[str], str | None]:
        with tempfile.TemporaryDirectory() as directory:
            status = Path(directory) / "coverage.json"
            if payload_override is not None:
                status.write_text(payload_override, encoding="utf-8")
                command = f'read_test_coverage_status {shlex.quote(str(status))}'
            else:
                command = (
                    "write_test_coverage_status "
                    f"{shlex.quote(str(status))} {run_audit} {run_integration} "
                    f"{crate_checked} {runtime_available} {runtime_executed}; "
                    f"read_test_coverage_status {shlex.quote(str(status))}"
                )
            result = subprocess.run(
                ["bash", "-c", f'source {shlex.quote(str(LIB))}; {command}'],
                capture_output=True,
                text=True,
                env={"PATH": "/usr/bin:/bin", "HOME": str(ROOT)},
                cwd=ROOT,
            )
            return result, status.read_text(encoding="utf-8") if status.exists() else None

    def test_coverage_status_round_trip_derives_completion_from_execution(self) -> None:
        complete, payload = self.run_coverage_status(
            run_audit=1,
            run_integration=1,
            crate_checked=1,
            runtime_available=1,
            runtime_executed=1,
        )
        self.assertEqual(complete.returncode, 0, complete.stderr)
        self.assertEqual(complete.stdout.strip(), "1")
        self.assertEqual(json.loads(payload or "")["coverage_complete"], 1)

        incomplete, payload = self.run_coverage_status(
            run_audit=1,
            run_integration=0,
            crate_checked=1,
            runtime_available=1,
            runtime_executed=0,
        )
        self.assertEqual(incomplete.returncode, 0, incomplete.stderr)
        self.assertEqual(incomplete.stdout.strip(), "0")
        self.assertEqual(json.loads(payload or "")["coverage_complete"], 0)

    def test_coverage_status_rejects_inconsistent_or_tampered_records(self) -> None:
        payload = json.dumps(
            {
                "schema_version": 1,
                "run_audit": 1,
                "run_integration": 1,
                "auditd_crate_checked": 1,
                "auditd_runtime_available": 1,
                "auditd_runtime_executed": 1,
                "coverage_complete": 0,
            }
        )
        result, _ = self.run_coverage_status(
            run_audit=0,
            run_integration=0,
            crate_checked=0,
            runtime_available=0,
            runtime_executed=0,
            payload_override=payload,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("coverage_complete", result.stderr)

    def test_verify_uses_recorded_coverage_instead_of_policy_switch(self) -> None:
        script = (ROOT / "scripts" / "verify.sh").read_text(encoding="utf-8")
        self.assertIn("--coverage-status", script)
        self.assertIn("read_test_coverage_status", script)
        self.assertNotIn('if [[ "$require_audit_target" == 1 ]]', script)

    def run_fake_test_script(
        self, *, host_os: str, without_audit: bool = False
    ) -> tuple[subprocess.CompletedProcess[str], dict[str, int] | None]:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source"
            protocol = source / "crates" / "shadowsocks-audit-protocol" / "src"
            auditd = source / "crates" / "shadowsocks-auditd"
            protocol.mkdir(parents=True)
            auditd.mkdir(parents=True)
            shutil.copyfile(ROOT / "tests/golden_vectors.json", protocol / "golden_vectors.json")
            (source / "Cargo.toml").write_text("[workspace]\n", encoding="utf-8")
            (protocol.parent / "Cargo.toml").write_text("[package]\nname='p'\n", encoding="utf-8")
            (auditd / "Cargo.toml").write_text("[package]\nname='a'\n", encoding="utf-8")

            fake_bin = root / "bin"
            fake_bin.mkdir()
            for name in ("cargo", "python3"):
                executable = fake_bin / name
                executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                executable.chmod(0o755)
            uname = fake_bin / "uname"
            uname.write_text(f"#!/bin/sh\nprintf '%s\\n' '{host_os}'\n", encoding="utf-8")
            uname.chmod(0o755)
            if host_os != "Linux":
                rustc = fake_bin / "rustc"
                rustc.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
                rustc.chmod(0o755)

            status = root / "coverage.json"
            arguments = [
                "bash",
                str(TEST_SCRIPT),
                "--source",
                str(source),
                "--no-integration",
                "--coverage-status",
                str(status),
            ]
            if without_audit:
                arguments.insert(-2, "--without-audit")
            environment = os.environ.copy()
            environment["PATH"] = f"{fake_bin}{os.pathsep}{environment['PATH']}"
            environment["SHADOWSOCKS_REQUIRE_AUDIT_TARGET"] = "0"
            for name in (
                "SHADOWSOCKS_RUN_FUZZ",
                "SHADOWSOCKS_RUST_PLUS_STRICT_FMT",
                "SHADOWSOCKS_RUST_PLUS_NO_DOTENV",
                "SHADOWSOCKS_TEST_COVERAGE_STATUS_FILE",
            ):
                environment.pop(name, None)
            result = subprocess.run(
                arguments,
                capture_output=True,
                text=True,
                env=environment,
                cwd=ROOT,
            )
            payload = json.loads(status.read_text(encoding="utf-8")) if status.exists() else None
            return result, payload

    def test_test_script_records_runtime_not_run_with_no_integration(self) -> None:
        result, payload = self.run_fake_test_script(host_os="Linux")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIsNotNone(payload)
        self.assertEqual(payload["auditd_crate_checked"], 1)
        self.assertEqual(payload["auditd_runtime_available"], 1)
        self.assertEqual(payload["auditd_runtime_executed"], 0)
        self.assertEqual(payload["coverage_complete"], 0)
        self.assertIn("runtime 未在本次运行执行", result.stdout)

    def test_test_script_records_without_audit_as_incomplete(self) -> None:
        result, payload = self.run_fake_test_script(host_os="Linux", without_audit=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIsNotNone(payload)
        self.assertEqual(payload["run_audit"], 0)
        self.assertEqual(payload["auditd_crate_checked"], 0)
        self.assertEqual(payload["coverage_complete"], 0)

    def test_test_script_records_non_linux_downgrade_without_crate_check(self) -> None:
        result, payload = self.run_fake_test_script(host_os="Darwin")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIsNotNone(payload)
        self.assertEqual(payload["auditd_crate_checked"], 0)
        self.assertEqual(payload["auditd_runtime_available"], 0)
        self.assertEqual(payload["coverage_complete"], 0)
        self.assertIn("未验证", result.stderr)


class CoverageStatusWriterTests(unittest.TestCase):
    """写入器自身的错误处理不能被 `mv` 的语义骗过去。"""

    def write(self, path: Path, *values: str) -> subprocess.CompletedProcess[str]:
        script = f'source "{LIB}"; write_test_coverage_status "{path}" ' + " ".join(values)
        return subprocess.run(
            ["bash", "-c", script],
            capture_output=True,
            text=True,
            env={"PATH": "/usr/bin:/bin", "HOME": str(ROOT)},
            cwd=ROOT,
        )

    def test_an_existing_directory_target_is_rejected_instead_of_silently_moved_into(self) -> None:
        """`mv -f src dir` 把 src 移进 dir 而不失败。

        没有这道检查时写入器会返回 0、宣告成功，而 `$path` 处根本没有文件——
        对一个自称「发布结论依据」的写入器，这是最糟的失败方式。
        """

        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / "status"
            target.mkdir()
            result = self.write(target, "1", "1", "1", "1", "1")
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn("目录", result.stderr)
            self.assertEqual(list(target.iterdir()), [], "不得在目标目录里留下临时文件")

    def test_a_regular_file_target_is_overwritten(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / "status"
            target.write_text("stale", encoding="utf-8")
            result = self.write(target, "1", "1", "1", "1", "1")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(target.read_text(encoding="utf-8"))["coverage_complete"], 1)


if __name__ == "__main__":
    unittest.main()
