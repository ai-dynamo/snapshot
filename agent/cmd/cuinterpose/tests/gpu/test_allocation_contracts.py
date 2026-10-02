# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Test allocation and handle lifetimes with CUDA, both with and without the shim."""

import ctypes
import os
from pathlib import Path
import subprocess
import sys

import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import POSIX_FD_HANDLE_TYPE, cuda_call


@pytest.mark.gpu
@pytest.mark.parametrize("interposed", [False, True], ids=["native", "shim"])
@pytest.mark.parametrize("case", ["aliases", "nonexportable", "malloc", "invalid-create"])
def test_allocation_contracts(case, interposed, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {"SNAPSHOT_CONTROL_DIR": str(control)}
    if interposed:
        env["LD_PRELOAD"] = str(tools.interposer)
    else:
        env.pop("LD_PRELOAD", None)
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), case, str(int(interposed))],
        env=env, capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def allocation_aliases(interposed, exportable):
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    if not exportable:
        properties.requestedHandleTypes = driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_NONE
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    cuda_driver.assert_handle_namespace(handle, interposed and exportable, "created backing")
    addresses = [cuda_driver.map_allocation(handle, size, 0) for _ in range(2)]
    cuda_driver.write_bytes(addresses[0], b"shared allocation")
    retained = cuda_call(driver.cuMemRetainAllocationHandle, addresses[0])
    cuda_call(driver.cuMemRelease, handle)
    cuda_call(driver.cuMemRelease, retained)
    # Mappings retain the backing after all application handles are released.
    for address in addresses:
        cuda_driver.assert_bytes(address, b"shared allocation", "released handles")
    retained = cuda_call(driver.cuMemRetainAllocationHandle, addresses[0])
    if exportable:
        descriptor = int(cuda_call(driver.cuMemExportToShareableHandle,
                                   retained, POSIX_FD_HANDLE_TYPE, 0))
        imported = cuda_call(driver.cuMemImportFromShareableHandle, descriptor, POSIX_FD_HANDLE_TYPE)
        os.close(descriptor)
        cuda_driver.assert_handle_namespace(imported, interposed, "imported alias")
        addresses.append(cuda_driver.map_allocation(imported, size, 0))
        cuda_call(driver.cuMemRelease, imported)
    cuda_call(driver.cuMemRelease, retained)
    cuda_driver.write_bytes(addresses[-1], b"updated allocation")
    for address in addresses:
        cuda_driver.assert_bytes(address, b"updated allocation", "retained and imported aliases")
        cuda_call(driver.cuMemUnmap, address, size)
        cuda_call(driver.cuMemAddressFree, address, size)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


def malloc_on_both_devices(interposed):
    contexts = [cuda_call(driver.cuDevicePrimaryCtxRetain, device) for device in range(2)]
    for device in (0, 1, 0):
        cuda_call(driver.cuCtxSetCurrent, contexts[device])
        for size in (1, 65537, (2 << 20) + 1):
            address = int(cuda_call(driver.cuMemAlloc, size))
            base, extent = cuda_call(driver.cuMemGetAddressRange, address + size - 1)
            assert int(base) == address and int(extent) >= size
            if interposed:
                assert int(extent) == size
                handle = cuda_call(driver.cuMemRetainAllocationHandle, address)
                properties = cuda_call(driver.cuMemGetAllocationPropertiesFromHandle, handle)
                assert properties.location.id == device
                rdma = all(cuda_call(driver.cuDeviceGetAttribute, attribute, device) for attribute in (
                    driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_SUPPORTED,
                    driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WITH_CUDA_VMM_SUPPORTED,
                ))
                assert bool(properties.allocFlags.gpuDirectRDMACapable) == rdma
                cuda_call(driver.cuMemRelease, handle)
            cuda_driver.write_bytes(address + size - 1, b"x")
            cuda_driver.assert_bytes(address + size - 1, b"x", "malloc bounds")
            cuda_call(driver.cuMemFree, address)
        # Pitched allocations stay native, so this exercises the range fallback.
        address, pitch = cuda_call(driver.cuMemAllocPitch, 16, 4, 4)
        base, extent = cuda_call(driver.cuMemGetAddressRange, int(address) + int(pitch) * 4 - 1)
        assert int(base) == int(address) and int(extent) >= int(pitch) * 4
        cuda_call(driver.cuMemFree, address)
    cuda_call(driver.cuCtxSetCurrent, 0)
    for device in range(2):
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


def invalid_create_preserves_output(interposed):
    # Use the C entry point to check whether it changes the caller's output value on
    # failure.
    create = ctypes.CDLL("libcuda.so.1").cuMemCreate
    create.argtypes = [ctypes.POINTER(ctypes.c_uint64), ctypes.c_size_t,
                       ctypes.c_void_p, ctypes.c_uint64]
    create.restype = ctypes.c_int
    properties = cuda_driver.allocation_properties(0)
    handle = ctypes.c_uint64(99)
    status = create(ctypes.byref(handle), 0, properties.getPtr(), 0)
    assert status == int(driver.CUresult.CUDA_ERROR_INVALID_VALUE), status
    assert handle.value == 99, "failed creation changed the caller's output"
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    properties.type = driver.CUmemAllocationType.CU_MEM_ALLOCATION_TYPE_INVALID
    status = create(ctypes.byref(handle), size, properties.getPtr(), 0)
    expected = (driver.CUresult.CUDA_ERROR_NOT_SUPPORTED if interposed
                else driver.CUresult.CUDA_ERROR_INVALID_VALUE)
    assert status == int(expected), status
    assert handle.value == 99, "invalid allocation type changed the caller's output"


if __name__ == "__main__":
    case, interposed = sys.argv[1], bool(int(sys.argv[2]))
    cuda_call(driver.cuInit, 0)
    if case == "malloc":
        malloc_on_both_devices(interposed)
    elif case == "invalid-create":
        invalid_create_preserves_output(interposed)
    else:
        allocation_aliases(interposed, exportable=case == "aliases")
