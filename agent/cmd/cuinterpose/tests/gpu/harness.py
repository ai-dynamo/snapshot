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

import ctypes
import os
import queue
import re
import signal
import socket
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from multiprocessing.reduction import send_handle
from pathlib import Path
from typing import NamedTuple

from cuda.bindings import driver

WORLD_SIZE = 2
COMMAND_TIMEOUT_SECONDS = 60
CHECKPOINT_TIMEOUT_SECONDS = 120
WORKER_TIMEOUT_SECONDS = 240

# Names pinned to protocol.h.
SOCKET_PREFIX = "cuinterpose-"
STATE_FILENAME = "cuinterpose.state"
CONTROL_DIR_ENV = "SNAPSHOT_CONTROL_DIR"
PARTICIPANT_ID_ENV = "SNAPSHOT_PARTICIPANT_ID"
# 32 lowercase hex digits; the shim accepts it as the parent's identity.
PARENT_PARTICIPANT_ID = "a" * 32

# Logical handles carry this tag in the top 16 bits (interpose.c).
LOGICAL_HANDLE_TAG = 0xD94D000000000000
LOGICAL_HANDLE_TAG_MASK = 0xFFFF000000000000

POSIX_FD_HANDLE_TYPE = (
    driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
)

PHASE_LINE = re.compile(
    r"^cuinterpose-coordinator phase=(?P<phase>\S+) status=(?P<status>\S+)"
    r"(?P<rest>(?: \S+=\S+)*)$"
)


class Tools(NamedTuple):
    interposer: Path
    coordinator: Path


class Environment(NamedTuple):
    tools: Tools
    gpus: tuple[str, str]
    launch_job_fds: tuple[int, ...]


# --- thin wrappers over cuda.bindings -----------------------------------------


def cuda_call(function, *arguments):
    status, *outputs = function(*arguments)
    if status != driver.CUresult.CUDA_SUCCESS:
        raise RuntimeError(f"{function.__name__} failed: {status.name} ({int(status)})")
    if not outputs:
        return None
    if len(outputs) == 1:
        return outputs[0]
    return tuple(outputs)


def assert_no_current_context(process: str) -> None:
    status, context = driver.cuCtxGetCurrent()
    if status == driver.CUresult.CUDA_ERROR_NOT_INITIALIZED:
        return
    if status != driver.CUresult.CUDA_SUCCESS:
        raise RuntimeError(f"cuCtxGetCurrent failed: {status.name} ({int(status)})")
    if int(context) != 0:
        raise AssertionError(f"{process} has a current CUDA context")


def allocation_properties(device) -> driver.CUmemAllocationProp:
    properties = driver.CUmemAllocationProp()
    properties.type = driver.CUmemAllocationType.CU_MEM_ALLOCATION_TYPE_PINNED
    properties.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
    properties.location.id = int(device)
    properties.requestedHandleTypes = POSIX_FD_HANDLE_TYPE
    return properties


def allocation_granularity(properties: driver.CUmemAllocationProp) -> int:
    return int(
        cuda_call(
            driver.cuMemGetAllocationGranularity,
            properties,
            driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM,
        )
    )


def round_up(size: int, granularity: int) -> int:
    return (size + granularity - 1) // granularity * granularity


def assert_handle_namespace(handle, logical: bool, stage: str) -> None:
    tagged = int(handle) & LOGICAL_HANDLE_TAG_MASK == LOGICAL_HANDLE_TAG
    if tagged != logical:
        expected = "logical" if logical else "raw"
        raise AssertionError(f"{stage}: handle {int(handle):#x} is not {expected}")


def map_allocation(handle, size: int, device) -> int:
    address = int(cuda_call(driver.cuMemAddressReserve, size, size, 0, 0))
    mapped = False
    try:
        cuda_call(driver.cuMemMap, address, size, 0, handle, 0)
        mapped = True
        access = driver.CUmemAccessDesc()
        access.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
        access.location.id = int(device)
        access.flags = driver.CUmemAccess_flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE
        cuda_call(driver.cuMemSetAccess, address, size, [access], 1)
    except Exception:
        if mapped:
            cuda_call(driver.cuMemUnmap, address, size)
        cuda_call(driver.cuMemAddressFree, address, size)
        raise
    return address


def write_bytes(address: int, expected: bytes) -> None:
    source = ctypes.create_string_buffer(expected)
    cuda_call(driver.cuMemcpyHtoD, address, ctypes.addressof(source), len(expected))


