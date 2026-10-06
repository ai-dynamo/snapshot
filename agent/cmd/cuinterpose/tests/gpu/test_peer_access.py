# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Check peer kernel access through malloc and memory IPC across shim reconstruction."""

import ctypes
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading

import pytest
from cuda.bindings import driver, runtime

import cuda_driver
from cuda_driver import cuda_call


@pytest.mark.gpu
@pytest.mark.parametrize("case", [
    "malloc-before", "malloc-after", "malloc-runtime", "malloc-checkpoint", "disable",
    "context-lifetime", "concurrent", "ipc", "ipc-reordered", "ipc-isolated", "ipc-checkpoint",
    # Device permissions intentionally allow more than native context isolation.
    # These importing-context peer grants are shim extensions.
    "ipc-peer-before", "ipc-peer-after",
])
def test_peer_access(case, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {"CUINTERPOSE_SOCKET_DIR": str(control), "LD_PRELOAD": str(tools.interposer)}
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), case, str(tools.coordinator)],
        env=env, capture_output=True, text=True, timeout=180,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    print(result.stdout, end="")


def increment(address):
    # A kernel must access the peer pointer directly. A driver copy can take a
    # different path and succeed even when the GPU cannot dereference the pointer.
    ptx = b"""
.version 7.0
.target sm_80
.address_size 64
.visible .entry increment(.param .u64 address) {
    .reg .u64 pointer;
    .reg .u32 value;
    ld.param.u64 pointer, [address];
    ld.global.u32 value, [pointer];
    add.u32 value, value, 1;
    st.global.u32 [pointer], value;
    ret;
}
"""
    module = cuda_call(driver.cuModuleLoadData, ptx)
    kernel = cuda_call(driver.cuModuleGetFunction, module, b"increment")
    argument = ctypes.c_uint64(int(address))
    arguments = (ctypes.c_void_p * 1)(ctypes.addressof(argument))
    cuda_call(driver.cuLaunchKernel, kernel, 1, 1, 1, 1, 1, 1, 0, 0,
              ctypes.addressof(arguments), 0)
    cuda_call(driver.cuCtxSynchronize)
    cuda_call(driver.cuModuleUnload, module)


def checkpoint_cuda(*pids):
    # Run outside every target process so its CUDA calls can be suspended.
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), "checkpoint", *map(str, pids)],
        env=env, check=True, timeout=70,
    )


def reconstruct(coordinator, *pids, native_checkpoint=False):
    control = Path(os.environ["CUINTERPOSE_SOCKET_DIR"])
    checkpoint = control / "checkpoint"
    checkpoint.mkdir()
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    for phase in ("--prepare", "--restore"):
        command = [coordinator, phase, "--socket-dir", str(control),
                   "--checkpoint-dir", str(checkpoint)]
        for pid in pids:
            command.extend(["--process", str(pid)])
        subprocess.run(command, env=env, check=True, timeout=20)
        if phase == "--prepare" and native_checkpoint:
            checkpoint_cuda(*pids)


def malloc_peer_access(case, coordinator):
    cuda_call(driver.cuInit, 0)
    assert cuda_call(driver.cuDeviceCanAccessPeer, 1, 0), "requires GPU peer access"
    contexts = [cuda_call(driver.cuDevicePrimaryCtxRetain, device) for device in range(2)]
    if case == "malloc-before":
        cuda_call(driver.cuCtxSetCurrent, contexts[1])
        cuda_call(driver.cuCtxEnablePeerAccess, contexts[0], 0)
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    address = cuda_call(driver.cuMemAlloc, 4)
    cuda_driver.write_bytes(address, (0).to_bytes(4, "little"))
    # Mark the allocation shared so reconstruction actually rebuilds its backing.
    cuda_call(driver.cuIpcGetMemHandle, address)
    cuda_call(driver.cuCtxSetCurrent, contexts[1])
    if case in ("malloc-after", "malloc-checkpoint"):
        cuda_call(driver.cuCtxEnablePeerAccess, contexts[0], 0)
    elif case == "malloc-runtime":
        assert runtime.cudaSetDevice(1)[0] == runtime.cudaError_t.cudaSuccess
        assert runtime.cudaDeviceEnablePeerAccess(0, 0)[0] == runtime.cudaError_t.cudaSuccess
    increment(address)
    reconstruct(coordinator, os.getpid(), native_checkpoint=case == "malloc-checkpoint")
    increment(address)
    # Replaying existing permissions is insufficient: the restored relationship
    # must also apply to allocations created after reconstruction.
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    fresh = cuda_call(driver.cuMemAlloc, 4)
    cuda_driver.write_bytes(fresh, (0).to_bytes(4, "little"))
    cuda_call(driver.cuCtxSetCurrent, contexts[1])
    increment(fresh)
    status, = driver.cuCtxEnablePeerAccess(contexts[0], 0)
    assert status == driver.CUresult.CUDA_ERROR_PEER_ACCESS_ALREADY_ENABLED
    increment(fresh)
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    cuda_driver.assert_bytes(fresh, (2).to_bytes(4, "little"), "post-restore allocation")
    cuda_call(driver.cuMemFree, fresh)
    cuda_driver.assert_bytes(address, (2).to_bytes(4, "little"), case)
    cuda_call(driver.cuMemFree, address)
    cuda_call(driver.cuCtxSetCurrent, 0)
    for device in range(2):
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


