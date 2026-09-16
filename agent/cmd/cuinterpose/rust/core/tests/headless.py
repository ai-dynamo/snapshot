#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the packaged C frontend and Rust core with headless CUDA fixtures."""

import argparse
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    workspace = Path(__file__).resolve().parents[2]
    parser.add_argument("--artifacts", type=Path,
                        default=workspace.parent / "build",
                        help="Packaged frontend, core, and coordinator (default: ../build)")
    parser.add_argument("--pagebroker", action="store_true",
                        help="Also exercise the real daemon (build agent/pagebroker test-allocations first)")
    args = parser.parse_args()
    subprocess.run([sys.executable, str(workspace.parent / "tests/gpu/test_reports.py")],
                   check=True)
    environment = os.environ.copy()
    environment.pop("LD_PRELOAD", None)
    artifacts = args.artifacts.resolve()
    for name in ("libcuinterpose.so", "libcuinterpose_core.so", "cuinterpose-coordinator"):
        if not (artifacts / name).is_file():
            parser.error(f"missing artifact: {artifacts / name}")
    with tempfile.TemporaryDirectory(prefix="cuinterpose-headless-") as directory:
        temporary = Path(directory)
        fixtures = temporary / "build"
        (fixtures / "test").mkdir(parents=True)
        sources = Path(__file__).with_name("fixtures")
        subprocess.run([
            "/usr/bin/gcc", "-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-I", str(sources),
            str(sources / "fake_cuda.c"), "-Wl,-soname,libcuda.so.1",
            "-o", str(fixtures / "test/libcuda.so.1"),
        ], env=environment, check=True)
        subprocess.run([sys.executable, str(workspace.parent / "frontend/tests/run.py"),
                        "--artifacts", str(artifacts)], env=environment, check=True)
        control = temporary / "control"
        control.mkdir()
        env = environment | {
            "SNAPSHOT_CONTROL_DIR": str(control),
            "CUINTERPOSE_COORDINATOR": str(artifacts / "cuinterpose-coordinator"),
        }
        env["LD_PRELOAD"] = str(artifacts / "libcuinterpose.so")
        lifecycle_env = env | {"LD_PRELOAD": env["LD_PRELOAD"] +
                              f":{fixtures / 'test/libcuda.so.1'}"}
        if args.pagebroker:
            for mode in ("shared", "empty", "truncated"):
                subprocess.run(
                    [sys.executable, str(Path(__file__).with_name("pagebroker.py")), mode],
                    env=lifecycle_env | {"FAKE_PAGEBROKER": "1", "CUINTERPOSE_ALLOCATION_STORAGE": "pagebroker"},
                    check=True, timeout=60)
        for mode in ("tracking", "exports", "exhaustion", "access", "shared",
                     "private-released", "no-context", "raw", "unsupported"):
            subprocess.run([sys.executable, str(Path(__file__).with_name("lifecycle.py")), mode],
                           env=lifecycle_env, check=True, timeout=60)
        blocker = temporary / "multicast-block.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-o", str(blocker),
            str(Path(__file__).with_name("multicast_block.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("released", "kind", "access", "failure", "native-address", "tracked-address", "extent", "inflight",
                     "pending-map", "create-output", "cached-export", "unsupported"):
            multicast_env = env | {
                "LD_PRELOAD": env["LD_PRELOAD"] + f":{blocker}:{fixtures / 'test/libcuda.so.1'}",
            }
            subprocess.run([sys.executable, str(Path(__file__).with_name("multicast.py")), mode],
                           env=multicast_env, check=True, timeout=60)
        carrier = temporary / "carrier-pending.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-o", str(carrier),
            str(Path(__file__).with_name("carrier_pending.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("save-copy", "load-copy", "save-pending", "load-pending"):
            carrier_env = env | {
                "LD_PRELOAD": env["LD_PRELOAD"] + f":{carrier}:{fixtures / 'test/libcuda.so.1'}",
            }
            result = subprocess.run(
                [sys.executable, str(Path(__file__).with_name("carrier.py")), mode],
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
        for mode in ("queue", "unicast", "multicast", "constructor0", "constructor1", "constructor2"):
            case_env = env | {"LD_PRELOAD": env["LD_PRELOAD"] +
                              f":{rpc}:{fixtures / 'test/libcuda.so.1'}"}
            if mode.startswith("constructor"):
                case_env["CUINTERPOSE_TEST_STARTUP_FAILURE"] = mode[-1]
            subprocess.run([sys.executable, str(Path(__file__).with_name("rpc.py")),
                            mode, str(rpc_constructor)],
                           env=case_env,
                           check=True, timeout=60)
        env["LD_PRELOAD"] += f":{fixtures / 'test/libcuda.so.1'}"
        env["CUINTERPOSE_PARTICIPANT_ID"] = "123456789abcdef0123456789abcdef0"
        for mode in ("preinit", "descriptors", "poison", "nested"):
            case_env = env | {"SNAPSHOT_CONTROL_TIMEOUT_SECONDS": "1"}
            subprocess.run([sys.executable, str(Path(__file__).with_name("fork.py")),
                            mode],
                           env=case_env, check=True, timeout=60)
    print("PASS packaged C frontend/Rust unicast/multicast/carrier/fork; no GPU qualification")


if __name__ == "__main__":
    main()
