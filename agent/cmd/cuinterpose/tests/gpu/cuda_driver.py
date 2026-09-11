# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CUDA Driver API mechanics used by the cuinterpose GPU tests.

This module contains no workload or coordinator orchestration. It owns direct
CUDA calls, VMM allocation helpers, raw allocations created outside the shim,
the copy-bandwidth baseline, and the native process checkpoint state machine.
"""

from __future__ import annotations

import ctypes
import os
import queue
import threading
import time
from typing import NamedTuple

from cuda.bindings import driver

POSIX_FD_HANDLE_TYPE = (
    driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR
)

# Logical handles carry this tag in the top 16 bits (interpose.c).
LOGICAL_HANDLE_TAG = 0xD94D000000000000
LOGICAL_HANDLE_TAG_MASK = 0xFFFF000000000000


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


class ExternalAllocation(NamedTuple):
    """A POSIX-shareable allocation owned by the uninterposed test process."""

    device: driver.CUdevice
    context: driver.CUcontext
    address: int
    size: int
    handle: driver.CUmemGenericAllocationHandle
    fd: int


def create_external_allocations(count: int, byte_base: int) -> list[ExternalAllocation]:
    """Create raw descriptors workers can import outside cuinterpose tracking."""
    cuda_call(driver.cuInit, 0)
    allocations: list[ExternalAllocation] = []
    try:
        for rank in range(count):
            device = cuda_call(driver.cuDeviceGet, rank)
            context = cuda_call(driver.cuDevicePrimaryCtxRetain, device)
            handle = None
            address = 0
            try:
                cuda_call(driver.cuCtxPushCurrent, context)
                try:
                    properties = allocation_properties(device)
                    size = int(
                        cuda_call(
                            driver.cuMemGetAllocationGranularity,
                            properties,
                            driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM,
                        )
                    )
                    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
                    address = map_allocation(handle, size, device)
                    write_bytes(address, bytes([byte_base + rank]) * 32)
                    fd = int(
                        cuda_call(
                            driver.cuMemExportToShareableHandle,
                            handle,
                            POSIX_FD_HANDLE_TYPE,
                            0,
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
            allocations.append(ExternalAllocation(device, context, address, size, handle, fd))
    except Exception:
        destroy_external_allocations(allocations)
        raise
    return allocations


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


def measure_h2d_gbps(device_ordinal: int, nbytes: int, repeats: int = 3) -> float:
    """Measure the best synchronous pinned H2D copy in decimal GB/s."""
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
                if attempt:
                    best = max(best, nbytes / 1e9 / elapsed)
            return best
        finally:
            cuda_call(driver.cuMemFreeHost, source)
            cuda_call(driver.cuMemFree, destination)
    finally:
        cuda_call(driver.cuCtxPopCurrent)
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


def _expect_process_state(process_id: int, expected) -> None:
    actual = cuda_call(driver.cuCheckpointProcessGetState, process_id)
    if actual != expected:
        raise AssertionError(
            f"CUDA process {process_id} is {actual.name}, expected {expected.name}"
        )


def _checkpoint_processes(process_ids: tuple[int, ...], command_timeout_seconds: int) -> None:
    cuda_call(driver.cuInit, 0)
    running = driver.CUprocessState.CU_PROCESS_STATE_RUNNING
    locked = driver.CUprocessState.CU_PROCESS_STATE_LOCKED
    checkpointed = driver.CUprocessState.CU_PROCESS_STATE_CHECKPOINTED
    for process_id in process_ids:
        _expect_process_state(process_id, running)

    lock_arguments = driver.CUcheckpointLockArgs()
    lock_arguments.timeoutMs = command_timeout_seconds * 1000
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessLock, process_id, lock_arguments)
    for process_id in process_ids:
        _expect_process_state(process_id, locked)

    checkpoint_arguments = driver.CUcheckpointCheckpointArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessCheckpoint, process_id, checkpoint_arguments)
    for process_id in process_ids:
        _expect_process_state(process_id, checkpointed)

    restore_arguments = driver.CUcheckpointRestoreArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessRestore, process_id, restore_arguments)
    for process_id in process_ids:
        _expect_process_state(process_id, locked)

    unlock_arguments = driver.CUcheckpointUnlockArgs()
    for process_id in process_ids:
        cuda_call(driver.cuCheckpointProcessUnlock, process_id, unlock_arguments)
    for process_id in process_ids:
        _expect_process_state(process_id, running)


def native_checkpoint(
    process_ids: tuple[int, ...],
    *,
    command_timeout_seconds: int,
    checkpoint_timeout_seconds: int,
) -> None:
    """Checkpoint and restore workers with a bound on a wedged driver call."""
    outcomes: queue.Queue[Exception | None] = queue.Queue(maxsize=1)

    def run() -> None:
        try:
            _checkpoint_processes(process_ids, command_timeout_seconds)
        except Exception as error:  # noqa: BLE001 -- forwarded to the test thread
            outcomes.put(error)
        else:
            outcomes.put(None)

    threading.Thread(target=run, daemon=True).start()
    try:
        outcome = outcomes.get(timeout=checkpoint_timeout_seconds)
    except queue.Empty as error:
        raise TimeoutError(
            f"native CUDA checkpoint exceeded {checkpoint_timeout_seconds} seconds"
        ) from error
    if outcome is not None:
        raise outcome
