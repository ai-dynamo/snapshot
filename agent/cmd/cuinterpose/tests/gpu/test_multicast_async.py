# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Restore multicast bindings to native async pools and tracked VMM members."""

import ctypes
import os
from pathlib import Path
import select
import subprocess
import sys

import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import cuda_call
from test_multicast_locking import records


@pytest.mark.gpu
@pytest.mark.multicast
@pytest.mark.parametrize("source,version", [("address", "v1"), ("address", "v2"), ("memory", "v2")])
def test_multicast_bindings_survive_restore(source, version, multicast_supported, tools, tmp_path):
    if version == "v2":
        assert cuda_call(driver.cuDriverGetVersion) >= 13010, "selected v2 cases require CUDA 13.1"
    control = tmp_path / "control"
    checkpoint = tmp_path / "checkpoint"
    control.mkdir()
    checkpoint.mkdir()
    environment = os.environ | {
        "LD_PRELOAD": str(tools.interposer), "SNAPSHOT_CONTROL_DIR": str(control),
    }
    with (tmp_path / "worker.log").open("w+") as log:
        child = subprocess.Popen(
            [sys.executable, str(Path(__file__).resolve()), source, version], env=environment,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=log, text=True,
        )
        try:
            ready = select.select([child.stdout], [], [], 20)[0]
            if not ready or child.stdout.readline().strip() != "ready":
                log.seek(0)
                pytest.fail("worker did not become ready:\n" + log.read())
            for phase in ("--prepare", "--restore"):
                subprocess.run([
                    str(tools.coordinator), phase, "--control-dir", str(control),
                    "--checkpoint-dir", str(checkpoint), "--process", str(child.pid),
                ], check=True, capture_output=True, text=True, timeout=30)
                if phase == "--prepare":
                    # CUDA restores its own allocations before the shim rebuilds
                    # multicast bindings, including native async-pool addresses.
                    cuda_driver.native_checkpoint(
                        (child.pid,), command_timeout_seconds=20,
                        checkpoint_timeout_seconds=30,
                    )
            stdout, _ = child.communicate("continue\n", timeout=20)
            log.seek(0)
            assert child.returncode == 0, stdout + log.read()
        finally:
            if child.poll() is None:
                child.kill()
            child.wait(timeout=10)
            log.seek(0)
            print(log.read())


def read_multicast(address):
    # Ordinary loads from a multicast VA are undefined. Use a real multimem load.
    ptx = b""".version 8.1
.target sm_90
.address_size 64
.visible .entry read_multicast(.param .u64 source, .param .u64 destination) {
 .reg .b64 %source, %destination;
 .reg .b32 %value;
 ld.param.u64 %source, [source];
 ld.param.u64 %destination, [destination];
 multimem.ld_reduce.acquire.sys.global.add.u32 %value, [%source];
 st.global.u32 [%destination], %value;
 ret;
}
\0"""
    module = cuda_call(driver.cuModuleLoadData, ptx)
    function = cuda_call(driver.cuModuleGetFunction, module, b"read_multicast")
    output = int(cuda_call(driver.cuMemAlloc, 4))
    cuda_call(driver.cuMemsetD32, output, 0, 1)
    arguments = (ctypes.c_uint64(address), ctypes.c_uint64(output))
    parameters = (ctypes.c_void_p * 2)(*(ctypes.addressof(arg) for arg in arguments))
    launch = ctypes.CDLL("libcuda.so.1").cuLaunchKernel
    launch.argtypes = [ctypes.c_void_p] + [ctypes.c_uint] * 7 + [
        ctypes.c_void_p, ctypes.POINTER(ctypes.c_void_p), ctypes.c_void_p,
    ]
    launch.restype = ctypes.c_int
    assert launch(int(function), 1, 1, 1, 1, 1, 1, 0, None, parameters, None) == 0
    cuda_call(driver.cuCtxSynchronize)
    result = ctypes.c_uint32()
    cuda_call(driver.cuMemcpyDtoH, ctypes.addressof(result), output, 4)
    assert result.value == 0x23456789, hex(result.value)
    cuda_call(driver.cuMemFree, output)
    cuda_call(driver.cuModuleUnload, module)


