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
from test_multicast_locking import records


@pytest.mark.gpu
@pytest.mark.parametrize("case", ["aliases", "nonexportable", "malloc", "invalid-create"])
def test_allocation_contracts(case, tools, tmp_path):
    run_case(case, tools, tmp_path)


@pytest.mark.gpu
@pytest.mark.parametrize("case", ["nonposix-create", "nonposix-export", "export-flags"])
def test_shim_rejections_preserve_state(case, tools, tmp_path):
    run_case(case, tools, tmp_path)


@pytest.mark.gpu
@pytest.mark.multicast
@pytest.mark.host_numa
def test_host_numa_v1_multicast_bind_is_rejected(multicast_supported, tools, tmp_path):
    run_case("host-numa-v1", tools, tmp_path)


def run_case(case, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {"CUINTERPOSE_SOCKET_DIR": str(control), "LD_PRELOAD": str(tools.interposer)}
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), case],
        env=env, capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def allocation_aliases(exportable):
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    if not exportable:
        properties.requestedHandleTypes = driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_NONE
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    cuda_driver.assert_handle_namespace(handle, exportable, "created backing")
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
        cuda_driver.assert_handle_namespace(imported, True, "imported alias")
        addresses.append(cuda_driver.map_allocation(imported, size, 0))
        cuda_call(driver.cuMemRelease, imported)
    cuda_call(driver.cuMemRelease, retained)
    cuda_driver.write_bytes(addresses[-1], b"updated allocation")
    for address in addresses:
        cuda_driver.assert_bytes(address, b"updated allocation", "retained and imported aliases")
        cuda_call(driver.cuMemUnmap, address, size)
        cuda_call(driver.cuMemAddressFree, address, size)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


def malloc_on_both_devices():
    contexts = [cuda_call(driver.cuDevicePrimaryCtxRetain, device) for device in range(2)]
    for device in (0, 1, 0):
        cuda_call(driver.cuCtxSetCurrent, contexts[device])
        for size in (1, 65537, (2 << 20) + 1):
            address = int(cuda_call(driver.cuMemAlloc, size))
            base, extent = cuda_call(driver.cuMemGetAddressRange, address + size - 1)
            assert int(base) == address and int(extent) == size
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


def invalid_create_preserves_output():
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
    assert status == int(driver.CUresult.CUDA_ERROR_NOT_SUPPORTED), status
    assert handle.value == 99, "invalid allocation type changed the caller's output"


def rejected_allocation(case):
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    library = ctypes.CDLL("libcuda.so.1")
    if case == "nonposix-create":
        create = library.cuMemCreate
        create.argtypes = [ctypes.POINTER(ctypes.c_uint64), ctypes.c_size_t,
                           ctypes.c_void_p, ctypes.c_uint64]
        create.restype = ctypes.c_int
        properties.requestedHandleTypes = driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_FABRIC
        output = ctypes.c_uint64(99)
        before = records()
        status = create(ctypes.byref(output), size, properties.getPtr(), 0)
        assert status == int(driver.CUresult.CUDA_ERROR_NOT_SUPPORTED), (case, status)
        assert output.value == 99 and records() == before
    else:
        handle = cuda_call(driver.cuMemCreate, size, properties, 0)
        export = library.cuMemExportToShareableHandle
        export.argtypes = [ctypes.POINTER(ctypes.c_int), ctypes.c_uint64,
                           ctypes.c_uint, ctypes.c_uint64]
        export.restype = ctypes.c_int
        kind = (driver.CUmemAllocationHandleType.CU_MEM_HANDLE_TYPE_FABRIC
                if case == "nonposix-export" else POSIX_FD_HANDLE_TYPE)
        output = ctypes.c_int(-99)
        before = records()
        status = export(ctypes.byref(output), int(handle), int(kind), int(case == "export-flags"))
        assert status == int(driver.CUresult.CUDA_ERROR_INVALID_VALUE), (case, status)
        assert output.value == -99 and records() == before
        descriptor = int(cuda_call(driver.cuMemExportToShareableHandle,
                                  handle, POSIX_FD_HANDLE_TYPE, 0))
        os.close(descriptor)
        cuda_call(driver.cuMemRelease, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


def rejected_host_numa_bind():
    from test_host_numa import host_properties

    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = driver.CUmulticastObjectProp()
    properties.numDevices = 2
    properties.handleTypes = POSIX_FD_HANDLE_TYPE
    properties.size = cuda_call(driver.cuMulticastGetGranularity, properties,
                               driver.CUmulticastGranularity_flags.CU_MULTICAST_GRANULARITY_MINIMUM)
    group = cuda_call(driver.cuMulticastCreate, properties)
    for device in range(2):
        cuda_call(driver.cuMulticastAddDevice, group, device)
    node = max(0, int(cuda_call(driver.cuDeviceGetAttribute,
                               driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, 0)))
    host = host_properties(node)
    granularity = int(cuda_call(driver.cuMemGetAllocationGranularity, host,
                               driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    size = ((int(properties.size) + granularity - 1) // granularity) * granularity
    member = cuda_call(driver.cuMemCreate, size, host, 0)
    before = records()
    status, = driver.cuMulticastBindMem(group, 0, member, 0, int(properties.size), 0)
    assert status == driver.CUresult.CUDA_ERROR_NOT_SUPPORTED
    assert records() == before, "rejected HOST_NUMA bind changed tracked state"
    cuda_call(driver.cuMemRelease, member)
    cuda_call(driver.cuMemRelease, group)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    case = sys.argv[1]
    cuda_call(driver.cuInit, 0)
    if case == "malloc":
        malloc_on_both_devices()
    elif case == "invalid-create":
        invalid_create_preserves_output()
    elif case == "host-numa-v1":
        rejected_host_numa_bind()
    elif case in ("nonposix-create", "nonposix-export", "export-flags"):
        rejected_allocation(case)
    else:
        allocation_aliases(exportable=case == "aliases")
