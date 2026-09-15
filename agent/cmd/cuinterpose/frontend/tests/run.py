#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Test one packaged artifact set against CUDA-named providers in fresh processes."""

import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path,
                        default=Path(__file__).resolve().parents[2] / "build")
    parser.add_argument("--loader-only", action="store_true",
                        help="Test the frontend with its independent mock core")
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2]
    fixtures = Path(__file__).resolve().parent / "fixtures"
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    env.pop("CUINTERPOSE_TEST_CORE_INITIALIZED", None)
    artifacts = args.artifacts.resolve()
    core = artifacts / "libcuinterpose_core.so"
    frontend = artifacts / "libcuinterpose.so"
    # The own-wrapper identity exception is valid only when an earlier preload
    # cannot preempt addresses in the frontend's wrapper inventory.
    exports = subprocess.check_output(
        ["nm", "-D", "--defined-only", "--format=posix", str(frontend)], text=True,
    )
    functions = {fields[0] for line in exports.splitlines()
                 if len(fields := line.split()) >= 2 and fields[1] == "T"}
    relocations = subprocess.check_output(["readelf", "-rW", str(frontend)], text=True)
    for line in relocations.splitlines():
        fields = line.split()
        if len(fields) >= 5:
            assert fields[4].split("@", 1)[0] not in functions, (
                f"frontend function reference is preemptible: {line}"
            )
    print("PASS frontend function references bind locally", flush=True)
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
                "-Wl,-soname,libcudart.so.13", "-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("core.c", "libcuinterpose_core.so", shared),
            ("core.c", "bad-core.so", shared + ["-DBAD_CORE_ABI"]),
            ("core.c", "bad-size-core.so", shared + ["-DBAD_CORE_SIZE"]),
            ("constructor.c", "constructor.so", shared + ["-pthread"]),
            ("plugin.c", "plugin.so", shared),
            ("plugin.c", "libcuda.so.fake", shared),
            ("probe.c", "probe", ["-ldl", "-rdynamic"]),
            ("direct.c", "direct", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("init_only.c", "init-only", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN"]),
        ]
        for source, output, options in targets:
            subprocess.run(compiler + [str(fixtures / source), "-o", str(build / output)] + options,
                           env=env, check=True)
        env["LD_LIBRARY_PATH"] = str(build)
        env["LD_PRELOAD"] = str(build / "libcuinterpose.so")
        cases = ["direct", "lookup", "queries", "missing", "bindings", "runtime", "local-lifetime",
                 "constructor-reentry", "constructor-concurrent",
                 "providers", "identities", "ready-failure",
                 "missing-core", "bad-core", "bad-size-core",
                 "runtime-nested", "early-plugin", "early-plugin-nested"]
        for case in cases:
            case_env = env.copy()
            if case in ("runtime-nested", "early-plugin-nested"):
                case_env["CUINTERPOSE_TEST_NESTED_RUNTIME"] = "1"
            if case == "constructor-reentry":
                case_env["CUINTERPOSE_TEST_REENTER_CORE"] = "1"
            if case == "constructor-concurrent":
                case_env["CUINTERPOSE_TEST_CONSTRUCTOR"] = "create"
            if case in ("early-plugin", "early-plugin-nested"):
                case_env["LD_PRELOAD"] = str(build / "plugin.so") + ":" + case_env["LD_PRELOAD"]
            elif case in ("missing-core", "bad-core", "bad-size-core"):
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
        if args.loader_only:
            return
        actual = build / "actual"
        actual.mkdir()
        shutil.copy2(frontend, actual / "libcuinterpose.so")
        shutil.copy2(core, actual / "libcuinterpose_core.so")
        constructor = actual / "generation-constructor.so"
        subprocess.run(compiler + [
            str(workspace / "tests/headless/generation_constructor.c"), "-o", str(constructor),
        ] + shared + ["-pthread", "-ldl"], env=env, check=True)
        actual_env = env | {"LD_PRELOAD": str(actual / "libcuinterpose.so"),
                            "SNAPSHOT_CONTROL_DIR": str(actual)}
        subprocess.run([str(build / "init-only")], env=actual_env, check=True, timeout=20)
        modes = ["init", "init-handle", "init-failure", "private", *map(str, range(7)),
                 "tracked-query", "fork", "constructor"]
        for mode in modes:
            subprocess.run([sys.executable, str(fixtures.parent / "endpoint.py"), mode, str(constructor)],
                           env=actual_env, check=True, timeout=20)
        subprocess.run([sys.executable, str(fixtures.parent / "endpoint.py"),
                        "resolver-startup-failure", str(constructor)],
                       env=actual_env | {"CUINTERPOSE_TEST_NESTED_RUNTIME": "1"},
                       check=True, timeout=20)
        print(f"{len(modes) + 1} actual-core endpoint cases passed; no GPU or VMM lifecycle qualification.")


if __name__ == "__main__":
    main()
