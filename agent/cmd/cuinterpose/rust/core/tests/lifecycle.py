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
from support import driver, props, Properties, Location
from protocol_client import LIFECYCLE, command

cuda = driver()
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
    creation = Properties(1, 8, Location(1, 0), None) if mode == "unsupported" else props
    assert cuda.cuMemCreate(c.byref(handle), length, c.byref(creation), 0) == 0
    if mode != "no-context":
        assert cuda.cuMemMap(address, length, 0, handle, 0) == 0
    if mode in ("raw", "unsupported"):
        if mode == "raw":
            with open("/dev/null", "rb") as foreign:
                raw = u64()
                assert cuda.cuMemImportFromShareableHandle(c.byref(raw), foreign.fileno(), 1) == 0
        coordinator = subprocess.run([
            os.environ["CUINTERPOSE_COORDINATOR"], "--prepare",
            "--checkpoint-dir", os.environ["SNAPSHOT_CONTROL_DIR"],
            "--control-dir", os.environ["SNAPSHOT_CONTROL_DIR"],
            "--process", str(os.getpid()),
        ], capture_output=True, text=True)
        assert coordinator.returncode != 0, coordinator
        inspection = command("inspect")
        assert inspection["live_raw_imports"] == (1 if mode == "raw" else 0)
        assert inspection["unsupported_creations"] == (1 if mode == "unsupported" else 0)
        assert cuda.fakeMappedCount() == 1
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
        allocation = next(record["allocation"] for record in command("inspect")["entries"]
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
        allocation = next(record["allocation"] for record in command("inspect")["entries"]
                          if "allocation" in record)
        assert allocation["virtual_allocation_handle_count"] == 2
        properties = Properties()
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(properties), retained) == 0
        assert properties.handles == 1
        assert cuda.cuMemMap(address + length // 2, length, 0, handle, 0) != 0
        assert cuda.cuMemUnmap(address, length // 2) != 0
        assert cuda.cuMemRelease(retained) == 0
        assert cuda.cuMemRelease(handle) == 0
        records = command("inspect")["entries"]
        assert sum("allocation" in record for record in records) == 1
        assert cuda.cuMemUnmap(address, length) == 0
        assert command("inspect")["entries"] == []
        for _ in range(100):
            assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
            assert cuda.cuMemRelease(handle) == 0
        assert command("inspect")["entries"] == []
        return
    if mode == "access":
        class Access(c.Structure):
            _fields_ = [("location", Location), ("flags", c.c_uint)]
        cuda.cuMemSetAccess.argtypes = [u64, size, c.POINTER(Access), size]
        for device in (0, 1):
            access = Access(Location(1, device), 3)
            assert cuda.cuMemSetAccess(address, length, c.byref(access), 1) == 0
        records = command("inspect")["entries"]
        mapping = next(record["mapping"] for record in records if "mapping" in record)
        assert {entry[1] for entry in mapping["access"]} == {0, 1}
        assert cuda.cuMemSetAccess(address, length // 2, c.byref(access), 1) != 0
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
        allocation = next(record["allocation"] for record in command("inspect")["entries"]
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
            allocation = next(record["allocation"] for record in command("inspect")["entries"]
                              if "allocation" in record)
            assert allocation["virtual_allocation_handle_count"] == 2
        os.close(virtual_shareable_handle.value)
        if mode == "no-context":
            assert cuda.cuCtxSetCurrent(7) == 0
    else:
        assert mode == "private-released"
        assert cuda.cuMemRelease(handle) == 0
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
