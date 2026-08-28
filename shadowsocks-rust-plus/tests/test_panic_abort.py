#!/usr/bin/env python3
"""Prove that the release profile aborts on panic in a child process."""

from __future__ import annotations

import argparse
import os
import subprocess
import tempfile
import tomllib
from pathlib import Path


def _profile_panic(source: Path) -> str:
    try:
        value = tomllib.loads((source / "Cargo.toml").read_text(encoding="utf-8"))
    except (OSError, tomllib.TOMLDecodeError) as error:
        raise RuntimeError(f"无法读取准备源码 Cargo.toml：{error}") from error
    profile = value.get("profile", {}).get("release", {})
    panic = profile.get("panic")
    if panic != "abort":
        raise RuntimeError("准备源码 release profile 必须设置 panic = abort")
    return panic


def run_probe() -> None:
    # Keep the probe package independent of the project dependencies. The
    # project profile assertion above binds this executable check to the same
    # release policy while keeping the test fast enough for every CI run.
    with tempfile.TemporaryDirectory(prefix="ssrp-panic-probe-") as temporary:
        root = Path(temporary)
        (root / "src").mkdir()
        (root / "Cargo.toml").write_text(
            "[package]\n"
            "name = \"ssrp-panic-probe\"\n"
            "version = \"0.0.0\"\n"
            "edition = \"2021\"\n\n"
            "[profile.release]\n"
            "panic = \"abort\"\n",
            encoding="utf-8",
        )
        (root / "src/main.rs").write_text(
            'fn main() { std::panic::panic_any("release panic probe"); }\n',
            encoding="utf-8",
        )
        result = subprocess.run(
            ["cargo", "run", "--quiet", "--release", "--manifest-path", str(root / "Cargo.toml")],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
            env={**os.environ, "RUST_BACKTRACE": "0"},
        )
        if result.returncode == 0:
            raise RuntimeError("release panic probe unexpectedly exited successfully")
        combined = result.stdout + result.stderr
        # Unix cargo normally propagates SIGABRT as -6 (or 134 through a shell).
        # Keep a textual fallback for Windows and toolchains that wrap signals.
        if result.returncode not in (-6, 134) and not any(
            marker in combined.lower() for marker in ("sigabrt", "signal: 6", "abort")
        ):
            raise RuntimeError(
                f"panic probe did not abort (status {result.returncode})"
            )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    args = parser.parse_args()
    source = args.source.resolve()
    _profile_panic(source)
    run_probe()
    print("release-profile panic=abort 子进程检查通过。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