def assert_bytes(address: int, expected: bytes, stage: str) -> None:
    actual = ctypes.create_string_buffer(len(expected))
    cuda_call(driver.cuMemcpyDtoH, ctypes.addressof(actual), address, len(expected))
    if actual.raw != expected:
        raise AssertionError(f"{stage}: got {actual.raw!r}, expected {expected!r}")


def destroy_mapped_allocation(address: int, size: int, handle) -> None:
    cuda_call(driver.cuMemUnmap, address, size)
    cuda_call(driver.cuMemAddressFree, address, size)
    cuda_call(driver.cuMemRelease, handle)


# --- allocations made outside the shim ----------------------------------------


class ExternalAllocation(NamedTuple):
    """A POSIX-shareable allocation owned by the (uninterposed) test process.

    Workers import its descriptor raw, which is exactly the "untracked import"
    the coordinator must refuse to checkpoint while it is alive.
    """

    device: driver.CUdevice
    context: driver.CUcontext
    address: int
    size: int
    handle: driver.CUmemGenericAllocationHandle
    fd: int


def create_external_allocations(byte_base: int) -> list[ExternalAllocation]:
    cuda_call(driver.cuInit, 0)
    allocations: list[ExternalAllocation] = []
    try:
        for rank in range(WORLD_SIZE):
            allocations.append(_create_external_allocation(rank, byte_base))
    except Exception:
        destroy_external_allocations(allocations)
        raise
    return allocations


def _create_external_allocation(rank: int, byte_base: int) -> ExternalAllocation:
    device = cuda_call(driver.cuDeviceGet, rank)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, device)
    handle = None
    address = 0
    try:
        cuda_call(driver.cuCtxPushCurrent, context)
        try:
            properties = allocation_properties(device)
            size = allocation_granularity(properties)
            handle = cuda_call(driver.cuMemCreate, size, properties, 0)
            address = map_allocation(handle, size, device)
            write_bytes(address, bytes([byte_base + rank]) * 32)
            fd = int(
                cuda_call(
                    driver.cuMemExportToShareableHandle, handle, POSIX_FD_HANDLE_TYPE, 0
                )
            )
        except Exception:
            if address:
                destroy_mapped_allocation(address, size, handle)
            elif handle is not None:
                cuda_call(driver.cuMemRelease, handle)
            raise
        finally:
            cuda_call(driver.cuCtxPopCurrent)
    except Exception:
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)
        raise
    return ExternalAllocation(device, context, address, size, handle, fd)


def destroy_external_allocations(allocations: list[ExternalAllocation]) -> None:
    while allocations:
        allocation = allocations.pop()
        os.close(allocation.fd)
        cuda_call(driver.cuCtxPushCurrent, allocation.context)
        try:
            destroy_mapped_allocation(allocation.address, allocation.size, allocation.handle)
        finally:
            cuda_call(driver.cuCtxPopCurrent)
            cuda_call(driver.cuDevicePrimaryCtxRelease, allocation.device)


# --- environment --------------------------------------------------------------


def launch_job_fds() -> tuple[int, ...] | None:
    """Descriptors of the cuda-checkpoint launch-job file, or None when the
    process was not started through ``cuda-checkpoint --launch-job``."""
    job_file = os.environ.get("CUDA_CHECKPOINT_JOB_FILE")
    if not job_file or not Path(job_file).is_file():
        return None
    match = re.fullmatch(r"/proc/self/fd/([0-9]+)", job_file)
    if match is None:
        return ()
    descriptor = int(match.group(1))
    os.fstat(descriptor)
    return (descriptor,)


def require_launch_job() -> tuple[int, ...]:
    fds = launch_job_fds()
    if fds is None:
        raise RuntimeError(
            "CUDA_CHECKPOINT_JOB_FILE is missing; run through cuda-checkpoint --launch-job"
        )
    return fds


def visible_gpus() -> tuple[str, str] | None:
    """The two GPU ordinals the workers use, or None when fewer are available."""
    configured = os.environ.get("CUDA_VISIBLE_DEVICES")
    if configured is None:
        cuda_call(driver.cuInit, 0)
        if int(cuda_call(driver.cuDeviceGetCount)) < WORLD_SIZE:
            return None
        return "0", "1"
    devices = [entry.strip() for entry in configured.split(",") if entry.strip()]
    if len(devices) < WORLD_SIZE or devices[0] == devices[1]:
        return None
    return devices[0], devices[1]


