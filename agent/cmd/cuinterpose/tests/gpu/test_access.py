# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""VMM access updates survive shared-backing reconstruction on real GPUs."""

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
            "SNAPSHOT_CONTROL_DIR": str(control),
        },
        capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def run_worker(coordinator):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    address = cuda_driver.map_allocation(handle, size, 0)
    # Export selects the shared-carrier path, which destroys and recreates backing.
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
    control = Path(os.environ["SNAPSHOT_CONTROL_DIR"])
    checkpoint = control / "checkpoint"
    checkpoint.mkdir()
    for phase in ("--prepare", "--restore"):
        subprocess.run([
            coordinator, phase, "--control-dir", str(control),
            "--checkpoint-dir", str(checkpoint), "--process", str(os.getpid()),
        ], check=True, timeout=20)
    check_access((none, read))
    os.close(fd)
    cuda_driver.destroy_mapped_allocation(address, size, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    run_worker(*sys.argv[1:])
