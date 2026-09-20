#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Process-isolated tracking and ownership scenarios against the packaged shim."""

import ctypes as c
import os
import subprocess
import sys
import threading
import resource
import socket
import time
from support import driver, props, Properties, Location
from protocol_client import LIFECYCLE, command

cuda = driver()
assert cuda.cuInit(0) == 0
u64, size = c.c_uint64, c.c_size_t
cuda.cuMemMap.argtypes = [u64, size, size, u64, u64]
cuda.cuMemUnmap.argtypes = [u64, size]
cuda.cuMemRetainAllocationHandle.argtypes = [c.POINTER(u64), c.c_void_p]
cuda.cuMemImportFromShareableHandle.argtypes = [c.POINTER(u64), c.c_void_p, c.c_uint]
cuda.cuMemGetAllocationPropertiesFromHandle.argtypes = [c.POINTER(Properties), u64]
cuda.fakeCopiedToHost.restype = u64
cuda.fakeCopiedToDevice.restype = u64


def main():
    mode = sys.argv[1]
    length, address = 4096, 0x10000000
    cuda.cuCtxSetCurrent.argtypes = [c.c_void_p]
    cuda.fakeCurrentContext.restype = c.c_void_p
    if mode == "no-context":
        assert cuda.cuCtxSetCurrent(None) == 0
    handle = u64()
    if mode == "unsupported":
        for kind in (8, 9):
            creation = Properties(1, kind, Location(1, 0), None)
            handle.value = 123
            assert cuda.cuMemCreate(c.byref(handle), length, c.byref(creation), 0) == 801
            assert handle.value == 123
        assert cuda.fakeLiveAllocations() == 0
        assert command("inspect") == {"records": []}
        return
    if mode == "raw":
        with open("/dev/null", "rb") as foreign:
            for kind in (1, 8):
                raw = u64(123)
                assert cuda.cuMemImportFromShareableHandle(c.byref(raw), foreign.fileno(), kind) == 801
                assert raw.value == 123
        assert cuda.fakeLiveAllocations() == 0
        assert command("begin_checkpoint") == {"records": []}
        for operation in LIFECYCLE:
            command(operation)
        return
    assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
    if mode != "no-context":
        assert cuda.cuMemMap(address, length, 0, handle, 0) == 0
    if mode == "accept-exhaustion":
        peer = socket.socket(socket.AF_UNIX)
        resource.setrlimit(resource.RLIMIT_NOFILE, (64, 64))
        opened = []
        try:
            while True:
                opened.append(os.open("/dev/null", os.O_RDONLY))
        except OSError:
            pass
        started = time.process_time()
        peer.connect(os.path.join(os.environ["SNAPSHOT_CONTROL_DIR"],
                                  f"cuinterpose-{os.getpid()}.sock"))
        time.sleep(0.3)
        assert time.process_time() - started < 0.15, "accept spins under FD exhaustion"
        for fd in opened:
            os.close(fd)
        peer.close()
        assert command("inspect")["records"]
        return
    if mode == "ranges":
        class Access(c.Structure):
            _fields_ = [("location", Location), ("flags", c.c_uint)]
        cuda.cuMemSetAccess.argtypes = [u64, size, c.POINTER(Access), size]
        second = u64()
        assert cuda.cuMemCreate(c.byref(second), length, c.byref(props), 0) == 0
        assert cuda.cuMemMap(address + length, length, 0, second, 0) == 0
        access = Access(Location(1, 0), 3)
        assert cuda.cuMemSetAccess(address, 2 * length, c.byref(access), 1) == 0
        records = command("inspect")["records"]
        mappings = [r["mapping"] for r in records if "mapping" in r]
        assert len(mappings) == 2 and all(m["access"] == [[1, 0, 3]] for m in mappings), mappings
        # Failed driver calls leave both records unchanged.
        cuda.fakeFailNext(b"cuMemSetAccess")
        access.flags = 1
        assert cuda.cuMemSetAccess(address, 2 * length, c.byref(access), 1) != 0
        assert command("inspect")["records"] == records
        assert cuda.cuMemRelease(handle) == 0
        assert cuda.cuMemRelease(second) == 0
        assert cuda.cuMemUnmap(address, 2 * length) == 0
        assert command("inspect")["records"] == []
        return
    if mode == "retain-release-failure":
        cuda.fakeFailNext(b"cuMemRelease")
        cuda.cuMemRetainAllocationHandle(c.byref(u64()), address)
        raise AssertionError("failed redundant release returned to application")
    if mode == "checkpoint-entry":
        before = command("inspect")
        command("prepare_multicast", False)  # Reading alone never reserves state.
        assert command("begin_checkpoint") == before
        command("begin_checkpoint", False)
        assert cuda.cuMemRelease(handle) == 600
        assert cuda.cuMemUnmap(address, length) == 600
        private = Properties.from_buffer_copy(props)
        private.handles = 0
        assert cuda.cuMemCreate(c.byref(u64()), length, c.byref(private), 0) == 600
        assert command("inspect") == before
        for operation in LIFECYCLE:
            command(operation)
        assert cuda.cuMemUnmap(address, length) == 0
        assert cuda.cuMemRelease(handle) == 0
        return
    if mode == "exhaustion":
        resource.setrlimit(resource.RLIMIT_NOFILE, (64, 64))
        opened = []
        try:
            while True:
                opened.append(os.open("/dev/null", os.O_RDONLY))
        except OSError:
            pass
        virtual_shareable_handle = c.c_int(-1)
        assert (
            cuda.cuMemExportToShareableHandle(
                c.byref(virtual_shareable_handle), handle, 1, 0
            )
            != 0
        )
        for fd in opened:
            os.close(fd)
        allocation = next(record["allocation"] for record in command("inspect")["records"]
                          if "allocation" in record)
        assert allocation["virtual_allocation_handle_count"] == 1
        assert (
            cuda.cuMemExportToShareableHandle(
                c.byref(virtual_shareable_handle), handle, 1, 0
            )
            == 0
        )
        os.close(virtual_shareable_handle.value)
        return
    if mode == "tracking":
        retained = u64()
        assert cuda.cuMemRetainAllocationHandle(c.byref(retained), address) == 0
        assert retained.value != handle.value
        allocation = next(record["allocation"] for record in command("inspect")["records"]
                          if "allocation" in record)
        assert allocation["virtual_allocation_handle_count"] == 2
        properties = Properties()
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(properties), retained) == 0
        assert properties.handles == 1
        assert cuda.cuMemUnmap(address, length // 2) != 0
        assert cuda.cuMemRelease(retained) == 0
        assert cuda.cuMemRelease(handle) == 0
        records = command("inspect")["records"]
        assert sum("allocation" in record for record in records) == 1
        assert cuda.cuMemUnmap(address, length) == 0
        assert command("inspect")["records"] == []
        for _ in range(100):
            assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
            assert cuda.cuMemRelease(handle) == 0
        assert command("inspect")["records"] == []
        return
    if mode == "access":
        class Access(c.Structure):
            _fields_ = [("location", Location), ("flags", c.c_uint)]
        cuda.cuMemSetAccess.argtypes = [u64, size, c.POINTER(Access), size]
        for device in (0, 1):
            access = Access(Location(1, device), 3)
            assert cuda.cuMemSetAccess(address, length, c.byref(access), 1) == 0
        records = command("inspect")["records"]
        mapping = next(record["mapping"] for record in records if "mapping" in record)
        assert {entry[1] for entry in mapping["access"]} == {0, 1}
        return
    if mode == "exports":
        descriptors = []
        def export():
            virtual_shareable_handle = c.c_int(-1)
            assert (
                cuda.cuMemExportToShareableHandle(
                    c.byref(virtual_shareable_handle), handle, 1, 0
                )
                == 0
            )
            descriptors.append(virtual_shareable_handle.value)
        workers = [threading.Thread(target=export) for _ in range(8)]
        for worker in workers:
            worker.start()
        for worker in workers:
            worker.join()
        assert len(descriptors) == 8 and cuda.fakeExportCalls() == 1
        for fd in descriptors:
            imported = u64()
            assert cuda.cuMemImportFromShareableHandle(c.byref(imported), fd, 1) == 0
            assert cuda.cuMemRelease(imported) == 0
            os.close(fd)
        allocation = next(record["allocation"] for record in command("inspect")["records"]
                          if "allocation" in record)
        assert allocation["virtual_allocation_handle_count"] == 1
        return
    shared = mode in ("shared", "no-context")
    if shared:
        virtual_shareable_handle = c.c_int(-1)
        assert (
            cuda.cuMemExportToShareableHandle(
                c.byref(virtual_shareable_handle), handle, 1, 0
            )
            == 0
        )
        if mode != "no-context":
            imported = u64()
            assert (
                cuda.cuMemImportFromShareableHandle(
                    c.byref(imported), virtual_shareable_handle.value, 1
                )
                == 0
            )
            allocation = next(record["allocation"] for record in command("inspect")["records"]
                              if "allocation" in record)
            assert allocation["virtual_allocation_handle_count"] == 2
        os.close(virtual_shareable_handle.value)
        if mode == "no-context":
            assert cuda.cuCtxSetCurrent(7) == 0
    else:
        assert mode == "private-released"
        assert cuda.cuMemRelease(handle) == 0
    command("begin_checkpoint")
    for operation in LIFECYCLE:
        command(operation)
        if operation == "prepare_unicast":
            assert cuda.fakeMappedCount() == (0 if shared else 1)
    assert cuda.fakeCopiedToHost() == (length if shared else 0)
    assert cuda.fakeCopiedToDevice() == (length if shared else 0)
    assert cuda.fakeRegisteredHostRanges() == 0
    assert bool(cuda.fakePrimaryContextRetainCalls()) == (mode == "no-context")
    assert cuda.fakePrimaryContextsHeld() == 0
    assert cuda.fakeCurrentContext() == (7 if mode == "no-context" else 1)
    assert cuda.fakeMappedCount() == (0 if mode == "no-context" else 1)
    command("inspect")
    print("PASS lifecycle", mode)


if __name__ == "__main__":
    main()
