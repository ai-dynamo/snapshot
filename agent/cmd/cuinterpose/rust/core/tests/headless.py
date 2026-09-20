#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the packaged C frontend and Rust core with headless CUDA fixtures."""

import argparse
import os
from pathlib import Path
import subprocess
import sys
import signal
import resource
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    workspace = Path(__file__).resolve().parents[2]
    parser.add_argument("--artifacts", type=Path,
                        default=workspace.parent / "build",
                        help="Packaged frontend, core, and coordinator (default: ../build)")
    args = parser.parse_args()
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
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
        initialization = [sys.executable, str(Path(__file__).with_name("initialization_suite.py")),
                          "--artifacts", str(artifacts), "--fixtures", str(fixtures)]
        subprocess.run(initialization + ["--prepare-only"], env=environment, check=True)
        subprocess.run(initialization, env=lifecycle_env, check=True)
        subprocess.run([sys.executable, str(Path(__file__).with_name("memory_ipc.py"))],
                       env=lifecycle_env, check=True, timeout=60)
        for mode in ("tracking", "exports", "exhaustion", "access", "shared",
                     "private-released", "no-context", "raw", "unsupported", "checkpoint-entry",
                     "ranges", "accept-exhaustion"):
            subprocess.run([sys.executable, str(Path(__file__).with_name("lifecycle.py")), mode],
                           env=lifecycle_env, check=True, timeout=60)
        failure = subprocess.run(
            [sys.executable, str(Path(__file__).with_name("lifecycle.py")), "retain-release-failure"],
            env=lifecycle_env, capture_output=True, text=True, timeout=60)
        assert failure.returncode == -signal.SIGABRT, failure
        assert "unrecoverable state change" in failure.stderr, failure.stderr
        blocker = temporary / "multicast-block.so"
        subprocess.run([
            "/usr/bin/gcc", "-std=c11", "-Wall", "-Wextra", "-Werror",
            "-shared", "-fPIC", "-pthread", "-o", str(blocker),
            str(Path(__file__).with_name("multicast_block.c")), "-ldl",
        ], env=environment, check=True)
        for mode in ("released", "kind", "access", "failure", "native-address", "tracked-address", "extent", "inflight", "blocking-bind",
                     "create-output", "cached-export", "unsupported"):
            multicast_env = env | {
                "LD_PRELOAD": env["LD_PRELOAD"] + f":{blocker}:{fixtures / 'test/libcuda.so.1'}",
            }
            result = subprocess.run([sys.executable, str(Path(__file__).with_name("multicast.py")), mode],
                                    env=multicast_env, capture_output=True, text=True, timeout=60)
            if mode == "failure":
                assert result.returncode == -signal.SIGABRT, result
                assert "unrecoverable state change" in result.stderr, result.stderr
            else:
                print(result.stdout, end="")
                print(result.stderr, end="", file=sys.stderr)
                result.check_returncode()
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
                assert result.returncode == -signal.SIGABRT, result
                assert "unrecoverable state change" in result.stderr, result.stderr
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
        for mode in ("preinit", "rejected", "exec"):
            case_env = env | {"SNAPSHOT_CONTROL_TIMEOUT_SECONDS": "1"}
            subprocess.run([sys.executable, str(Path(__file__).with_name("fork.py")),
                            mode],
                           env=case_env, check=True, timeout=60)
    print("PASS packaged C frontend/Rust unicast/multicast/carrier/fork; no GPU qualification")


if __name__ == "__main__":
    main()
