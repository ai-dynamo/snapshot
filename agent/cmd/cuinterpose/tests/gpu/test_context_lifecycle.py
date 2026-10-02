# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Context teardown frees converted mallocs while explicit VMM survives."""

import os
from pathlib import Path
import subprocess
import sys

import pytest

from cuda.bindings import driver, runtime  # noqa: E402

import cuda_driver  # noqa: E402
from cuda_driver import cuda_call  # noqa: E402


@pytest.mark.gpu
@pytest.mark.parametrize("mode", ["destroy", "reset", "release", "runtime"])
def test_context_teardown(mode, tools, tmp_path):
    cuda_call(driver.cuInit, 0)
    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {
        "LD_PRELOAD": str(tools.interposer),
        "SNAPSHOT_CONTROL_DIR": str(control),
    }
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), mode, str(tools.coordinator)],
        env=env, capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def runtime_call(function, *args):
    status, *outputs = function(*args)
    assert status == runtime.cudaError_t.cudaSuccess, (function.__name__, status)
    return outputs[0] if outputs else None


def run_worker(mode, coordinator):
    cuda_call(driver.cuInit, 0)
    device = cuda_call(driver.cuDeviceGet, 0)
    observer = cuda_call(driver.cuCtxCreate, None, 0, device)
    survivor = int(cuda_call(driver.cuMemAlloc, 4096))
    cuda_driver.write_bytes(survivor, b"other context")

    if mode == "destroy":
        target = cuda_call(driver.cuCtxCreate, None, 0, device)
    elif mode == "runtime":
        # Exercise Runtime API initialization with no application primary retain.
        cuda_call(driver.cuCtxSetCurrent, 0)
        runtime_call(runtime.cudaSetDevice, 0)
    else:
        target = cuda_call(driver.cuDevicePrimaryCtxRetain, device)
        cuda_call(driver.cuCtxSetCurrent, target)

    allocated = int(runtime_call(runtime.cudaMalloc, 64 << 20) if mode == "runtime"
                    else cuda_call(driver.cuMemAlloc, 64 << 20))
    malloc_handle = cuda_call(driver.cuMemRetainAllocationHandle, allocated)
    cuda_driver.assert_handle_namespace(malloc_handle, virtual=True, stage="converted malloc")
    rdma_supported = all(cuda_call(driver.cuDeviceGetAttribute, attribute, device) for attribute in (
        driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED,
        driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WITH_CUDA_VMM_SUPPORTED,
    ))
    malloc_properties = cuda_call(driver.cuMemGetAllocationPropertiesFromHandle, malloc_handle)
    assert bool(malloc_properties.allocFlags.gpuDirectRDMACapable) == rdma_supported
    assert bool(cuda_call(
        driver.cuPointerGetAttribute,
        driver.CUpointer_attribute.CU_POINTER_ATTRIBUTE_IS_GPU_DIRECT_RDMA_CAPABLE,
        allocated,
    )) == rdma_supported
    cuda_call(driver.cuMemRelease, malloc_handle)
    cuda_driver.write_bytes(allocated, b"converted malloc")
    properties = cuda_driver.allocation_properties(device)
    extent = int(cuda_call(
        driver.cuMemGetAllocationGranularity, properties,
        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM,
    ))
    handle = cuda_call(driver.cuMemCreate, extent, properties, 0)
    cuda_driver.assert_handle_namespace(handle, virtual=True, stage="before teardown")
    address = cuda_driver.map_allocation(handle, extent, device)
    cuda_driver.write_bytes(address, b"explicit VMM")
    fd = int(cuda_call(driver.cuMemExportToShareableHandle, handle,
                       cuda_driver.POSIX_FD_HANDLE_TYPE, 0))

    if mode == "release":
        cuda_call(driver.cuDevicePrimaryCtxRetain, device)
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)
        base, size = cuda_call(driver.cuMemGetAddressRange, allocated)
        assert int(base) == allocated and int(size) == 64 << 20
        cuda_driver.assert_bytes(allocated, b"converted malloc", "nonfinal release")

    if mode == "destroy":
        cuda_call(driver.cuCtxDestroy, target)
    elif mode == "runtime":
        runtime_call(runtime.cudaDeviceReset)
    elif mode == "reset":
        cuda_call(driver.cuDevicePrimaryCtxReset, device)
    else:
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)

    cuda_call(driver.cuCtxSetCurrent, observer)
    status, *_ = driver.cuMemGetAddressRange(allocated)
    assert status in (driver.CUresult.CUDA_ERROR_INVALID_VALUE,
                      driver.CUresult.CUDA_ERROR_NOT_FOUND), status
    # Check driver backing as well as the shim's malloc-range bookkeeping.
    status, *_ = driver.cuMemRetainAllocationHandle(allocated)
    assert status != driver.CUresult.CUDA_SUCCESS, "malloc mapping survived teardown"
    cuda_driver.assert_bytes(survivor, b"other context", "other live context")
    cuda_driver.assert_bytes(address, b"explicit VMM", "after teardown")

    # Shared direct VMM must still be checkpointable after its cached context dies.
    # Exercise carrier prepare/reconstruction here; this is not a native/CRIU dump.
    control = Path(os.environ["SNAPSHOT_CONTROL_DIR"])
    checkpoint = control / "checkpoint"
    checkpoint.mkdir()
    for phase in ("--prepare", "--restore"):
        subprocess.run([
            coordinator, phase, "--control-dir", str(control),
            "--checkpoint-dir", str(checkpoint), "--process", str(os.getpid()),
        ], check=True, timeout=20)
    cuda_driver.assert_bytes(address, b"explicit VMM", "after reconstruction")
    cuda_driver.assert_bytes(survivor, b"other context", "after reconstruction")

    os.close(fd)
    cuda_driver.destroy_mapped_allocation(address, extent, handle)
    cuda_call(driver.cuMemFree, survivor)
    cuda_call(driver.cuCtxDestroy, observer)
    if mode == "reset":
        # Reset preserves the application's primary-context retain.
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


if __name__ == "__main__":
    run_worker(*sys.argv[1:])
