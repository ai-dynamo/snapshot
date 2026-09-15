#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Process-isolated carrier failures, zero handles, and native pass-through."""

import ctypes as c
import os
import socket
import sys
import threading
import time
from fork import cuda, props, stats, Properties
from protocol_client import LIFECYCLE, command, receive, seal_ticket, send

CREATE, MAP, ACCESS, D2H, H2D, SYNC, DESTROY, UNMAP, FREE = range(9)
RELEASE, PROPERTIES, SET_CONTEXT, RETAIN_PRIMARY, RELEASE_PRIMARY, REGISTER, UNREGISTER, RETAIN = range(9, 17)
u64, size = c.c_uint64, c.c_size_t
cuda.cuMemMap.argtypes = [u64, size, size, u64, u64]
cuda.cuMemUnmap.argtypes = [u64, size]
cuda.cuMemRetainAllocationHandle.argtypes = [c.POINTER(u64), c.c_void_p]
cuda.cuMemImportFromShareableHandle.argtypes = [c.POINTER(u64), c.c_void_p, c.c_uint]
cuda.cuMemGetAllocationPropertiesFromHandle.argtypes = [c.POINTER(Properties), u64]
cuda.carrier_fail.argtypes = [c.c_int, c.c_int, c.c_int, c.c_int]
cuda.carrier_zero_real.restype = u64
cuda.fakeCopiedToHost.restype = u64
cuda.fakeCopiedToDevice.restype = u64
cuda.fakeAllocationRefs.argtypes = [u64]
cuda.fakeCurrentContext.restype = c.c_void_p
cuda.cuCtxSetCurrent.argtypes = [c.c_void_p]


