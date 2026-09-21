# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Shared pieces of the GPU tests.

The test process never loads the shim. It launches one interposed *parent*
Python process (``worker.py``) that forks ``WORLD_SIZE`` CUDA workers, and then
drives a checkpoint from the outside exactly as the snapshot agent would:
``cuinterpose-coordinator --prepare``, the native ``cuCheckpointProcess*``
sequence, ``cuinterpose-coordinator --restore``.
"""

from __future__ import annotations

import os
import signal
import socket
import subprocess
import sys
import time
from multiprocessing.reduction import send_handle
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
CONTROL_DIR_ENV = "SNAPSHOT_CONTROL_DIR"


class Tools(NamedTuple):
    interposer: Path
    coordinator: Path


class Environment(NamedTuple):
    tools: Tools
    gpus: tuple[str, str]


def visible_gpus() -> tuple[str, str] | None:
    """The two GPU ordinals the workers use, or None when fewer are available."""
    configured = os.environ.get("CUDA_VISIBLE_DEVICES")
    if configured is None:
        cuda_driver.cuda_call(driver.cuInit, 0)
        if int(cuda_driver.cuda_call(driver.cuDeviceGetCount)) < WORLD_SIZE:
            return None
        return "0", "1"
    devices = [entry.strip() for entry in configured.split(",") if entry.strip()]
    if len(devices) < WORLD_SIZE or devices[0] == devices[1]:
        return None
    return devices[0], devices[1]
class Workload:
    """One interposed parent process with ``WORLD_SIZE`` forked CUDA workers.

    Use as a context manager: on the way out it terminates the process group,
    collects the parent's and workers' output, and attaches it to any error
    that is propagating, so a failure shows what the workers saw.
    """

    def __init__(
        self,
        tmp_path: Path,
        environment: Environment,
        *,
        mode: str,
        carrier_bytes: int,
        seed: int,
        admission_only: bool = False,
    ) -> None:
        if mode not in {"unicast", "multicast"}:
            raise ValueError(mode)
        self.environment = environment
        self.mode = mode
        self.carrier_bytes = carrier_bytes
        self.seed = seed
        self.admission_only = admission_only
        self.control_dir = tmp_path / "control"
        self.checkpoint_dir = tmp_path / "checkpoint"
        self.sync_dir = tmp_path / "sync"
        self.store_path = tmp_path / "torch-distributed-store"
        for directory in (self.control_dir, self.checkpoint_dir, self.sync_dir):
            directory.mkdir()
        self.parent: subprocess.Popen[str] | None = None
        self.child_pids: tuple[int, ...] = ()
        self.output: tuple[str, str] = ("", "")
        self._output_collected = False
        self._externals: list[cuda_driver.ExternalAllocation] = []
        self._restore_channels = [socket.socketpair() for _ in range(WORLD_SIZE)]

    def __enter__(self) -> Workload:
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        try:
            if self.parent is not None and (exc is not None or self.parent.poll() is None):
                self._kill(signal.SIGTERM)
            if self.parent is not None and not self._output_collected:
                try:
                    self.output = self.parent.communicate(timeout=10)
                except subprocess.TimeoutExpired:
                    self._kill(signal.SIGKILL)
                    self.output = self.parent.communicate(timeout=10)
                self._output_collected = True
        finally:
            cuda_driver.destroy_external_allocations(self._externals)
            for sender, receiver in self._restore_channels:
                sender.close()
                receiver.close()
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
        sockets = sorted(path.name for path in self.control_dir.glob("*.sock"))
        return (
            f"seed: {self.seed}\n"
            f"parent PID/return code: {parent_pid}/{returncode}\n"
            f"forked worker PIDs: {self.child_pids}\n"
            f"control sockets: {sockets}\n"
            f"parent and worker stdout:\n{self.output[0] or ''}\n"
            f"parent and worker stderr:\n{self.output[1] or ''}"
        )

    def start(self) -> None:
        """Start the parent, wait until every worker is ready, and check the shim
        is loaded and listening in each of them."""
        self._externals = cuda_driver.create_external_allocations(WORLD_SIZE, 1)
        self.parent = self._start_parent(
            tuple(allocation.fd for allocation in self._externals),
            tuple(receiver.fileno() for _, receiver in self._restore_channels),
        )
        for _, receiver in self._restore_channels:
            receiver.close()
        self.child_pids = self._wait_for_child_pids()
        self.wait_for_workers("ready")
        # Workers have checked that foreign imports are rejected.
        cuda_driver.destroy_external_allocations(self._externals)
        for process_id in self.child_pids:
            self.assert_worker_runtime(process_id)

    def coordinate(self, operation: str) -> None:
        command = [str(self.environment.tools.coordinator), operation,
                   "--checkpoint-dir", str(self.checkpoint_dir),
                   "--control-dir", str(self.control_dir)]
        for pid in self.child_pids:
            command.extend(["--process", str(pid)])
        environment = os.environ.copy()
        environment.pop("LD_PRELOAD", None)
        result = subprocess.run(command, env=environment, capture_output=True,
                                text=True, timeout=COMMAND_TIMEOUT_SECONDS)
        assert result.returncode == 0, result.stderr

    def hand_fresh_imports(self) -> None:
        """Give each worker a fresh foreign FD to verify rejection after restore."""
        self._externals = cuda_driver.create_external_allocations(WORLD_SIZE, 0x40)
        for rank, (sender, _) in enumerate(self._restore_channels):
            send_handle(sender, self._externals[rank].fd, self.child_pids[rank])

    def finish(self) -> None:
        """Wait for the workers' done markers and a clean parent exit."""
        assert self.parent is not None
        self.wait_for_workers("done")
        self.output = self.parent.communicate(timeout=COMMAND_TIMEOUT_SECONDS)
        self._output_collected = True
        if self.parent.returncode != 0:
            raise RuntimeError(f"parent {self.parent.pid} exited with {self.parent.returncode}")


    def _start_parent(
        self, raw_fds: tuple[int, ...], restore_fds: tuple[int, ...]
    ) -> subprocess.Popen[str]:
        environment = os.environ.copy()
        environment.update(
            {
                "CUDA_VISIBLE_DEVICES": ",".join(self.environment.gpus),
                CONTROL_DIR_ENV: str(self.control_dir),
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
                *(str(fd) for fd in raw_fds),
                *(str(fd) for fd in restore_fds),
                str(self.sync_dir),
                str(self.store_path),
                self.mode,
                str(self.carrier_bytes),
                str(self.seed),
                "admission-only" if self.admission_only else "checkpoint",
            ],
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
            pass_fds=raw_fds + restore_fds,
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
        endpoint = self.control_dir / f"{SOCKET_PREFIX}{process_id}.sock"
        if not endpoint.is_socket():
            raise AssertionError(f"shim endpoint does not exist: {endpoint}")
