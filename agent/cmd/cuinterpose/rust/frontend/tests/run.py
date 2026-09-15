#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Build CUDA-named providers and test the Rust front end in fresh processes."""

import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
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
    subprocess.run(
        ["cargo", "build", "--release", "--target", "x86_64-unknown-linux-gnu", "-p", "cuinterpose-core"],
        cwd=workspace, env=env, check=True,
    )
    target = Path(env.get("CARGO_TARGET_DIR", workspace / "target"))
    if not target.is_absolute():
        target = workspace / target
    core = target / "x86_64-unknown-linux-gnu/release/libcuinterpose_core.so"
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
            ("prefix_core.c", "short-version-core.so", shared + ["-DWRONG_VERSION"]),
            ("prefix_core.c", "short-size-core.so", shared),
            ("next.c", "caller.so", shared + ["-DCALLER", "-ldl"]),
            ("next.c", "following.so", shared + ["-DMARKER=93"]),
            ("constructor.c", "constructor.so", shared + ["-pthread"]),
            ("plugin.c", "plugin.so", shared),
            ("plugin.c", "libcuda.so.fake", shared),
            ("chain.c", "chain.so", shared + ["-ldl"]),
            ("probe.c", "probe", ["-ldl", "-rdynamic"]),
            ("direct.c", "direct", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("init_only.c", "init-only", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN"]),
        ]
        for source, output, options in targets:
            subprocess.run(compiler + [str(fixtures / source), "-o", str(build / output)] + options,
                           env=env, check=True)
        env["LD_LIBRARY_PATH"] = str(build)
        env["LD_PRELOAD"] = str(build / "libcuinterpose.so")
        cases = ["direct", "lookup", "queries", "missing", "bindings", "runtime", "local-lifetime", "next",
                 "constructor-reentry", "constructor-concurrent",
                 "providers", "identities", "ready-failure", "chain-before", "chain-after",
                 "missing-core", "bad-core", "short-version-core", "short-size-core",
                 "runtime-nested", "identities-nested", "early-plugin", "early-plugin-nested"]
        for case in cases:
            case_env = env.copy()
            if case in ("runtime-nested", "identities-nested", "early-plugin-nested"):
                case_env["CUINTERPOSE_TEST_NESTED_RUNTIME"] = "1"
            if case == "constructor-reentry":
                case_env["CUINTERPOSE_TEST_REENTER_CORE"] = "1"
            if case == "constructor-concurrent":
                case_env["CUINTERPOSE_TEST_CONSTRUCTOR"] = "create"
            if case == "next":
                case_env["LD_PRELOAD"] += ":" + str(build / "caller.so") + ":" + str(build / "following.so")
            elif case in ("early-plugin", "early-plugin-nested"):
                case_env["LD_PRELOAD"] = str(build / "plugin.so") + ":" + case_env["LD_PRELOAD"]
            elif case in ("chain-before", "chain-after"):
                libraries = [str(build / "libcuinterpose.so"), str(build / "chain.so")]
                if case == "chain-before":
                    libraries.reverse()
                # The RTLD_NEXT fixture deliberately tests the global scope.
                libraries.append(str(build / "libcuda.so.1"))
                case_env["LD_PRELOAD"] = ":".join(libraries)
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
        actual = build / "actual"
        actual.mkdir()
        shutil.copy2(frontend, actual / "libcuinterpose.so")
        shutil.copy2(core, actual / "libcuinterpose_core.so")
        constructor = actual / "generation-constructor.so"
        subprocess.run(compiler + [
            str(workspace / "core/tests/generation_constructor.c"), "-o", str(constructor),
        ] + shared + ["-pthread", "-ldl"], env=env, check=True)
        actual_env = env | {"LD_PRELOAD": str(actual / "libcuinterpose.so"),
                            "SNAPSHOT_CONTROL_DIR": str(actual)}
        subprocess.run([str(build / "init-only")], env=actual_env, check=True, timeout=20)
        for mode in ["init", "init-handle", "init-failure", "private", *map(str, range(7)), "tracked-query", "constructor"]:
            subprocess.run([sys.executable, str(fixtures.parent / "endpoint.py"), mode, str(constructor)],
                           env=actual_env, check=True, timeout=20)
        for mode in [*map(str, range(3, 7)), "resolver-startup-failure"]:
            subprocess.run([sys.executable, str(fixtures.parent / "endpoint.py"), mode, str(constructor)],
                           env=actual_env | {"CUINTERPOSE_TEST_NESTED_RUNTIME": "1"},
                           check=True, timeout=20)
        print("19 actual-core endpoint cases passed; no GPU or VMM lifecycle qualification.")


if __name__ == "__main__":
    main()