def main():
    mode = sys.argv[1]
    length = 1 << 20
    if mode in ("import-failure", "zero-import"):
        # A real creator outside this shim's allocation table serves a raw FD.
        # Property lookup after import must roll back the new driver reference.
        native_prop = Properties.from_buffer_copy(props)
        native_prop.handles = 0
        native, raw = u64(), c.c_int(-1)
        assert cuda.cuMemCreate(c.byref(native), length, c.byref(native_prop), 0) == 0
        assert cuda.cuMemExportToShareableHandle(c.byref(raw), native, 1, 0) == 0
        endpoint = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/peer.sock"
        creator, allocation = b"1" * 16, b"a" * 16
        fd = seal_ticket(dict(creator=creator, allocation=allocation, endpoint=endpoint,
                              resource={"kind": "unicast"}))
        with socket.socket(socket.AF_UNIX) as listener:
            listener.bind(endpoint)
            listener.listen()
            errors = []
            def serve():
                try:
                    for _ in range(2 if mode == "zero-import" else 1):
                        connection, _ = listener.accept()
                        with connection:
                            connection.settimeout(5)
                            request = receive(connection)
                            assert request == dict(kind="export", participant=creator,
                                                   resource="unicast", allocation=allocation)
                            send(connection, dict(participant=creator, result={"Ok": dict(
                                kind="export", resource="unicast", allocation=allocation)}), raw.value)
                except BaseException as error:
                    errors.append(error)
            worker = threading.Thread(target=serve)
            worker.start()
            cuda.carrier_reset()
            if mode == "import-failure":
                cuda.carrier_fail(PROPERTIES, 1, 0, 711)
            else:
                cuda.carrier_zero_import()
            imported = u64(0xAAAA)
            result = cuda.cuMemImportFromShareableHandle(c.byref(imported), c.c_void_p(fd), 1)
            if mode == "import-failure":
                assert result == 711 and imported.value == 0xAAAA
            else:
                assert result == 0
                assert imported.value & 0xFFFF000000000000 == 0xD94D000000000000
                assert cuda.cuMemMap(0x10000000, length, 0, imported, 0) == 0
                assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), imported) == 0
                for operation in LIFECYCLE[:4]:
                    command(operation)
                cuda.carrier_zero_import()
                for operation in LIFECYCLE[4:]:
                    command(operation)
                assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), imported) == 0
                assert cuda.cuMemUnmap(0x10000000, length) == 0
                assert cuda.cuMemRelease(imported) == 0
            worker.join(5)
            assert not worker.is_alive() and not errors
        assert stats().allocations == 0 and stats().handles == 0 and stats().raw == 0
        os.unlink(endpoint)
        assert cuda.fakeAllocationRefs(native) == 1
        assert cuda.carrier_calls(RELEASE) == (2 if mode == "zero-import" else 1)
        assert cuda.cuMemRelease(native) == 0
        os.close(fd)
        os.close(raw.value)
        assert cuda.fakeLiveAllocations() == 0
        print("PASS carrier", mode)
        return
    count = 1 if mode in ("zero", "zero-released", "zero-member", "native", "release-error") else 3
    handles, tickets = [], []
    if mode in ("context-failure", "save-pending", "load-pending"):
        assert cuda.cuCtxSetCurrent(None) == 0
    if mode.startswith("zero"):
        cuda.carrier_zero()
    for index in range(count):
        handle = u64()
        assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
        assert cuda.cuMemMap(0x10000000 + index * length, length, 0, handle, 0) == 0
        fd = c.c_int(-1)
        assert cuda.cuMemExportToShareableHandle(c.byref(fd), handle, 1, 0) == 0
        handles.append(handle)
        tickets.append(fd.value)
    if mode == "release-error":
        cuda.carrier_fail(RELEASE, 1, 0, 712)
        assert cuda.cuMemRelease(handles[0]) == 712
        assert stats().handles == 1
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), handles[0]) == 0
        assert cuda.cuMemRelease(handles[0]) == 0
        handles.clear()
    if mode == "retain-release":
        cuda.carrier_reset()
        cuda.carrier_fail(RELEASE, 1, 0, 712)
        alias = u64(0xAAAA)
        assert cuda.cuMemRetainAllocationHandle(c.byref(alias), c.c_void_p(0x10000000)) == 712
        assert alias.value == 0xAAAA and stats().phase == 5
        assert cuda.carrier_calls(RETAIN) == 1 and cuda.carrier_calls(RELEASE) == 1
        # The redundant reference is owned by failed state, not an invisible
        # live alias. Further calls cannot pretend normal tracking continues.
        assert cuda.cuMemRelease(handles[0]) == 600
        print("PASS carrier", mode)
        return
    if mode.startswith("zero"):
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), handles[0]) == 0
        alias = u64()
        assert cuda.cuMemImportFromShareableHandle(c.byref(alias), c.c_void_p(tickets[0]), 1) == 0
        assert cuda.cuMemRelease(alias) == 0
        assert cuda.cuMemRetainAllocationHandle(c.byref(alias), c.c_void_p(0x10000000)) == 0
        assert cuda.cuMemRelease(alias) == 0
        # The pinned fake counts handle + map; it cannot observe export-FD close.
        assert cuda.fakeAllocationRefs(cuda.carrier_zero_real()) == 2
    group = None
    if mode == "zero-member":
        class Multicast(c.Structure):
            _fields_ = [("devices", c.c_uint), ("size", size), ("handles", u64), ("flags", u64)]
        cuda.cuMulticastCreate.argtypes = [c.POINTER(u64), c.POINTER(Multicast)]
        cuda.cuMulticastAddDevice.argtypes = [u64, c.c_int]
        cuda.cuMulticastBindMem_v2.argtypes = [u64, c.c_int, size, u64, size, size, u64]
        cuda.cuMulticastUnbind.argtypes = [u64, c.c_int, size, size]
        group = u64()
        assert cuda.cuMulticastCreate(c.byref(group), c.byref(Multicast(1, length, 1, 0))) == 0
        assert cuda.cuMulticastAddDevice(group, 0) == 0
        assert cuda.cuMulticastBindMem_v2(group, 0, 0, handles[0], 0, length, 0) == 0
    if mode in ("zero-released", "zero-member"):
        assert cuda.cuMemRelease(handles[0]) == 0
        handles.clear()
    if mode in ("save-released", "save-pending"):
        for handle in handles:
            assert cuda.cuMemRelease(handle) == 0
        handles.clear()
    command("prepare_multicast")
    cuda.carrier_reset()
    if mode == "save-pending":
        cuda.carrier_pending_copies()
        command("save_allocations")  # Harness requires exit 127, not a returned error.
        raise AssertionError("unknown D2H completion returned")
    if mode in ("save-copy", "save-sync", "save-unmap", "save-released"):
        failed = {"save-copy": D2H, "save-sync": SYNC, "save-unmap": UNMAP,
                  "save-released": D2H}[mode]
        cuda.carrier_fail(failed, 2 if failed == D2H else 1, int(failed == UNMAP), 711)
        response = command("save_allocations", False)
        assert "CUDA error 711" in response
        assert stats().phase == 5
        assert cuda.fakeLiveAllocations() == count and cuda.fakeMappedCount() == count
        assert cuda.carrier_calls(UNMAP) == count and cuda.carrier_calls(FREE) == 1
        assert cuda.carrier_streams() == 0 and cuda.carrier_reservations() == 0
        assert cuda.fakeRegisteredHostRanges() == 0 and cuda.fakePrimaryContextsHeld() == 0
        if mode == "save-released":
            assert cuda.carrier_calls(RETAIN) == count and cuda.carrier_calls(RELEASE) == count
        print("PASS carrier", mode)
        return
    if mode == "context-failure":
        cuda.carrier_fail(SET_CONTEXT, 1, 0, 711)
        cuda.carrier_fail(RELEASE_PRIMARY, 1, 1, 712)
        response = command("save_allocations", False)
        assert "CUDA error 711" in response
        assert cuda.carrier_calls(RELEASE_PRIMARY) == 1
        assert cuda.fakePrimaryContextsHeld() == 0 and cuda.fakeRegisteredHostRanges() == 0
        print("PASS carrier", mode)
        return
    if mode == "timing":
        cuda.carrier_delay(MAP, 60000)
        cuda.carrier_delay(D2H, 10000)
        cuda.carrier_delay(SYNC, 10000)
    start = time.monotonic()
    response = command("save_allocations")
    duration_us = (time.monotonic() - start) * 1e6
    if mode == "timing":
        copy_us = response["copy_us"]
        assert copy_us >= 30000 and duration_us - copy_us >= 150000, (duration_us, copy_us)
        assert cuda.carrier_calls(SYNC) == 1
        assert cuda.carrier_calls(SET_CONTEXT) == 0
    command("prepare_unicast")
    if mode == "native":
        native_prop = Properties.from_buffer_copy(props)
        native_prop.handles = 0
        native, alias = u64(), u64()
        assert cuda.cuMemCreate(c.byref(native), length, c.byref(native_prop), 0) == 0
        assert cuda.cuMemMap(0x60000000, length, 0, native, 0) == 0
        class Access(c.Structure):
            _fields_ = [("kind", c.c_int), ("device", c.c_int), ("flags", c.c_uint)]
        cuda.cuMemSetAccess.argtypes = [u64, size, c.POINTER(Access), size]
        assert cuda.cuMemSetAccess(0x60000000, length, c.byref(Access(1, 0, 3)), 1) == 0
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), native) == 0
        assert cuda.cuMemRetainAllocationHandle(c.byref(alias), c.c_void_p(0x60000000)) == 0
        assert cuda.cuMemRelease(alias) == 0
        fd = c.c_int(-1)
        assert cuda.cuMemExportToShareableHandle(c.byref(fd), native, 1, 0) == 0
        assert cuda.cuMemImportFromShareableHandle(c.byref(alias), c.c_void_p(fd.value), 1) == 0
        assert stats().raw == 1
        assert cuda.cuMemRelease(alias) == 0 and stats().raw == 0
        os.close(fd.value)
        assert cuda.cuMemMap(0x10000000, length, 0, native, 0) != 0
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), handles[0]) == 600
        assert cuda.cuMemRelease(handles[0]) == 600
        assert cuda.cuMemUnmap(0x10000000, length) == 600
        assert cuda.cuMemUnmap(0x60000000, length) == 0
        assert cuda.cuMemRelease(native) == 0
    cuda.carrier_reset()
    if mode == "load-pending":
        cuda.fakeForgetHostRegistrations()
        cuda.carrier_pending_copies()
        command("load_allocations")  # Harness requires exit 127, not a returned error.
        raise AssertionError("unknown H2D completion returned")
    if mode.startswith("load-"):
        failed = {"load-create": CREATE, "load-map": MAP, "load-copy": H2D,
                  "load-sync": SYNC, "load-unmap": UNMAP, "load-cleanup": H2D}[mode]
        cuda.carrier_fail(failed, 2 if failed in (CREATE, MAP, H2D) else 1,
                          int(failed == UNMAP), 711)
        if mode == "load-cleanup":
            # Fail after performing cleanup so we can prove both continued
            # cleanup attempts and complete reference release.
            cuda.carrier_fail(DESTROY, 1, 1, 712)
            cuda.carrier_fail(UNMAP, 1, 1, 713)
            cuda.carrier_fail(FREE, 1, 1, 714)
        cuda.fakeForgetHostRegistrations()
        response = command("load_allocations", False)
        assert "CUDA error 711" in response
        assert stats().phase == 5
        assert cuda.fakeLiveAllocations() == 0 and cuda.fakeMappedCount() == 0
        assert cuda.carrier_streams() == 0 and cuda.carrier_reservations() == 0
        assert cuda.fakeRegisteredHostRanges() == 0 and cuda.fakePrimaryContextsHeld() == 0
        if mode == "load-cleanup":
            assert cuda.carrier_calls(DESTROY) == 1
            assert cuda.carrier_calls(UNMAP) == count
            assert cuda.carrier_calls(FREE) == 1 and cuda.carrier_calls(RELEASE) == count
        print("PASS carrier", mode)
        return
    if mode.startswith("zero"):
        cuda.carrier_zero()  # The first fresh backing allocation is also zero.
    cuda.fakeForgetHostRegistrations()
    command("load_allocations")
    assert cuda.carrier_calls(REGISTER) == 1
    for operation in LIFECYCLE[4:]:
        command(operation)
    assert cuda.fakeCopiedToHost() == count * length and cuda.fakeCopiedToDevice() == count * length
    if mode in ("zero-released", "zero-member"):
        assert cuda.carrier_calls(RELEASE) >= 1
        handle = u64()
        assert cuda.cuMemRetainAllocationHandle(c.byref(handle), c.c_void_p(0x10000000)) == 0
        handles.append(handle)
    if group is not None:
        assert cuda.cuMulticastUnbind(group, 0, 0, length) == 0
        assert cuda.cuMemRelease(group) == 0
    for index in range(count):
        assert cuda.cuMemUnmap(0x10000000 + index * length, length) == 0
    for handle in handles:
        assert cuda.cuMemGetAllocationPropertiesFromHandle(c.byref(Properties()), handle) == 0
        assert cuda.cuMemRelease(handle) == 0
    for fd in tickets:
        os.close(fd)
    assert cuda.fakeLiveAllocations() == 0 and cuda.fakeMappedCount() == 0
    assert cuda.carrier_streams() == 0 and cuda.carrier_reservations() == 0
    assert cuda.fakeRegisteredHostRanges() == 0 and cuda.fakePrimaryContextsHeld() == 0
    print("PASS carrier", mode)


if __name__ == "__main__":
    main()
