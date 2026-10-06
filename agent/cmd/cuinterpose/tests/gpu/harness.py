# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Shared GPU test harness.

The test process stays outside the shim and starts worker.py with the shim loaded.
That parent forks WORLD_SIZE CUDA workers, which the harness drives through
coordinator inspection and preparation, the native cuCheckpointProcess* sequence, and
coordinator reconstruction. The harness does not run CRIU or the Go agent.
"""

from __future__ import annotations

import os
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import NamedTuple

from cuda.bindings import driver

import cuda_driver

WORLD_SIZE = 2
COMMAND_TIMEOUT_SECONDS = 60
CHECKPOINT_TIMEOUT_SECONDS = 120
WORKER_TIMEOUT_SECONDS = 240

SOCKET_PREFIX = "cuinterpose-"
STATE_FILENAME = "cuinterpose.state"
SOCKET_DIR_ENV = "CUINTERPOSE_SOCKET_DIR"


class Tools(NamedTuple):
    interposer: Path
    coordinator: Path


class Environment(NamedTuple):
    tools: Tools
    gpus: tuple[str, str]


def visible_gpus() -> tuple[str, str] | None:
    """Return the first two GPU ordinals, or None if fewer are available."""
    cuda_driver.cuda_call(driver.cuInit, 0)
    if int(cuda_driver.cuda_call(driver.cuDeviceGetCount)) < WORLD_SIZE:
        return None
    configured = os.environ.get("CUDA_VISIBLE_DEVICES")
    if configured is None:
        return "0", "1"
    devices = [entry.strip() for entry in configured.split(",") if entry.strip()]
    if len(devices) < WORLD_SIZE or devices[0] == devices[1]:
        return None
    return devices[0], devices[1]
class Workload:
    """One parent with the shim loaded and WORLD_SIZE forked CUDA workers.

    The context manager terminates the process group on exit and attaches output from
    the parent and workers to any raised error, so a failure includes the workers'
    diagnostics.
    """

    def __init__(
        self,
        tmp_path: Path,
        environment: Environment,
        *,
        mode: str,
        carrier_bytes: int,
        seed: int,
    ) -> None:
        if mode not in {"unicast", "multicast"}:
            raise ValueError(mode)
        self.environment = environment
        self.mode = mode
        self.carrier_bytes = carrier_bytes
        self.seed = seed
        self.socket_dir = tmp_path / "sockets"
        self.checkpoint_dir = tmp_path / "checkpoint"
        self.sync_dir = tmp_path / "sync"
        self.store_path = tmp_path / "torch-distributed-store"
        for directory in (self.socket_dir, self.checkpoint_dir, self.sync_dir):
            directory.mkdir()
        self.parent: subprocess.Popen[str] | None = None
        self.child_pids: tuple[int, ...] = ()
        self.output: tuple[str, str] = ("", "")
        self._output_collected = False

    def __enter__(self) -> Workload:
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        if self.parent is not None and (exc is not None or self.parent.poll() is None):
            self._kill(signal.SIGTERM)
        if self.parent is not None and not self._output_collected:
            try:
                self.output = self.parent.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                self._kill(signal.SIGKILL)
                self.output = self.parent.communicate(timeout=10)
            self._output_collected = True
        if exc is not None:
            exc.add_note(self.diagnostics())

    def _kill(self, signum: int) -> None:
        assert self.parent is not None
        try:
            os.killpg(self.parent.pid, signum)
        except ProcessLookupError:
            pass

    def diagnostics(self) -> str:
        parent_pid = self.parent.pid if self.parent is not None else "not started"
        returncode = self.parent.returncode if self.parent is not None else "not started"
        sockets = sorted(path.name for path in self.socket_dir.glob("*.sock"))
        return (
            f"seed: {self.seed}\n"
            f"parent PID/return code: {parent_pid}/{returncode}\n"
            f"forked worker PIDs: {self.child_pids}\n"
            f"control sockets: {sockets}\n"
            f"parent and worker stdout:\n{self.output[0] or ''}\n"
            f"parent and worker stderr:\n{self.output[1] or ''}"
        )

    def start(self) -> None:
        """Start the parent and wait until every worker has loaded the shim and started
        its listener.
        """
        self.parent = self._start_parent()
        self.child_pids = self._wait_for_child_pids()
        self.wait_for_workers("ready")
        for process_id in self.child_pids:
            self.assert_worker_runtime(process_id)

    def coordinate(self, operation: str) -> None:
        command = [str(self.environment.tools.coordinator), operation,
                   "--socket-dir", str(self.socket_dir)]
        if operation != "--inspect":
            command += ["--checkpoint-dir", str(self.checkpoint_dir)]
        for pid in self.child_pids:
            command.extend(["--process", str(pid)])
        environment = os.environ.copy()
        environment.pop("LD_PRELOAD", None)
        result = subprocess.run(command, env=environment, capture_output=True,
                                text=True, timeout=COMMAND_TIMEOUT_SECONDS)
        assert result.returncode == 0, result.stderr

    def finish(self) -> None:
        """Wait for the workers' done markers and a successful parent exit."""
        assert self.parent is not None
        self.wait_for_workers("done")
        self.output = self.parent.communicate(timeout=COMMAND_TIMEOUT_SECONDS)
        self._output_collected = True
        if self.parent.returncode != 0:
            raise RuntimeError(f"parent {self.parent.pid} exited with {self.parent.returncode}")


    def _start_parent(self) -> subprocess.Popen[str]:
        environment = os.environ.copy()
        environment.update(
            {
                "CUDA_VISIBLE_DEVICES": ",".join(self.environment.gpus),
                SOCKET_DIR_ENV: str(self.socket_dir),
                "LD_PRELOAD": str(self.environment.tools.interposer),
                "PYTHONFAULTHANDLER": "1",
                "PYTHONUNBUFFERED": "1",
                "TORCH_SYMMEM_IMPLICIT_POOL": "0",
            }
        )
        if self.mode == "multicast":
            environment.pop("TORCH_SYMM_MEM_DISABLE_MULTICAST", None)
        else:
            environment["TORCH_SYMM_MEM_DISABLE_MULTICAST"] = "1"
        return subprocess.Popen(
            [
                sys.executable,
                "-X",
                "faulthandler",
                "-u",
                str(Path(__file__).with_name("worker.py")),
                str(self.sync_dir),
                str(self.store_path),
                self.mode,
                str(self.carrier_bytes),
                str(self.seed),
            ],
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )

    def _wait_for_child_pids(self) -> tuple[int, ...]:
        assert self.parent is not None
        paths = [self.sync_dir / f"pid-{rank}" for rank in range(WORLD_SIZE)]
        deadline = time.monotonic() + WORKER_TIMEOUT_SECONDS
        while True:
            try:
                pids = tuple(int(path.read_text()) for path in paths)
            except (FileNotFoundError, ValueError):
                pids = ()
            if len(pids) == WORLD_SIZE:
                if len(set(pids)) != WORLD_SIZE:
                    raise AssertionError(f"forked worker PIDs are not unique: {pids}")
                return pids
            if self.parent.poll() is not None:
                raise RuntimeError(
                    f"parent {self.parent.pid} exited before publishing child PIDs "
                    f"with {self.parent.returncode}"
                )
            if time.monotonic() >= deadline:
                raise TimeoutError("timed out waiting for forked worker PIDs")
            time.sleep(0.05)

    def wait_for_workers(self, marker: str) -> None:
        assert self.parent is not None
        expected = [self.sync_dir / f"{marker}-{rank}" for rank in range(WORLD_SIZE)]
        deadline = time.monotonic() + WORKER_TIMEOUT_SECONDS
        while not all(path.exists() for path in expected):
            if self.parent.poll() is not None:
                raise RuntimeError(
                    f"parent {self.parent.pid} exited before workers reached {marker} "
                    f"with {self.parent.returncode}"
                )
            if time.monotonic() >= deadline:
                missing = [str(path) for path in expected if not path.exists()]
                raise TimeoutError(f"timed out waiting for {marker}: {', '.join(missing)}")
            time.sleep(0.05)

    def assert_worker_runtime(self, process_id: int) -> None:
        interposer = self.environment.tools.interposer
        maps = Path(f"/proc/{process_id}/maps").read_text().splitlines()
        mapped_paths = {
            fields[5] for line in maps if len(fields := line.split(maxsplit=5)) == 6
        }
        if str(interposer) not in mapped_paths:
            raise AssertionError(f"{interposer} is not loaded in process {process_id}")
        endpoint = self.socket_dir / f"{SOCKET_PREFIX}{process_id}.sock"
        if not endpoint.is_socket():
            raise AssertionError(f"shim endpoint does not exist: {endpoint}")
