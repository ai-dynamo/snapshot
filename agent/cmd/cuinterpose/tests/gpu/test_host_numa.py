# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Shared HOST_NUMA bytes and CPU aliases survive cuinterpose reconstruction."""

import ctypes
from multiprocessing import Pipe
from multiprocessing.connection import Connection
import os
from pathlib import Path
import subprocess
import sys

import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import POSIX_FD_HANDLE_TYPE, assert_handle_namespace, cuda_call  # noqa: E402


def host_properties(node):
    properties = driver.CUmemAllocationProp()
    properties.type = driver.CUmemAllocationType.CU_MEM_ALLOCATION_TYPE_PINNED
    properties.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_HOST_NUMA
    properties.location.id = node
    properties.requestedHandleTypes = POSIX_FD_HANDLE_TYPE
    return properties


def map_host(handle, size, node):
    address = int(cuda_call(driver.cuMemAddressReserve, size, 0, 0, 0))
    mapped = False
    try:
        cuda_call(driver.cuMemMap, address, size, 0, handle, 0)
        mapped = True
        access = driver.CUmemAccessDesc()
        access.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_HOST_NUMA
        access.location.id = node
        access.flags = driver.CUmemAccess_flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE
        cuda_call(driver.cuMemSetAccess, address, size, [access], 1)
    except Exception:
        if mapped:
            cuda_call(driver.cuMemUnmap, address, size)
        cuda_call(driver.cuMemAddressFree, address, size)
        raise
    return address


def unmap_host(address, size):
    cuda_call(driver.cuMemUnmap, address, size)
    cuda_call(driver.cuMemAddressFree, address, size)


def verify(address, size, value):
    assert ctypes.string_at(address, size) == bytes([value]) * size, "HOST_NUMA contents changed"


def receive(channel):
    assert channel.poll(20), "HOST_NUMA importer did not reply"
    return channel.recv()


@pytest.mark.gpu
@pytest.mark.host_numa
@pytest.mark.parametrize("release_creator_handle", [False, True])
def test_host_numa_shared_reconstruction(release_creator_handle, tools, tmp_path):
    status, = driver.cuInit(0)
    assert status == driver.CUresult.CUDA_SUCCESS
    # Prefer a nonzero node when available, so a NUMA ID cannot accidentally
    # work as the worker's sole CUDA device ordinal.
    nodes = sorted(int(path.name[4:]) for path in Path("/sys/devices/system/node").glob("node[0-9]*")
                   if (path / "cpulist").read_text().strip())
    node = int(os.environ.get("CUINTERPOSE_TEST_HOST_NUMA_NODE", nodes[-1] if nodes else 0))
    properties = host_properties(node)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    # This selected lane requires native POSIX-shareable HOST_NUMA VMM support.
    status, handle = driver.cuMemCreate(size, properties, 0)
    assert status == driver.CUresult.CUDA_SUCCESS, status
    cuda_call(driver.cuMemRelease, handle)

    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {
        "LD_PRELOAD": str(tools.interposer),
        "SNAPSHOT_CONTROL_DIR": str(control),
        "CUDA_VISIBLE_DEVICES": os.environ.get("CUDA_VISIBLE_DEVICES", "0").split(",")[0],
    }
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), "creator", str(node), str(size),
         str(tools.coordinator), str(int(release_creator_handle))],
        env=env, capture_output=True, text=True, timeout=120,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def run_importer(node, size, descriptor, channel_fd):
    channel = Connection(channel_fd)
    cuda_call(driver.cuInit, 0)
    handle = cuda_call(driver.cuMemImportFromShareableHandle, descriptor, POSIX_FD_HANDLE_TYPE)
    os.close(descriptor)
    assert_handle_namespace(handle, virtual=True, stage="HOST_NUMA import")
    address = map_host(handle, size, node)
    channel.send("ready")
    while True:
        operation, value = channel.recv()
        if operation == "stop":
            break
        if operation == "write":
            ctypes.memset(address, value, size)
        else:
            assert operation == "verify", operation
            verify(address, size, value)
        channel.send("ok")
    unmap_host(address, size)
    cuda_call(driver.cuMemRelease, handle)
    channel.close()