def assert_ungranted(address):
    location = driver.CUmemLocation()
    location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE
    location.id = 1
    assert cuda_call(driver.cuMemGetAccess, location, address) == driver.CUmemAccess_flags.CU_MEM_ACCESS_FLAGS_PROT_NONE


def peer_lifetime(case):
    cuda_call(driver.cuInit, 0)
    contexts = [cuda_call(driver.cuCtxCreate, None, 0, device) for device in range(2)]
    for target in (0, 1):
        cuda_call(driver.cuCtxSetCurrent, contexts[1])
        status, = driver.cuCtxEnablePeerAccess(contexts[0], 1)
        assert status == driver.CUresult.CUDA_ERROR_INVALID_VALUE
        cuda_call(driver.cuCtxSetCurrent, contexts[0])
        address = cuda_call(driver.cuMemAlloc, 4)
        assert_ungranted(address)
        cuda_call(driver.cuMemFree, address)
        cuda_call(driver.cuCtxSetCurrent, contexts[1])
        cuda_call(driver.cuCtxEnablePeerAccess, contexts[0], 0)
        if case == "disable":
            cuda_call(driver.cuCtxDisablePeerAccess, contexts[0])
        else:
            cuda_call(driver.cuCtxDestroy, contexts[target])
            contexts[target] = cuda_call(driver.cuCtxCreate, None, 0, target)
        cuda_call(driver.cuCtxSetCurrent, contexts[0])
        address = cuda_call(driver.cuMemAlloc, 4)
        assert_ungranted(address)
        cuda_driver.write_bytes(address, (0).to_bytes(4, "little"))
        cuda_call(driver.cuCtxSetCurrent, contexts[1])
        cuda_call(driver.cuCtxEnablePeerAccess, contexts[0], 0)
        increment(address)
        cuda_call(driver.cuCtxDisablePeerAccess, contexts[0])
        cuda_call(driver.cuCtxSetCurrent, contexts[0])
        cuda_driver.assert_bytes(address, (1).to_bytes(4, "little"), case)
        cuda_call(driver.cuMemFree, address)
    for context in contexts:
        cuda_call(driver.cuCtxDestroy, context)


def concurrent_malloc():
    cuda_call(driver.cuInit, 0)
    contexts = [cuda_call(driver.cuDevicePrimaryCtxRetain, device) for device in range(2)]
    barrier = threading.Barrier(2)
    pointers, failures = [], []

    def allocate():
        try:
            cuda_call(driver.cuCtxSetCurrent, contexts[0])
            barrier.wait()
            for _ in range(20):
                address = cuda_call(driver.cuMemAlloc, 4)
                cuda_driver.write_bytes(address, (0).to_bytes(4, "little"))
                pointers.append(address)
        except Exception as error:
            failures.append(error)

    worker = threading.Thread(target=allocate)
    worker.start()
    cuda_call(driver.cuCtxSetCurrent, contexts[1])
    barrier.wait()
    cuda_call(driver.cuCtxEnablePeerAccess, contexts[0], 0)
    worker.join()
    assert not failures, failures
    for address in pointers:
        increment(address)
    cuda_call(driver.cuCtxSetCurrent, contexts[0])
    for address in pointers:
        cuda_driver.assert_bytes(address, (1).to_bytes(4, "little"), "concurrent allocation")
        cuda_call(driver.cuMemFree, address)
    cuda_call(driver.cuCtxSetCurrent, 0)
    for device in range(2):
        cuda_call(driver.cuDevicePrimaryCtxRelease, device)


