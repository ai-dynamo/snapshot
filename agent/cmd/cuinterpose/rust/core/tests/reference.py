#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Build pinned C fake-driver fixtures and exercise the Rust implementation."""

import argparse
import os
from pathlib import Path
import subprocess
import tempfile

REFERENCE = "21008b50b93a9879a805665e331e777bb93abf49"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixtures", type=Path, help="Reuse an explicitly supplied fixture build directory")
    parser.add_argument("--image", default="snapshot-cuinterpose-check:latest",
                        help="Local build image with CUDA 13.1 headers, gcc, and gtest")
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2]
    repo = workspace.parents[3]
    environment = os.environ.copy()
    environment.pop("LD_PRELOAD", None)
    environment.setdefault("CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER", "/usr/bin/gcc")
    subprocess.run(["cargo", "build", "--workspace", "--release", "--target",
                    "x86_64-unknown-linux-gnu"], cwd=workspace, env=environment, check=True)
    artifacts = workspace / "target/x86_64-unknown-linux-gnu/release"
    with tempfile.TemporaryDirectory(prefix="cuinterpose-reference-") as directory:
        temporary = Path(directory)
        fixtures = args.fixtures.resolve() if args.fixtures else temporary / "build"
        if not args.fixtures:
            archive = subprocess.check_output([
                "git", "archive", REFERENCE, "agent/cmd/cuinterpose",
            ], cwd=repo)
            subprocess.run(["tar", "xf", "-", "-C", str(temporary)],
                           input=archive, check=True)
            fixtures.mkdir()
            subprocess.run([
                "docker", "run", "--runtime=runc", "--rm", "--entrypoint", "bash",
                "--user", f"{os.getuid()}:{os.getgid()}",
                "-v", f"{temporary}:/work", args.image, "-lc",
                "cd /work/agent/cmd/cuinterpose && "
                "make BUILD_DIR=/work/build CUDA_HOME=/usr/local/cuda-13.1 "
                "SANITIZE=address,undefined test",
            ], check=True, timeout=600)
        control = temporary / "control"
        control.mkdir()
        env = environment | {
            "ASAN_OPTIONS": "detect_leaks=0",
            "SNAPSHOT_CONTROL_DIR": str(control),
            "CUINTERPOSE_COORDINATOR": str(artifacts / "cuinterpose-coordinator"),
        }
        subprocess.run([str(fixtures / "test/coordinator_test")], env=env,
                       check=True, timeout=90)
        env["LD_PRELOAD"] = ("/lib/x86_64-linux-gnu/libasan.so.8:"
                             f"{artifacts / 'libcuinterpose.so'}")
        for name in ("state_preload_test", "lifecycle_preload_test"):
            subprocess.run([str(fixtures / "test" / name)], env=env,
                           check=True, timeout=90)
        env["LD_PRELOAD"] += f":{fixtures / 'test/libcuda.so.1'}"
        env["CUINTERPOSE_PARTICIPANT_ID"] = "123456789abcdef0123456789abcdef0"
        constructor = temporary / "generation-constructor.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-o", str(constructor),
            str(Path(__file__).with_name("generation_constructor.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("preinit", "descriptors", "concurrent", "poison", "nested", "carrier",
                     "generation-constructor", "startup-failure"):
            case_env = env | {"SNAPSHOT_CONTROL_TIMEOUT_SECONDS": "1"}
            subprocess.run(["/usr/bin/python3", str(Path(__file__).with_name("fork.py")),
                            mode, str(constructor)],
                           env=case_env, check=True, timeout=60)
    print(f"PASS Rust unicast/fork with C reference {REFERENCE}; no GPU qualification")


if __name__ == "__main__":
    main()
