#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Memory IPC sharing, reference counts, and host-carrier lifecycle regression."""

import ctypes as c
import subprocess
import sys

from protocol_client import LIFECYCLE, command
from support import driver


class Handle(c.Structure):
    _fields_ = [("bytes", c.c_ubyte * 64)]


cuda = driver()
u64 = c.c_uint64
cuda.cuMemAlloc_v2.argtypes = [c.POINTER(u64), c.c_size_t]
cuda.cuMemFree_v2.argtypes = [u64]
cuda.cuMemGetAddressRange_v2.argtypes = [c.POINTER(u64), c.POINTER(c.c_size_t), u64]
cuda.cuIpcGetMemHandle.argtypes = [c.POINTER(Handle), u64]
cuda.cuIpcOpenMemHandle_v2.argtypes = [c.POINTER(u64), Handle, c.c_uint]
cuda.cuIpcCloseMemHandle.argtypes = [u64]


def check_import(virtual_ipc_mem_handle):
    address, repeated = u64(), u64()
    assert cuda.cuIpcOpenMemHandle_v2(c.byref(address), virtual_ipc_mem_handle, 0) != 0
    assert cuda.cuIpcOpenMemHandle_v2(c.byref(address), Handle(), 1) != 0
    cuda.fakeFailNext(b"cuMemAddressReserve")
    address.value = 123
    assert cuda.cuIpcOpenMemHandle_v2(c.byref(address), virtual_ipc_mem_handle, 1) != 0
    assert address.value == 123
    assert cuda.fakeLiveAllocations() == 0
    assert not command("inspect")["records"]
    assert cuda.cuIpcOpenMemHandle_v2(c.byref(address), virtual_ipc_mem_handle, 1) == 0
    assert cuda.cuIpcOpenMemHandle_v2(c.byref(repeated), virtual_ipc_mem_handle, 1) == 0
    assert address.value == repeated.value
    assert cuda.fakeMappedCount() == 1
    assert cuda.cuIpcGetMemHandle(c.byref(Handle()), address) != 0
    assert cuda.fakeExportCalls() == 0
    assert cuda.cuMemFree_v2(address) != 0
    assert cuda.cuIpcCloseMemHandle(address) == 0
    assert cuda.fakeMappedCount() == 1
    assert cuda.cuIpcCloseMemHandle(address) == 0
    assert not command("inspect")["records"]
    assert cuda.cuIpcCloseMemHandle(address) != 0


if len(sys.argv) > 1:
    check_import(Handle.from_buffer_copy(bytes.fromhex(sys.argv[1])))
else:
    for failure in (b"cuMemCreate", b"cuMemAddressReserve"):
        failed = u64(123)
        cuda.fakeFailNext(failure)
        assert cuda.cuMemAlloc_v2(c.byref(failed), 17) != 0
        assert failed.value == 123
        assert cuda.fakeLiveAllocations() == 0
        assert cuda.fakeMappedCount() == 0
        assert not command("inspect")["records"]
    shared, private = u64(), u64()
    assert cuda.cuMemAlloc_v2(c.byref(shared), 17) == 0
    assert cuda.cuMemAlloc_v2(c.byref(private), 17) == 0
    base, size = u64(), c.c_size_t()
    assert cuda.cuMemGetAddressRange_v2(c.byref(base), c.byref(size), shared.value + 1) == 0
    assert base.value == shared.value and size.value == 17
    virtual_ipc_mem_handle, repeated = Handle(), Handle()
    assert cuda.cuIpcGetMemHandle(c.byref(virtual_ipc_mem_handle), shared.value + 1) != 0
    assert cuda.cuIpcGetMemHandle(c.byref(virtual_ipc_mem_handle), shared) == 0
    assert cuda.cuIpcGetMemHandle(c.byref(repeated), shared) == 0
    assert bytes(virtual_ipc_mem_handle) == bytes(repeated)
    assert cuda.fakeExportCalls() == 1
    subprocess.run(
        [sys.executable, __file__, bytes(virtual_ipc_mem_handle).hex()], check=True
    )
    for operation in LIFECYCLE[:3]:
        command(operation)
    assert cuda.fakeMappedCount() == 1  # Private malloc stays native-owned.
    assert cuda.fakeCopiedToHost() > 0
    assert cuda.cuMemAlloc_v2(c.byref(base), 32) != 0
    for operation in LIFECYCLE[3:]:
        command(operation)
    assert cuda.fakeMappedCount() == 2
    subprocess.run(
        [sys.executable, __file__, bytes(virtual_ipc_mem_handle).hex()], check=True
    )
    cuda.fakeFailNext(b"cuCtxSynchronize")
    assert cuda.cuMemFree_v2(shared) != 0
    assert cuda.fakeMappedCount() == 2
    assert cuda.cuMemFree_v2(shared) == 0
    assert cuda.cuMemFree_v2(private) == 0
    assert not command("inspect")["records"]
    print(
        "PASS memory IPC: private memory, peer virtual handles, repeat opens, restore, cleanup"
    )
