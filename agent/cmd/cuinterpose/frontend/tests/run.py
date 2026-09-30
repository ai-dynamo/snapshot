#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Test one packaged artifact set against CUDA-named providers in fresh processes."""

import argparse
import errno
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
    workspace = Path(__file__).resolve().parents[2] / "rust"
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
            "-fno-omit-frame-pointer", "-fno-optimize-sibling-calls",
        ]
        subprocess.run([
            "cbindgen", "--quiet", "--config", str(workspace / "abi/cbindgen.toml"),
            "--crate", "cuinterpose-abi", "--output", str(build / "core_abi.h"), ".",
        ], cwd=workspace, env=env, check=True)
        shared = ["-shared", "-fPIC", "-Wl,-Bsymbolic-functions"]
        targets = [
            ("driver.c", "libcuda.so.1", shared + ["-Wl,-soname,libcuda.so.1"]),
            ("runtime.c", "libcudart.so.13", shared + [
                "-Wl,-soname,libcudart.so.13", "-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("runtime.c", "libcudart.so.11.0", shared + [
                "-DLEGACY_RUNTIME", "-Wl,-soname,libcudart.so.11.0", "-L" + str(build),
                "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("runtime.c", "libcudart.so.14", shared + [
                "-DNO_RUNTIME_VERSION", "-Wl,-soname,libcudart.so.14", "-L" + str(build),
                "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("core.c", "libcuinterpose_core.so", shared + ["-pthread"]),
            ("core.c", "bad-core.so", shared + ["-DBAD_CORE_ABI", "-pthread"]),
            ("core.c", "bad-size-core.so", shared + ["-DBAD_CORE_SIZE", "-pthread"]),
            ("constructor.c", "constructor.so", shared + ["-pthread"]),
            ("failures.c", "failures.so", shared + ["-ldl"]),
            ("plugin.c", "plugin.so", shared),
            ("plugin.c", "libcuda.so.fake", shared),
            ("scope.c", "scope-dependency.so", shared + ["-DSCOPE_DEPENDENCY"]),
            ("scope.c", "scope.so", shared + [
                "-L" + str(build), "-l:scope-dependency.so", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("probe.c", "probe", ["-ldl", "-rdynamic", "-pthread"]),
            ("direct.c", "direct", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("init_only.c", "init-only", ["-L" + str(build), "-l:libcuda.so.1", "-Wl,-rpath,$ORIGIN"]),
        ]
        for source, output, options in targets:
            includes = (
                ["-I", str(build), "-I", "/opt/cuda/include"]
                if source == "core.c"
                else ["-I", str(fixtures)]
            )
            subprocess.run(compiler + includes + [
                str(fixtures / source), "-o", str(build / output),
            ] + options,
                           env=env, check=True)
        env["LD_LIBRARY_PATH"] = str(build)
        env["LD_PRELOAD"] = str(build / "libcuinterpose.so")
        cases = ["direct", "lookup", "scope", "resolver-bootstrap", "queries", "missing", "bindings",
                 "runtime", "runtime-12", "runtime-legacy", "runtime-unknown-version",
                 "runtime-version-error", "runtime-version-dependency",
                 "local-lifetime", "constructor-reentry", "constructor-concurrent", "concurrent",
                 "providers", "identities", "ready-failure",
                 "missing-core", "bad-core", "bad-size-core",
                 "runtime-nested", "early-plugin", "early-plugin-nested",
                 "backend-race", "backend-late-winner", "backend-failure", "retention-failure"]
        for case in cases:
            case_env = env.copy()
            if case in ("runtime-nested", "early-plugin-nested"):
                case_env["CUINTERPOSE_TEST_NESTED_RUNTIME"] = "1"
            if case == "runtime-12":
                case_env["CUINTERPOSE_TEST_RUNTIME_VERSION"] = "12000"
            if case == "runtime-unknown-version":
                case_env["CUINTERPOSE_TEST_RUNTIME_VERSION"] = "0"
            if case == "runtime-version-error":
                case_env["CUINTERPOSE_TEST_RUNTIME_VERSION_ERROR"] = "1"
            if case == "constructor-reentry":
                case_env["CUINTERPOSE_TEST_REENTER_CORE"] = "1"
            if case == "constructor-concurrent":
                case_env["CUINTERPOSE_TEST_CONSTRUCTOR"] = "create"
            if case == "resolver-bootstrap":
                case_env["CUINTERPOSE_TEST_CONSTRUCTOR"] = "lookup"
            if case == "concurrent":
                case_env["CUINTERPOSE_TEST_CONCURRENT_CORE"] = "1"
            if case in ("early-plugin", "early-plugin-nested"):
                case_env["LD_PRELOAD"] = str(build / "plugin.so") + ":" + case_env["LD_PRELOAD"]
            elif case in ("backend-race", "backend-late-winner", "backend-failure", "retention-failure"):
                case_env["LD_PRELOAD"] += ":" + str(build / "failures.so")
            elif case in ("missing-core", "bad-core", "bad-size-core"):
                variant = build / case
                variant.mkdir()
                shutil.copy2(frontend, variant / "libcuinterpose.so")
                if case != "missing-core":
                    shutil.copy2(build / f"{case}.so", variant / "libcuinterpose_core.so")
                case_env["LD_PRELOAD"] = str(variant / "libcuinterpose.so")
            command = [str(build / "direct")] if case == "direct" else [str(build / "probe"), case]
            if case == "runtime-12":
                command = [str(build / "probe"), "runtime"]
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
            str(fixtures / "generation_constructor.c"), "-o", str(constructor),
        ] + shared + ["-pthread", "-ldl"], env=env, check=True)
        actual_env = env | {"LD_PRELOAD": str(actual / "libcuinterpose.so"),
                            "SNAPSHOT_CONTROL_DIR": str(actual)}
        changed_cwd = build / "changed-cwd"
        changed_cwd.mkdir()
        subprocess.run([str(build / "init-only")], env=actual_env, check=True, timeout=20)
        modes = ["init", "init-handle", "init-failure", "relative-preload-chdir", "private", *map(str, range(7)),
                 "tracked-query", "concurrent", "constructor", "fork-before-init", "fork-after-init", "exec",
                 "same-pid-exec", "stale", "stale-concurrent", "existing-file", "existing-symlink",
                 "existing-live", "existing-full", "permissive-umask"]
        for mode in modes:
            relative_preload = mode == "relative-preload-chdir"
            mode_env = actual_env | {"LD_PRELOAD": "./actual/libcuinterpose.so"} if relative_preload else actual_env
            completed = subprocess.run(
                [sys.executable, str(fixtures.parent / "endpoint.py"), mode,
                 str(changed_cwd if relative_preload else constructor)],
                cwd=build if relative_preload else None, env=mode_env, timeout=20,
                stderr=subprocess.PIPE if mode == "existing-file" else None, text=True,
            )
            if completed.stderr:
                print(completed.stderr, file=sys.stderr, end="")
            completed.check_returncode()
            if mode == "existing-file":
                assert "bind control socket" in completed.stderr, completed.stderr
                assert f"os error {errno.EADDRINUSE}" in completed.stderr, completed.stderr
        subprocess.run([sys.executable, str(fixtures.parent / "endpoint.py"),
                        "resolver-startup-failure", str(constructor)],
                       env=actual_env | {"CUINTERPOSE_TEST_NESTED_RUNTIME": "1"},
                       check=True, timeout=20)
        print(f"{len(modes) + 1} actual-core endpoint cases passed; no GPU or VMM lifecycle qualification.")


if __name__ == "__main__":
    main()