def measure_h2d_gbps(device_ordinal: int, nbytes: int, repeats: int = 3) -> float:
    """Best of ``repeats`` synchronous pinned host-to-device copies, in GB/s
    (10**9 bytes per second, the unit of the coordinator's ``gb_per_s``)."""
    cuda_call(driver.cuInit, 0)
    device = cuda_call(driver.cuDeviceGet, device_ordinal)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, device)
    cuda_call(driver.cuCtxPushCurrent, context)
    try:
        destination = cuda_call(driver.cuMemAlloc, nbytes)
        source = cuda_call(driver.cuMemHostAlloc, nbytes, 0)
        try:
            best = 0.0
            for attempt in range(repeats + 1):
                start = time.perf_counter()
                cuda_call(driver.cuMemcpyHtoD, destination, source, nbytes)
                elapsed = time.perf_counter() - start
                if attempt:  # the first copy warms up the link
                    best = max(best, nbytes / 1e9 / elapsed)
            return best
        finally:
            cuda_call(driver.cuMemFreeHost, source)
            cuda_call(driver.cuMemFree, destination)
    finally:
        cuda_call(driver.cuCtxPopCurrent)
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


# --- the coordinator ----------------------------------------------------------


@dataclass
class CoordinatorRun:
    status: int
    out: str
    err: str
    phases: dict[str, dict[str, str]] = field(default_factory=dict)

    @property
    def failed(self) -> str:
        return f"coordinator exited {self.status}\nstdout:\n{self.out}\nstderr:\n{self.err}"


def parse_phase_lines(stdout: str) -> dict[str, dict[str, str]]:
    phases: dict[str, dict[str, str]] = {}
    for line in stdout.splitlines():
        match = PHASE_LINE.match(line.strip())
        if match is None:
            continue
        fields = {"status": match.group("status")}
        for pair in match.group("rest").split():
            key, _, value = pair.partition("=")
            fields[key] = value
        phases[match.group("phase")] = fields
    return phases


def run_coordinator(
    coordinator: Path,
    operation: str,
    checkpoint_dir: Path,
    control_dir: Path,
    process_ids: tuple[int, ...],
) -> CoordinatorRun:
    command = [
        str(coordinator),
        operation,
        "--proc-root",
        "",
        "--checkpoint-dir",
        str(checkpoint_dir),
        "--control-dir",
        str(control_dir),
    ]
    for process_id in process_ids:
        command.extend(["--process", str(process_id), str(process_id)])
    environment = os.environ.copy()
    environment.pop("LD_PRELOAD", None)
    result = subprocess.run(
        command,
        env=environment,
        check=False,
        capture_output=True,
        text=True,
        timeout=COMMAND_TIMEOUT_SECONDS,
    )
    return CoordinatorRun(
        result.returncode, result.stdout, result.stderr, parse_phase_lines(result.stdout)
    )


# --- the native checkpoint ----------------------------------------------------


def _expect_state(process_id: int, expected) -> None:
    actual = cuda_call(driver.cuCheckpointProcessGetState, process_id)
    if actual != expected:
        raise AssertionError(
            f"CUDA process {process_id} is {actual.name}, expected {expected.name}"
        )


def _native_checkpoint(process_ids: tuple[int, ...]) -> None:
    cuda_call(driver.cuInit, 0)
    running = driver.CUprocessState.CU_PROCESS_STATE_RUNNING
    locked = driver.CUprocessState.CU_PROCESS_STATE_LOCKED
    checkpointed = driver.CUprocessState.CU_PROCESS_STATE_CHECKPOINTED
    for process_id in process_ids:
        _expect_state(process_id, running)

    lock_arguments = driver.CUcheckpointLockArgs()
    lock_arguments.timeoutMs = COMMAND_TIMEOUT_SECONDS * 1000
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessLock, process_id, lock_arguments)
    for process_id in process_ids:
        _expect_state(process_id, locked)

    checkpoint_arguments = driver.CUcheckpointCheckpointArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessCheckpoint, process_id, checkpoint_arguments)
    for process_id in process_ids:
        _expect_state(process_id, checkpointed)

    restore_arguments = driver.CUcheckpointRestoreArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessRestore, process_id, restore_arguments)
    for process_id in process_ids:
        _expect_state(process_id, locked)

    unlock_arguments = driver.CUcheckpointUnlockArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessUnlock, process_id, unlock_arguments)
    for process_id in process_ids:
        _expect_state(process_id, running)