def run_creator(node, size, coordinator, release_handle):
    cuda_call(driver.cuInit, 0)
    # Keep the allocation contextless. Carrier registration must choose a CUDA
    # device independently from the host NUMA node used for backing placement.
    handle = cuda_call(driver.cuMemCreate, size, host_properties(node), 0)
    assert_handle_namespace(handle, virtual=True, stage="HOST_NUMA create")
    address = map_host(handle, size, node)
    alias = map_host(handle, size, node)
    value = 0x31
    ctypes.memset(address, value, size)
    # Reconstruct CPU and GPU backing in the same carrier, including a host
    # allocation created without a context and a device allocation with one.
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    device_properties = cuda_driver.allocation_properties(0)
    device_size = int(cuda_call(driver.cuMemGetAllocationGranularity, device_properties,
                               driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    device_handle = cuda_call(driver.cuMemCreate, device_size, device_properties, 0)
    device_address = cuda_driver.map_allocation(device_handle, device_size, 0)
    cuda_driver.write_bytes(device_address, b"mixed device backing")
    device_fd = int(cuda_call(driver.cuMemExportToShareableHandle,
                             device_handle, POSIX_FD_HANDLE_TYPE, 0))
    cuda_call(driver.cuCtxSetCurrent, 0)
    descriptor = int(cuda_call(driver.cuMemExportToShareableHandle, handle, POSIX_FD_HANDLE_TYPE, 0))
    channel, child_channel = Pipe()
    importer = subprocess.Popen(
        [sys.executable, str(Path(__file__).resolve()), "importer", str(node), str(size),
         str(descriptor), str(child_channel.fileno())],
        pass_fds=(descriptor, child_channel.fileno()),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    child_channel.close()
    os.close(descriptor)
    try:
        assert receive(channel) == "ready"
        if release_handle:
            cuda_call(driver.cuMemRelease, handle)
        control = Path(os.environ["SNAPSHOT_CONTROL_DIR"])
        for cycle in range(2):
            checkpoint = control / f"checkpoint-{cycle}"
            checkpoint.mkdir()
            # All application CUDA calls have finished. The importer waits on
            # the pipe and the creator waits for each global coordinator phase.
            # No native checkpoint or CRIU participates in this regression.
            for phase in ("--prepare", "--restore"):
                subprocess.run([
                    coordinator, phase, "--control-dir", str(control),
                    "--checkpoint-dir", str(checkpoint), "--process", str(os.getpid()),
                    "--process", str(importer.pid),
                ], check=True, timeout=20)
            verify(address, size, value)
            verify(alias, size, value)
            cuda_call(driver.cuCtxPushCurrent, context)
            cuda_driver.assert_bytes(device_address, b"mixed device backing", "mixed carrier restore")
            cuda_call(driver.cuCtxPopCurrent)
            channel.send(("verify", value))
            assert receive(channel) == "ok"
            value = 0x52 + cycle
            channel.send(("write", value))
            assert receive(channel) == "ok"
            verify(address, size, value)
            verify(alias, size, value)
            value = 0x73 + cycle
            ctypes.memset(alias, value, size)
            channel.send(("verify", value))
            assert receive(channel) == "ok"
        channel.send(("stop", 0))
        stdout, stderr = importer.communicate(timeout=20)
        assert importer.returncode == 0, stdout + stderr
    finally:
        if importer.poll() is None:
            importer.kill()
        importer.communicate(timeout=5)
        channel.close()
    unmap_host(alias, size)
    unmap_host(address, size)
    if not release_handle:
        cuda_call(driver.cuMemRelease, handle)
    os.close(device_fd)
    cuda_call(driver.cuCtxSetCurrent, context)
    cuda_driver.destroy_mapped_allocation(device_address, device_size, device_handle)
    cuda_call(driver.cuCtxSetCurrent, 0)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    role, node, size, *arguments = sys.argv[1:]
    if role == "creator":
        run_creator(int(node), int(size), arguments[0], bool(int(arguments[1])))
    else:
        assert role == "importer", role
        run_importer(int(node), int(size), int(arguments[0]), int(arguments[1]))
