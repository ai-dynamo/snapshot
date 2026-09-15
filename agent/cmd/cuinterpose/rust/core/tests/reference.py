#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Build pinned C fake-driver fixtures and exercise the Rust implementation."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile

REFERENCE = "21008b50b93a9879a805665e331e777bb93abf49"
COORDINATOR_HEADER_SHA256 = "6ab9e2760582610425d37b6040075625707436f33e73a8b9a3bb796e9a9c2734"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixtures", type=Path,
                        help="Reuse prebuilt fixtures without applying/verifying the launch adapter")
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
        launch_test = temporary / "coordinator-launch-test"
        subprocess.run([
            "/usr/bin/g++", "-std=c++17", "-Wall", "-Wextra", "-Werror", "-pthread",
            str(Path(__file__).with_name("coordinator_launch_test.cc")),
            "-o", str(launch_test),
        ], env=environment, check=True)
        subprocess.run([str(launch_test)], env=environment, check=True, timeout=10)
        fixtures = args.fixtures.resolve() if args.fixtures else temporary / "build"
        if not args.fixtures:
            archive = subprocess.check_output([
                "git", "archive", REFERENCE, "agent/cmd/cuinterpose",
            ], cwd=repo)
            subprocess.run(["tar", "xf", "-", "-C", str(temporary)],
                           input=archive, check=True)
            tests = temporary / "agent/cmd/cuinterpose/tests"
            header = tests / "coordinator_driver.h"
            original = header.read_bytes()
            if hashlib.sha256(original).hexdigest() != COORDINATOR_HEADER_SHA256:
                raise RuntimeError("Pinned coordinator header changed; refusing test overlay")
            source = original.decode()
            include = "#include <vector>\n"
            launch = "  pid_t child = fork();\n"
            if source.count(include) != 1 or source.count(launch) != 1:
                raise RuntimeError("Expected exactly one coordinator launch site")
            header.write_text(source.replace(include, include + '#include "coordinator_launch.h"\n')
                              .replace(launch, "  pid_t child = cuinterpose_test::fork_coordinator();\n"))
            adapter = Path(__file__).with_name("coordinator_launch.h").read_bytes()
            (tests / "coordinator_launch.h").write_bytes(adapter)
            print(f"Reference {REFERENCE}: TEST-ONLY coordinator launch adapter v1 "
                  f"(sha256={hashlib.sha256(adapter).hexdigest()}); "
                  "fork EAGAIN retry deadline=5s, no CUDA/phase/suite retries", flush=True)
            fixtures.mkdir()
            subprocess.run([
                "docker", "run", "--runtime=runc", "--rm", "--entrypoint", "bash",
                "--user", f"{os.getuid()}:{os.getgid()}",
                "-v", f"{temporary}:/work", args.image, "-lc",
                "cd /work/agent/cmd/cuinterpose && "
                "make BUILD_DIR=/work/build CUDA_HOME=/usr/local/cuda-13.1 "
                "SANITIZE=address,undefined test",
            ], check=True, timeout=600)
        else:
            print("WARNING: --fixtures reuses prebuilt binaries; the coordinator-launch "
                  "adapter cannot be applied or verified. Unadapted binaries can fail "
                  "on fork EAGAIN. The default fresh-build path is authoritative.",
                  file=sys.stderr, flush=True)
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
        for name in ("state_preload_test", "lifecycle_preload_test", "multicast_preload_test"):
            subprocess.run([str(fixtures / "test" / name)], env=env,
                           check=True, timeout=90)
        blocker = temporary / "multicast-block.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-o", str(blocker),
            str(Path(__file__).with_name("multicast_block.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("released", "kind", "access", "failure", "native-address", "inflight",
                     "pending-map", "create-output"):
            multicast_env = env | {
                "LD_PRELOAD": env["LD_PRELOAD"] + f":{blocker}:{fixtures / 'test/libcuda.so.1'}",
            }
            subprocess.run(["/usr/bin/python3", str(Path(__file__).with_name("multicast.py")), mode],
                           env=multicast_env, check=True, timeout=60)
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
    print(f"PASS Rust unicast/multicast/fork with C reference {REFERENCE}; no GPU qualification")


if __name__ == "__main__":
    main()
