# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Verify VMM access permissions after shared backing is rebuilt on real GPUs."""

import os
from pathlib import Path
import subprocess
import sys

import pytest

from cuda.bindings import driver  # noqa: E402

import cuda_driver  # noqa: E402
from cuda_driver import cuda_call  # noqa: E402


@pytest.mark.gpu
def test_access_updates_survive_reconstruction(tools, tmp_path):
    cuda_call(driver.cuInit, 0)
    assert cuda_call(driver.cuDeviceCanAccessPeer, 1, 0), "requires GPU peer access"
    control = tmp_path / "control"
    control.mkdir()
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), str(tools.coordinator)],
        env=os.environ | {
            "LD_PRELOAD": str(tools.interposer),
            "CUINTERPOSE_SOCKET_DIR": str(control),
        },
        capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


@pytest.mark.gpu
@pytest.mark.parametrize("layout", ["prefix", "interior", "suffix", "adjacent"])
def test_access_range_boundaries(layout, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    environment = os.environ | {"CUINTERPOSE_SOCKET_DIR": str(control), "LD_PRELOAD": str(tools.interposer)}
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), layout, str(tools.coordinator)],
        env=environment, capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def run_range_worker(layout, coordinator):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    granularity = int(cuda_call(
        driver.cuMemGetAllocationGranularity, properties,
        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM,
    ))
    size = 3 * granularity
    address = int(cuda_call(driver.cuMemAddressReserve, size, 0, 0, 0))
    ranges = [(index * granularity, granularity) for index in range(3)] \
        if layout == "adjacent" else [(0, size)]
    handles, descriptors = [], []
    for offset, length in ranges:
        handle = cuda_call(driver.cuMemCreate, length, properties, 0)
        handles.append(handle)
        cuda_call(driver.cuMemMap, address + offset, length, 0, handle, 0)
        descriptors.append(int(cuda_call(
            driver.cuMemExportToShareableHandle, handle,
            cuda_driver.POSIX_FD_HANDLE_TYPE, 0,
        )))
    access = driver.CUmemAccessDesc()
    access.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
    access.location.id = 0
    flags = driver.CUmemAccess_flags
    access.flags = flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE
    cuda_call(driver.cuMemSetAccess, address, size, [access], 1)
    access.flags = flags.CU_MEM_ACCESS_FLAGS_PROT_READ
    offset = {"prefix": 0, "interior": granularity, "suffix": 2 * granularity,
              "adjacent": 0}[layout]
    length = size if layout == "adjacent" else granularity
    status, = driver.cuMemSetAccess(address + offset, length, [access], 1)
    expected_status = driver.CUresult.CUDA_SUCCESS if layout == "adjacent" \
        else driver.CUresult.CUDA_ERROR_INVALID_VALUE
    assert status == expected_status, (layout, status)
    expected_access = flags.CU_MEM_ACCESS_FLAGS_PROT_READ if layout == "adjacent" \
        else flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE

    def check_access():
        for index in range(3):
            assert cuda_call(driver.cuMemGetAccess, access.location,
                             address + index * granularity) == expected_access

    check_access()
    control = Path(os.environ["CUINTERPOSE_SOCKET_DIR"])
    checkpoint = control / "checkpoint"
    checkpoint.mkdir()
    for phase in ("--prepare", "--restore"):
        subprocess.run([
            coordinator, phase, "--socket-dir", str(control),
            "--checkpoint-dir", str(checkpoint), "--process", str(os.getpid()),
        ], check=True, timeout=20)
    check_access()
    for descriptor in descriptors:
        os.close(descriptor)
    for offset, length in ranges:
        cuda_call(driver.cuMemUnmap, address + offset, length)
    cuda_call(driver.cuMemAddressFree, address, size)
    for handle in handles:
        cuda_call(driver.cuMemRelease, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


def run_worker(coordinator):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    address = cuda_driver.map_allocation(handle, size, 0)
    # Export marks this backing as shared so the host carrier path destroys and
    # recreates it.
    fd = int(cuda_call(driver.cuMemExportToShareableHandle, handle,
                       cuda_driver.POSIX_FD_HANDLE_TYPE, 0))
    flags = driver.CUmemAccess_flags
    readwrite, read, none = (flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE,
                           flags.CU_MEM_ACCESS_FLAGS_PROT_READ,
                           flags.CU_MEM_ACCESS_FLAGS_PROT_NONE)

    def set_access(*entries):
        descriptors = []
        for device, access in entries:
            descriptor = driver.CUmemAccessDesc()
            descriptor.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
            descriptor.location.id = device
            descriptor.flags = access
            descriptors.append(descriptor)
        cuda_call(driver.cuMemSetAccess, address, size, descriptors, len(descriptors))

    def check_access(expected):
        for device, access in enumerate(expected):
            location = driver.CUmemLocation()
            location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
            location.id = device
            assert cuda_call(driver.cuMemGetAccess, location, address) == access

    set_access((1, readwrite))
    check_access((readwrite, readwrite))  # Omitted locations retain their access.
    set_access((1, readwrite), (1, read))
    check_access((readwrite, read))  # The last duplicate descriptor takes effect.
    set_access((0, none))
    check_access((none, read))
    control = Path(os.environ["CUINTERPOSE_SOCKET_DIR"])
    checkpoint = control / "checkpoint"
    checkpoint.mkdir()
    for phase in ("--prepare", "--restore"):
        subprocess.run([
            coordinator, phase, "--socket-dir", str(control),
            "--checkpoint-dir", str(checkpoint), "--process", str(os.getpid()),
        ], check=True, timeout=20)
    check_access((none, read))
    os.close(fd)
    cuda_driver.destroy_mapped_allocation(address, size, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    if len(sys.argv) == 3:
        run_range_worker(*sys.argv[1:])
    else:
        run_worker(*sys.argv[1:])
