#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Test the packaged frontend and core with the installed CUDA driver and two GPUs."""

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
    parser.add_argument("--cuda-include", type=Path,
                        default=Path(os.environ.get("CUDA_INCLUDE", "/opt/cuda/include")))
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2] / "rust"
    fixtures = Path(__file__).resolve().parent / "fixtures"
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    artifacts = args.artifacts.resolve()
    core = artifacts / "libcuinterpose_core.so"
    frontend = artifacts / "libcuinterpose.so"
    for required in (frontend, core, args.cuda_include / "cuda.h"):
        if not required.is_file():
            parser.error(f"required artifact or CUDA header missing: {required}")
    # Wrapper addresses must bind locally even if another preload exports them.
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
        actual = build / "actual"
        actual.mkdir()
        shutil.copy2(frontend, actual / "libcuinterpose.so")
        shutil.copy2(core, actual / "libcuinterpose_core.so")
        compiler = [
            "gcc", "-std=gnu11", "-O2", "-g", "-Wall", "-Wextra", "-Werror",
            "-fno-omit-frame-pointer", "-fno-optimize-sibling-calls",
            "-I", str(args.cuda_include), "-I", str(build),
        ]
        if (artifacts / "core_abi.h").is_file():
            shutil.copy2(artifacts / "core_abi.h", build / "core_abi.h")
        else:
            subprocess.run([
                "cbindgen", "--quiet", "--config", str(workspace / "abi/cbindgen.toml"),
                "--crate", "cuinterpose-abi", "--output", str(build / "core_abi.h"), ".",
            ], cwd=workspace, env=env, check=True)
        shared = ["-shared", "-fPIC", "-Wl,-Bsymbolic-functions"]
        targets = [
            ("scope.c", "scope-dependency.so", shared + ["-DSCOPE_DEPENDENCY"]),
            ("scope.c", "scope.so", shared + [
                "-L" + str(build), "-l:scope-dependency.so", "-Wl,-rpath,$ORIGIN", "-ldl"]),
            ("probe.c", "probe", ["-ldl"]),
            ("direct.c", "direct", ["-l:libcuda.so.1", "-ldl"]),
            ("abi.c", "abi", ["-ldl", "-pthread"]),
            ("init_only.c", "init-only", ["-l:libcuda.so.1"]),
            ("generation_constructor.c", "constructor.so", shared + ["-ldl"]),
        ]
        for source, output, options in targets:
            subprocess.run(compiler + [str(fixtures / source), "-o", str(build / output)] + options,
                           env=env, check=True)
        # Fail if the required GPUs are unavailable. Do not skip the tests.
        subprocess.run([str(build / "probe"), "require-gpus"], env=env, check=True, timeout=60)
        library_path = os.pathsep.join(filter(None, (str(build), env.get("LD_LIBRARY_PATH"))))
        actual_env = env | {"LD_LIBRARY_PATH": library_path,
                            "LD_PRELOAD": str(actual / "libcuinterpose.so"),
                            "SNAPSHOT_CONTROL_DIR": str(actual)}
        subprocess.run([str(build / "abi"), str(core)],
                       env=env | {"SNAPSHOT_CONTROL_DIR": str(actual)}, check=True, timeout=60)
        cases = ["direct", "lookup", "scope", "queries", "runtime", "local-lifetime", "missing-core"]
        for case in cases:
            case_env = actual_env.copy()
            if case == "missing-core":
                variant = build / case
                variant.mkdir()
                shutil.copy2(frontend, variant / "libcuinterpose.so")
                case_env["LD_PRELOAD"] = str(variant / "libcuinterpose.so")
            command = [str(build / "direct")] if case == "direct" else [str(build / "probe"), case]
            subprocess.run(command, env=case_env, timeout=60, check=True)
            print(f"PASS real CUDA loader {case}", flush=True)
        changed_cwd = build / "changed-cwd"
        changed_cwd.mkdir()
        subprocess.run([str(build / "init-only")], env=actual_env, check=True, timeout=60)
        modes = ["init", "init-handle", "init-failure", "relative-preload-chdir", *map(str, range(7)),
                 "tracked-query", "concurrent", "constructor", "fork-before-init", "fork-after-init", "exec",
                 "same-pid-exec", "stale", "stale-concurrent", "existing-file", "existing-symlink",
                 "existing-live", "existing-full", "permissive-umask", "out-of-order",
                 "resolver-startup-failure"]
        for mode in modes:
            relative_preload = mode == "relative-preload-chdir"
            mode_env = actual_env | {"LD_PRELOAD": "./actual/libcuinterpose.so"} if relative_preload else actual_env
            completed = subprocess.run(
                [sys.executable, str(fixtures.parent / "endpoint.py"), mode,
                 str(changed_cwd if relative_preload else build / "constructor.so")],
                cwd=build if relative_preload else None, env=mode_env, timeout=60,
                stderr=subprocess.PIPE if mode == "existing-file" else None, text=True,
            )
            if completed.stderr:
                print(completed.stderr, file=sys.stderr, end="")
            completed.check_returncode()
            if mode == "existing-file":
                assert "bind control socket" in completed.stderr, completed.stderr
                assert f"os error {errno.EADDRINUSE}" in completed.stderr, completed.stderr
        print(f"{len(cases)} real CUDA loader, ABI handshake, and {len(modes) + 1} endpoint cases passed.")


if __name__ == "__main__":
    main()