def ipc_import(fd, device, case):
    channel = socket.socket(fileno=int(fd))
    channel.settimeout(120 if case == "ipc-checkpoint" else 30)
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, int(device))
    peer = None
    if case.startswith("ipc-peer-"):
        peer = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
        if case == "ipc-peer-before":
            cuda_call(driver.cuCtxSetCurrent, peer)
            cuda_call(driver.cuCtxEnablePeerAccess, context, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    handle = driver.CUipcMemHandle()
    data = channel.recv(64, socket.MSG_WAITALL)
    assert len(data) == 64, "missing IPC handle"
    ctypes.memmove(handle.getPtr(), data, 64)
    address = cuda_call(driver.cuIpcOpenMemHandle, handle,
                        driver.CUipcMem_flags.CU_IPC_MEM_LAZY_ENABLE_PEER_ACCESS)
    if os.environ.get("LD_PRELOAD"):
        backing = cuda_call(driver.cuMemRetainAllocationHandle, address)
        properties = cuda_call(driver.cuMemGetAllocationPropertiesFromHandle, backing)
        print(f"imported backing location: {properties.location.type.name} "
              f"{properties.location.id}", flush=True)
        cuda_call(driver.cuMemRelease, backing)
    increment(address)
    if peer is not None:
        cuda_call(driver.cuCtxSetCurrent, peer)
        if case == "ipc-peer-after":
            cuda_call(driver.cuCtxEnablePeerAccess, context, 0)
        increment(address)
    channel.sendall(b"ready")
    assert channel.recv(1) == b"r"
    increment(address)
    cuda_call(driver.cuCtxSetCurrent, context)
    cuda_call(driver.cuIpcCloseMemHandle, address)
    cuda_call(driver.cuDevicePrimaryCtxRelease, int(device))
    if peer is not None:
        cuda_call(driver.cuDevicePrimaryCtxRelease, 0)
    channel.close()


def ipc_peer_access(case, coordinator):
    # Exercise driver-reported imported allocation locations when device ordinals
    # differ between processes, including an exporter GPU hidden from the importer.
    visible = os.environ.get("CUDA_VISIBLE_DEVICES", "0,1").split(",")[:2]
    env = os.environ.copy()
    importer_device = "1"
    if case == "ipc-reordered":
        env["CUDA_VISIBLE_DEVICES"] = ",".join(reversed(visible))
        importer_device = "0"
    elif case == "ipc-isolated":
        os.environ["CUDA_VISIBLE_DEVICES"] = visible[0]
        env["CUDA_VISIBLE_DEVICES"] = visible[1]
        importer_device = "0"
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    address = cuda_call(driver.cuMemAlloc, 4)
    cuda_driver.write_bytes(address, (0).to_bytes(4, "little"))
    handle = cuda_call(driver.cuIpcGetMemHandle, address)
    parent, child = socket.socketpair()
    parent.settimeout(30)
    process = subprocess.Popen(
        [sys.executable, str(Path(__file__).resolve()), "import", str(child.fileno()),
         importer_device, case], env=env, pass_fds=(child.fileno(),),
    )
    child.close()
    try:
        parent.sendall(ctypes.string_at(handle.getPtr(), 64))
        assert parent.recv(5, socket.MSG_WAITALL) == b"ready"
        reconstruct(coordinator, os.getpid(), process.pid,
                    native_checkpoint=case == "ipc-checkpoint")
        parent.sendall(b"r")
        assert process.wait(timeout=30) == 0
        count = 3 if case.startswith("ipc-peer-") else 2
        cuda_driver.assert_bytes(address, count.to_bytes(4, "little"), case)
    finally:
        parent.close()
        if process.poll() is None:
            process.kill()
            process.wait()
    cuda_call(driver.cuMemFree, address)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    if sys.argv[1] == "checkpoint":
        cuda_driver.native_checkpoint(
            tuple(map(int, sys.argv[2:])), command_timeout_seconds=20,
            checkpoint_timeout_seconds=60,
        )
        raise SystemExit(0)
    if sys.argv[1] == "import":
        ipc_import(*sys.argv[2:])
        raise SystemExit(0)
    case, coordinator = sys.argv[1:]
    if case in ("disable", "context-lifetime"):
        peer_lifetime(case)
    elif case == "concurrent":
        concurrent_malloc()
    elif case.startswith("malloc-"):
        malloc_peer_access(case, coordinator)
    else:
        ipc_peer_access(case, coordinator)
    print(f"{case}: passed", flush=True)