def native_checkpoint(process_ids: tuple[int, ...]) -> None:
    """Lock, checkpoint, restore, and unlock the workers through the driver's
    checkpoint API, with a timeout so a wedged driver fails the test instead
    of hanging it."""
    outcomes: queue.Queue[Exception | None] = queue.Queue(maxsize=1)

    def run() -> None:
        try:
            _native_checkpoint(process_ids)
        except Exception as error:  # noqa: BLE001 -- forwarded to the test thread
            outcomes.put(error)
        else:
            outcomes.put(None)

    threading.Thread(target=run, daemon=True).start()
    try:
        outcome = outcomes.get(timeout=CHECKPOINT_TIMEOUT_SECONDS)
    except queue.Empty as error:
        raise TimeoutError(
            f"native CUDA checkpoint exceeded {CHECKPOINT_TIMEOUT_SECONDS} seconds"
        ) from error
    if outcome is not None:
        raise outcome


# --- the interposed workload --------------------------------------------------


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
        hold_raw_import: bool = False,
    ) -> None:
        if mode not in {"unicast", "multicast"}:
            raise ValueError(mode)
        self.environment = environment
        self.mode = mode
        self.carrier_bytes = carrier_bytes
        self.seed = seed
        self.hold_raw_import = hold_raw_import
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
        self._externals: list[ExternalAllocation] = []
        self._restore_channels = [socket.socketpair() for _ in range(WORLD_SIZE)]

    # -- lifecycle -------------------------------------------------------------

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
            destroy_external_allocations(self._externals)
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

    # -- steps -----------------------------------------------------------------

    def start(self) -> None:
        """Start the parent, wait until every worker is ready, and check the shim
        is loaded and listening in each of them."""
        self._externals = create_external_allocations(1)
        self.parent = self._start_parent(
            tuple(allocation.fd for allocation in self._externals),
            tuple(receiver.fileno() for _, receiver in self._restore_channels),
        )
        for _, receiver in self._restore_channels:
            receiver.close()
        self.child_pids = self._wait_for_child_pids()
        self.wait_for_workers("ready")
        # The workers have imported and released (or are holding) their raw
        # imports; the creator side can go away either way.
        destroy_external_allocations(self._externals)
        for process_id in (self.parent.pid, *self.child_pids):
            self.assert_worker_runtime(process_id)

    def prepare(self) -> CoordinatorRun:
        return run_coordinator(
            self.environment.tools.coordinator,
            "--prepare",
            self.checkpoint_dir,
            self.control_dir,
            self.child_pids,
        )

    def restore(self) -> CoordinatorRun:
        return run_coordinator(
            self.environment.tools.coordinator,
            "--restore",
            self.checkpoint_dir,
            self.control_dir,
            self.child_pids,
        )

    def native_checkpoint(self) -> None:
        native_checkpoint(self.child_pids)

    def hand_fresh_imports(self) -> None:
        """Give every worker a brand-new raw descriptor to import after restore."""
        self._externals = create_external_allocations(0x40)
        for rank, (sender, _) in enumerate(self._restore_channels):
            send_handle(sender, self._externals[rank].fd, self.child_pids[rank])

    def resume(self) -> None:
        (self.sync_dir / "continue").touch()

    def finish(self) -> None:
        """Wait for the workers' done markers and a clean parent exit."""
        assert self.parent is not None
        self.wait_for_workers("done")
        self.output = self.parent.communicate(timeout=COMMAND_TIMEOUT_SECONDS)
        self._output_collected = True
        if self.parent.returncode != 0:
            raise RuntimeError(f"parent {self.parent.pid} exited with {self.parent.returncode}")

    def worker_carriers(self) -> tuple[int, int]:
        """Count and bytes of the tracked creator allocations the workers made
        themselves (not counting PyTorch's), summed over ranks."""
        count = 0
        total = 0
        for rank in range(WORLD_SIZE):
            fields = (self.sync_dir / f"carrier-{rank}").read_text().split()
            count += int(fields[0])
            total += int(fields[1])
        return count, total

    # -- helpers ---------------------------------------------------------------

    def _start_parent(
        self, raw_fds: tuple[int, ...], restore_fds: tuple[int, ...]
    ) -> subprocess.Popen[str]:
        environment = os.environ.copy()
        environment.update(
            {
                "CUDA_VISIBLE_DEVICES": ",".join(self.environment.gpus),
                PARTICIPANT_ID_ENV: PARENT_PARTICIPANT_ID,
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
                "hold-raw-import" if self.hold_raw_import else "release-raw-import",
            ],
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
            pass_fds=raw_fds + restore_fds + self.environment.launch_job_fds,
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
