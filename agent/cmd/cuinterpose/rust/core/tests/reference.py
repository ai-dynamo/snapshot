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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixtures", type=Path,
                        help="Reuse prebuilt fixtures (sanitizer linkage is checked)")
    parser.add_argument("--artifacts", type=Path,
                        help="Test an existing packaged frontend, core, and coordinator without rebuilding")
    parser.add_argument("--sanitized", action="store_true",
                        help="Diagnostic ASan/UBSan fixture run; post-fork runtime is not a GPU gate")
    parser.add_argument("--image", default="snapshot-cuinterpose-check:latest",
                        help="Local build image with CUDA 13.1 headers, gcc, and gtest")
    args = parser.parse_args()
    workspace = Path(__file__).resolve().parents[2]
    repo = workspace.parents[3]
    environment = os.environ.copy()
    environment.pop("LD_PRELOAD", None)
    environment.setdefault("CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER", "/usr/bin/gcc")
    if args.artifacts:
        artifacts = args.artifacts.resolve()
    else:
        subprocess.run(["cargo", "build", "--workspace", "--release", "--target",
                        "x86_64-unknown-linux-gnu"], cwd=workspace, env=environment, check=True)
        artifacts = workspace / "target/x86_64-unknown-linux-gnu/release"
    for name in ("libcuinterpose.so", "libcuinterpose_core.so", "cuinterpose-coordinator"):
        if not (artifacts / name).is_file():
            parser.error(f"missing artifact: {artifacts / name}")
    with tempfile.TemporaryDirectory(prefix="cuinterpose-reference-") as directory:
        temporary = Path(directory)
        fixtures = args.fixtures.resolve() if args.fixtures else temporary / "build"
        report_patch = Path(__file__).with_name("json-reports.patch")
        report_patch_hash = hashlib.sha256(report_patch.read_bytes()).hexdigest()
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
                f"SANITIZE={'address,undefined' if args.sanitized else ''} test",
            ], check=True, timeout=600)
            # The original C self-tests run above. Only report assertions are
            # adapted before rebuilding the callers that exercise Rust.
            subprocess.run(["git", "apply", str(report_patch.resolve())],
                           cwd=temporary / "agent/cmd/cuinterpose", check=True)
            subprocess.run([
                "docker", "run", "--runtime=runc", "--rm", "--entrypoint", "bash",
                "--user", f"{os.getuid()}:{os.getgid()}",
                "-v", f"{temporary}:/work", args.image, "-lc",
                "cd /work/agent/cmd/cuinterpose && "
                "make BUILD_DIR=/work/build CUDA_HOME=/usr/local/cuda-13.1 "
                f"SANITIZE={'address,undefined' if args.sanitized else ''} "
                "/work/build/test/coordinator_test /work/build/test/lifecycle_preload_test "
                "/work/build/test/multicast_preload_test",
            ], check=True, timeout=600)
            (fixtures / "rust-json-reports.sha256").write_text(report_patch_hash)
        if (fixtures / "rust-json-reports.sha256").read_text() != report_patch_hash:
            parser.error("fixtures must be rebuilt with the current JSON report assertions")
        for binary in ("coordinator_test", "state_preload_test", "lifecycle_preload_test",
                       "multicast_preload_test", "libcuda.so.1"):
            linkage = subprocess.check_output(["readelf", "-d", str(fixtures / "test" / binary)],
                                             text=True)
            instrumented = "libasan.so" in linkage or "libubsan.so" in linkage
            # The fake provider is not instrumented by the pinned Makefile.
            if binary != "libcuda.so.1":
                assert instrumented == args.sanitized, (binary, linkage)
        print(f"Reference {REFERENCE}: {'sanitizer diagnostic' if args.sanitized else 'unsanitized'} "
              "fixtures verified; no fork/phase/suite retry adapter", flush=True)
        control = temporary / "control"
        control.mkdir()
        env = environment | {
            "ASAN_OPTIONS": "detect_leaks=0",
            "SNAPSHOT_CONTROL_DIR": str(control),
            "CUINTERPOSE_COORDINATOR": str(artifacts / "cuinterpose-coordinator"),
        }
        subprocess.run([str(fixtures / "test/coordinator_test")], env=env,
                       check=True, timeout=90)
        env["LD_PRELOAD"] = ("/lib/x86_64-linux-gnu/libasan.so.8:" if args.sanitized else "") + str(
            artifacts / "libcuinterpose.so")
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
        carrier = temporary / "carrier-faults.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-o", str(carrier),
            str(Path(__file__).with_name("carrier_faults.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("zero", "zero-released", "zero-member", "zero-import", "native", "release-error", "import-failure",
                     "save-copy", "save-sync", "save-unmap", "save-released", "context-failure", "timing",
                     "load-create", "load-map", "load-copy", "load-sync", "load-unmap", "load-cleanup",
                     "save-pending", "load-pending", "retain-release"):
            carrier_env = env | {
                "LD_PRELOAD": env["LD_PRELOAD"] + f":{carrier}:{fixtures / 'test/libcuda.so.1'}",
            }
            result = subprocess.run(
                ["/usr/bin/python3", str(Path(__file__).with_name("carrier.py")), mode],
                env=carrier_env, timeout=60, capture_output=True, text=True)
            print(result.stdout, end="")
            print(result.stderr, end="", file=sys.stderr)
            if mode in ("save-pending", "load-pending"):
                assert result.returncode == 127, result
                assert "CUDA copy completion unknown; terminating without cleanup" in result.stderr
                assert "UNSAFE pending-copy cleanup" not in result.stderr
                print(f"PASS carrier {mode}: fail-stop without cleanup", flush=True)
            else:
                result.check_returncode()
        rpc = temporary / "rpc-threads.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-o", str(rpc),
            str(Path(__file__).with_name("rpc_threads.c")), "-ldl",
        ], env=environment, check=True)
        rpc_constructor = temporary / "rpc-constructor.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-o", str(rpc_constructor),
            str(Path(__file__).with_name("rpc_constructor.c")),
        ], env=environment, check=True)
        for mode in ("startup1", "startup2", "queue", "unicast", "multicast",
                     "constructor0", "constructor1", "constructor2"):
            subprocess.run(["/usr/bin/python3", str(Path(__file__).with_name("rpc.py")),
                            mode, str(rpc_constructor)],
                           env=env | {"LD_PRELOAD": env["LD_PRELOAD"] +
                                      f":{rpc}:{fixtures / 'test/libcuda.so.1'}"},
                           check=True, timeout=60)
        env["LD_PRELOAD"] += f":{fixtures / 'test/libcuda.so.1'}"
        env["CUINTERPOSE_PARTICIPANT_ID"] = "123456789abcdef0123456789abcdef0"
        for mode in ("preinit", "descriptors", "poison", "nested", "carrier", "startup-failure"):
            case_env = env | {"SNAPSHOT_CONTROL_TIMEOUT_SECONDS": "1"}
            subprocess.run(["/usr/bin/python3", str(Path(__file__).with_name("fork.py")),
                            mode],
                           env=case_env, check=True, timeout=60)
    print(f"PASS Rust unicast/multicast/carrier/fork with C reference {REFERENCE}; no GPU qualification")


if __name__ == "__main__":
    main()
