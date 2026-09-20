#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Build or run process-isolated backend initialization checks (no GPU)."""

import argparse
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile


def run(command, env):
    # A blocked constructor can leave descendant processes; bound the whole
    # process group, not just the direct test process.
    process = subprocess.Popen(command, env=env, start_new_session=True)
    try:
        assert process.wait(timeout=15) == 0, command
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", required=True, type=Path)
    parser.add_argument("--fixtures", required=True, type=Path)
    parser.add_argument("--prepare-only", action="store_true")
    args = parser.parse_args()
    sources = Path(__file__).resolve().parent
    workspace = sources.parents[1]
    fixtures = args.fixtures.resolve()
    artifacts = args.artifacts.resolve()
    clean = os.environ.copy()
    clean.pop("LD_PRELOAD", None)
    if args.prepare_only:
        fixtures.mkdir(parents=True, exist_ok=True)
        subprocess.run([
            "cbindgen", "--quiet", "--config", "abi/cbindgen.toml",
            "--crate", "cuinterpose-abi", "--output", str(fixtures / "core_abi.h"), ".",
        ], cwd=workspace, env=clean, check=True)
        # Use the minimal driver to isolate initialization scheduling from VMM.
        driver = workspace.parent / "frontend/tests/fixtures/driver.c"
        subprocess.run([
            "/usr/bin/gcc", "-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-Wl,-Bsymbolic-functions,-soname,libcuda.so.1",
            str(driver), "-o", str(fixtures / "libcuda.so.1"), "-ldl",
        ], env=clean, check=True)
        for source, output, extra in (
            ("generation_constructor.c", "generation-constructor.so", ["-shared", "-fPIC"]),
            ("initialization_faults.c", "initialization-faults.so", ["-shared", "-fPIC"]),
            ("abi_race.c", "abi-race", []),
            ("symbol_cache.c", "symbol-cache", []),
        ):
            subprocess.run([
                "/usr/bin/gcc", "-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror",
                "-I", str(fixtures), "-I", "/opt/cuda/include", "-pthread",
                str(sources / source), "-o", str(fixtures / output), "-ldl", *extra,
            ], env=clean, check=True)
        return
    environment = clean | {
        "LD_PRELOAD": str(artifacts / "libcuinterpose.so"),
        "LD_LIBRARY_PATH": str(fixtures),
    }
    with tempfile.TemporaryDirectory(prefix="cuinterpose-symbols-") as control:
        run([str(fixtures / "symbol-cache"), str(artifacts / "libcuinterpose_core.so"),
             str(fixtures / "test/libcuda.so.1")], clean | {"SNAPSHOT_CONTROL_DIR": control})
    count = 0
    for mode, argument in (
        ("cold", "32"), ("fork", "32"),
        ("constructor", str(fixtures / "generation-constructor.so")),
        ("fork-constructor", str(fixtures / "generation-constructor.so")),
    ):
        with tempfile.TemporaryDirectory(prefix="cuinterpose-init-") as control:
            run([sys.executable, str(sources / "initialization.py"), mode, argument],
                environment | {"SNAPSHOT_CONTROL_DIR": control})
        count += 1
    for mode in ("failure-race", "delayed", "recursive", "first-spawn", "second-spawn",
                 "collision", "permissions"):
        with tempfile.TemporaryDirectory(prefix="cuinterpose-init-") as control:
            env = environment | {
                "SNAPSHOT_CONTROL_DIR": control,
                "LD_PRELOAD": environment["LD_PRELOAD"] + ":" + str(fixtures / "initialization-faults.so"),
            }
            if mode == "permissions":
                env["CUINTERPOSE_TEST_CHMOD_FAILURE"] = "1"
            run([sys.executable, str(sources / "initialization_faults.py"), mode], env)
        count += 1
    for _ in range(10):
        with tempfile.TemporaryDirectory(prefix="cuinterpose-abi-") as control:
            run([str(fixtures / "abi-race"), str(artifacts / "libcuinterpose_core.so")],
                clean | {"SNAPSHOT_CONTROL_DIR": control})
            assert not list(Path(control).iterdir()), "ABI exchange created a control endpoint"
    print(f"PASS {count} initialization cases and 10 ABI processes x 32 callers", flush=True)


if __name__ == "__main__":
    main()
