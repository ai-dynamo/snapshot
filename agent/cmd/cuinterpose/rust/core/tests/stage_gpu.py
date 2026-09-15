#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Stage the pinned GPU suite with its JSON report consumer and Rust artifacts."""

import argparse
import ast
import json
from pathlib import Path
import shutil
import subprocess

from reference import REFERENCE


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--cuda-checkpoint", type=Path, required=True)
    args = parser.parse_args()
    args.destination.mkdir(parents=True, exist_ok=False)
    suite = args.destination / "gpu"
    suite.mkdir()
    repo = Path(__file__).resolve().parents[6]
    source = "agent/cmd/cuinterpose/tests/gpu"
    for name in ("harness.py", "worker.py", "cuda_driver.py", "conftest.py",
                 "test_posix.py", "test_multicast.py", "pyproject.toml"):
        contents = subprocess.check_output(["git", "show", f"{REFERENCE}:{source}/{name}"],
                                          cwd=repo)
        (suite / name).write_bytes(contents)
    subprocess.run(["git", "apply", str(Path(__file__).with_name("gpu-json-reports.patch").resolve())],
                   cwd=suite, check=True)

    # Exercise the exact staged parser without importing torch/CUDA on the build
    # host. CUDA workload bodies and their performance assertions are unchanged.
    syntax = ast.parse((suite / "harness.py").read_text())
    function = next(node for node in syntax.body
                    if isinstance(node, ast.FunctionDef) and node.name == "parse_phase_lines")
    namespace = {"json": json}
    exec(compile(ast.Module(body=[function], type_ignores=[]), str(suite / "harness.py"), "exec"),
         namespace)
    parse = namespace["parse_phase_lines"]
    assert parse('noise\n{"phase":"load_allocations","status":"ok","allocation_bytes":123}') == {
        "load_allocations": {"status": "ok", "allocation_bytes": 123}}
    for invalid in ('null', '[]', '{}', '{"phase":"inspect"}',
                    '{"phase":"inspect","status":"ok"} trailing'):
        assert parse(invalid) == {}

    build = args.destination / "build"
    build.mkdir()
    for name in ("libcuinterpose.so", "libcuinterpose_core.so", "cuinterpose-coordinator"):
        shutil.copy2(args.artifacts / name, build / name)
    binary = args.destination / "bin"
    binary.mkdir()
    shutil.copy2(args.cuda_checkpoint, binary / "cuda-checkpoint")
    print(f"Staged {REFERENCE} GPU assertions with JSON reports in {args.destination}")


if __name__ == "__main__":
    main()