def run_worker(source, version):
    cuda_call(driver.cuInit, 0)
    contexts = [cuda_call(driver.cuDevicePrimaryCtxRetain, device) for device in range(2)]
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    properties = driver.CUmulticastObjectProp()
    properties.numDevices = 2
    properties.handleTypes = cuda_driver.POSIX_FD_HANDLE_TYPE
    size = int(cuda_call(
        driver.cuMulticastGetGranularity, properties,
        driver.CUmulticastGranularity_flags.CU_MULTICAST_GRANULARITY_MINIMUM,
    ))
    properties.size = size
    group = cuda_call(driver.cuMulticastCreate, properties)
    for device in range(2):
        cuda_call(driver.cuMulticastAddDevice, group, device)
    allocations = []
    for device, value in enumerate((0x12345678, 0x11111111)):
        cuda_call(driver.cuCtxSetCurrent, contexts[device])
        if source == "memory":
            member = cuda_call(driver.cuMemCreate, size, cuda_driver.allocation_properties(device), 0)
            address = cuda_driver.map_allocation(member, size, device)
            allocations.append((member, address))
        else:
            pool_properties = driver.CUmemPoolProps()
            pool_properties.allocType = driver.CUmemAllocationType.CU_MEM_ALLOCATION_TYPE_PINNED
            pool_properties.handleTypes = cuda_driver.POSIX_FD_HANDLE_TYPE
            pool_properties.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
            pool_properties.location.id = device
            pool = cuda_call(driver.cuMemPoolCreate, pool_properties)
            stream = cuda_call(driver.cuStreamCreate, 0)
            base = int(cuda_call(driver.cuMemAllocFromPoolAsync, 2 * size, pool, stream))
            cuda_call(driver.cuStreamSynchronize, stream)
            address = (base + size - 1) // size * size
            allocations.append((pool, stream, base))
        cuda_call(driver.cuMemsetD32, address, value, size // 4)
        cuda_call(driver.cuCtxSynchronize)
        if source == "memory":
            cuda_call(driver.cuMulticastBindMem_v2, group, device, 0, member, 0, size, 0)
        elif version == "v2":
            cuda_call(driver.cuMulticastBindAddr_v2, group, device, 0, address, size, 0)
        else:
            cuda_call(driver.cuMulticastBindAddr, group, 0, address, size, 0)
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    multicast_address = cuda_driver.map_allocation(group, size, 0)
    current = records()
    bindings = [record["multicast_binding"] for record in current if "multicast_binding" in record]
    assert len(bindings) == 2, bindings
    assert {binding["device"] for binding in bindings} == {0, 1}, bindings
    if source == "memory":
        members = {record["allocation"]["location"]["id"]: record["allocation"]["allocation"]
                   for record in current if "allocation" in record}
        for binding in bindings:
            assert binding["source"]["memory"] == {
                "allocation": members[binding["device"]], "offset": 0,
            }, binding
    else:
        assert all(binding["source"]["address"]["tracked_member"] is None
                   for binding in bindings), bindings
    assert all(binding["version"] == version for binding in bindings), bindings
    read_multicast(multicast_address)
    print("ready", flush=True)
    assert input() == "continue"
    assert [record["multicast_binding"] for record in records()
            if "multicast_binding" in record] == bindings
    read_multicast(multicast_address)
    for device, allocation in enumerate(allocations):
        cuda_call(driver.cuCtxSetCurrent, contexts[device])
        cuda_call(driver.cuMulticastUnbind, group, device, 0, size)
        if source == "memory":
            member, address = allocation
            cuda_driver.destroy_mapped_allocation(address, size, member)
        else:
            pool, stream, base = allocation
            cuda_call(driver.cuMemFreeAsync, base, stream)
            cuda_call(driver.cuStreamSynchronize, stream)
            cuda_call(driver.cuStreamDestroy, stream)
            cuda_call(driver.cuMemPoolDestroy, pool)
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    cuda_driver.destroy_mapped_allocation(multicast_address, size, group)
    for device in range(2):
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


if __name__ == "__main__":
    run_worker(*sys.argv[1:])
