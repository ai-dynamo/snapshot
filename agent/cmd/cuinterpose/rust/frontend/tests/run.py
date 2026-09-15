#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Build CUDA-named providers and test the Rust front end in fresh processes."""

import argparse
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--frontend", type=Path, help="Use an already-built libcuinterpose.so")
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2]
    fixtures = Path(__file__).resolve().parent / "fixtures"
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    env.pop("CUINTERPOSE_TEST_CORE_INITIALIZED", None)
    env.setdefault("CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER", "/usr/bin/gcc")
    if args.frontend:
        frontend = args.frontend.resolve()
    else:
        subprocess.run(
            ["cargo", "build", "--release", "--target", "x86_64-unknown-linux-gnu", "-p", "cuinterpose"],
            cwd=workspace, env=env, check=True,
        )
        target = Path(env.get("CARGO_TARGET_DIR", workspace / "target"))
        if not target.is_absolute():
            target = workspace / target
        frontend = target / "x86_64-unknown-linux-gnu/release/libcuinterpose.so"
    with tempfile.TemporaryDirectory(prefix="cuinterpose-loader-") as directory:
        build = Path(directory)
        shutil.copy2(frontend, build / "libcuinterpose.so")
        compiler = [
            "/usr/bin/gcc", "-std=gnu11", "-O2", "-g", "-Wall", "-Wextra", "-Werror",
            "-fno-omit-frame-pointer", "-fno-optimize-sibling-calls", "-I", str(fixtures),
        ]
        shared = ["-shared", "-fPIC", "-Wl,-Bsymbolic-functions"]
        targets = [
            ("driver.c", "libcuda.so.1", shared + ["-Wl,-soname,libcuda.so.1"]),
            ("runtime.c", "libcudart.so.13", shared + [
                "-Wl,-soname,libcudart.so.13", "-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN"]),
            ("core.c", "libcuinterpose_core.so", shared),
            ("core.c", "bad-core.so", shared + ["-DBAD_CORE_ABI"]),
            ("prefix_core.c", "short-version-core.so", shared + ["-DWRONG_VERSION"]),
            ("prefix_core.c", "short-size-core.so", shared),
            ("next.c", "caller.so", shared + ["-DCALLER", "-ldl"]),
            ("next.c", "following.so", shared + ["-DMARKER=93"]),
            ("constructor.c", "constructor.so", shared + ["-pthread"]),
            ("probe.c", "probe", ["-ldl", "-rdynamic"]),
            ("direct.c", "direct", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
        ]
        for source, output, options in targets:
            subprocess.run(compiler + [str(fixtures / source), "-o", str(build / output)] + options,
                           env=env, check=True)
        env["LD_LIBRARY_PATH"] = str(build)
        env["LD_PRELOAD"] = str(build / "libcuinterpose.so")
        cases = ["direct", "lookup", "queries", "missing", "bindings", "runtime", "local-lifetime", "next",
                 "fork-reentry", "constructor-reentry", "constructor-fork", "constructor-concurrent",
                 "resolver-fork",
                 "missing-core", "bad-core", "short-version-core", "short-size-core"]
        for case in cases:
            case_env = env.copy()
            if case == "constructor-reentry":
                case_env["CUINTERPOSE_TEST_REENTER_CORE"] = "1"
            if case in ("constructor-fork", "constructor-concurrent"):
                case_env["CUINTERPOSE_TEST_CONSTRUCTOR"] = "fork" if case == "constructor-fork" else "create"
            if case == "next":
                case_env["LD_PRELOAD"] += ":" + str(build / "caller.so") + ":" + str(build / "following.so")
            elif case in ("missing-core", "bad-core", "short-version-core", "short-size-core"):
                variant = build / case
                variant.mkdir()
                shutil.copy2(frontend, variant / "libcuinterpose.so")
                if case != "missing-core":
                    shutil.copy2(build / f"{case}.so", variant / "libcuinterpose_core.so")
                case_env["LD_PRELOAD"] = str(variant / "libcuinterpose.so")
            command = [str(build / "direct")] if case == "direct" else [str(build / "probe"), case]
            subprocess.run(command, env=case_env, timeout=20, check=True)
            print(f"PASS {case}", flush=True)
        print(f"{len(cases)} front-end loader cases passed; checkpoint core and GPU behavior not tested.")


if __name__ == "__main__":
    main()
