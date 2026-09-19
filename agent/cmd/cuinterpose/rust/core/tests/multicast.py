#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Multicast invariants against the Rust core and local fake CUDA."""

import ctypes as c
import os
import subprocess
import socket
import sys
import threading
from support import driver, props, Properties

cuda = driver()
from protocol_client import LIFECYCLE, command, request_export


class Multicast(c.Structure):
    _fields_ = [("devices", c.c_uint), ("size", c.c_size_t),
                ("handles", c.c_uint64), ("flags", c.c_uint64)]


u64, size = c.c_uint64, c.c_size_t
cuda.cuMulticastCreate.argtypes = [c.POINTER(u64), c.POINTER(Multicast)]
cuda.cuMulticastAddDevice.argtypes = [u64, c.c_int]
cuda.cuMulticastBindMem_v2.argtypes = [u64, c.c_int, size, u64, size, size, u64]
cuda.cuMulticastBindAddr_v2.argtypes = [u64, c.c_int, size, u64, size, u64]
cuda.cuMulticastUnbind.argtypes = [u64, c.c_int, size, size]
cuda.cuMemMap.argtypes = [u64, size, size, u64, u64]
cuda.cuMemUnmap.argtypes = [u64, size]
cuda.cuMemRetainAllocationHandle.argtypes = [c.POINTER(u64), c.c_void_p]
cuda.cuMemImportFromShareableHandle.argtypes = [c.POINTER(u64), c.c_void_p, c.c_uint]
cuda.fakeCopiedToHost.restype = u64
cuda.fakeCopiedToDevice.restype = u64
cuda.multicast_block_arm.argtypes = [c.c_int]


def replay():
    for operation in LIFECYCLE:
        command(operation)


def main():
    mode = sys.argv[1]
    member, group = u64(), u64()
    length = 1 << 20
    if mode == "create-output":
        group.value = 0xAAAA
        cuda.multicast_fail_create()
        assert cuda.cuMulticastCreate(c.byref(group), c.byref(Multicast(1, length, 1, 0))) == 110
        assert group.value == 0x456
        assert command("inspect")["records"] == []
        assert cuda.fakeLiveAllocations() == 0
        # Failure must also finish its in-flight reservation.
        command("inspect")
        group.value = 0
    if mode == "unsupported":
        assert cuda.cuMulticastCreate(c.byref(group), c.byref(Multicast(1, length, 8, 0))) == 0
        inspection = command("inspect")
        assert inspection["records"] == [] and inspection["unsupported_creations"] == 1
        assert cuda.cuMemRelease(group) == 0
        result = subprocess.run([
            os.environ["CUINTERPOSE_COORDINATOR"], "--prepare",
            "--control-dir", os.environ["SNAPSHOT_CONTROL_DIR"],
            "--checkpoint-dir", os.environ["SNAPSHOT_CONTROL_DIR"],
            "--process", str(os.getpid()),
        ], capture_output=True)
        assert result.returncode != 0
        command("inspect")
        return
    assert cuda.cuMemCreate(c.byref(member), length, c.byref(props), 0) == 0
    assert cuda.cuMemMap(0x10000000, length, 0, member, 0) == 0
    assert cuda.cuMulticastCreate(c.byref(group), c.byref(Multicast(1, length, 1, 0))) == 0

    if mode == "inflight":
        cuda.multicast_block_arm(1)
        results = []
        worker = threading.Thread(target=lambda: results.append(cuda.cuMulticastAddDevice(group, 0)))
        worker.start()
        cuda.multicast_block_wait()
        try:
            command("inspect", False)
            command("prepare_multicast", False)
            command("restore_multicast_creators", False)
            assert cuda.cuMemRelease(group) == 600
            # State remains available while the driver collective is blocked.
            other = u64()
            assert cuda.cuMemCreate(c.byref(other), 4096, c.byref(props), 0) == 0
            assert cuda.cuMemRelease(other) == 0
        finally:
            cuda.multicast_block_release()
            worker.join(10)
        assert not worker.is_alive() and results == [0]
    else:
        assert cuda.cuMulticastAddDevice(group, 0) == 0

    binding_offset = length if mode == "extent" else 0
    mapped_size = 2 * length if mode == "extent" else length
    assert cuda.cuMulticastBindMem_v2(group, 0, binding_offset, member, 0, length, 0) == 0
    assert cuda.cuMemMap(0x70000000, mapped_size, 0, group, 0) == 0
    if mode == "cached-export":
        multicast_reference = next(
            record["multicast"]["allocation"]
            for record in command("inspect")["records"]
            if "multicast" in record
        )
        virtual_shareable_handles = []
        for handle in (member, group):
            fd = c.c_int(-1)
            assert cuda.cuMemExportToShareableHandle(c.byref(fd), handle, 1, 0) == 0
            virtual_shareable_handles.append(fd.value)
        command("prepare_multicast")
        path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
        response, exported = request_export(path, multicast_reference)
        assert "Err" in response["result"] and exported is None
        assert cuda.fakeMulticastObjects() == 0
        assert cuda.fakeMulticastBindings(0) == 0
        assert cuda.fakeMappedCount() == 1, "member mapping remains until PREPARE_UNICAST"
        for fd in virtual_shareable_handles:
            os.close(fd)
        print("PASS multicast cached-export")
        return  # Deliberately leave the process mid-prepare, as the C case did.
    elif mode == "released":
        # No unicast export ever occurred. BindMem alone transfers ownership,
        # and restore must temporarily retain the mapped member to rebind it.
        assert cuda.cuMemRelease(member) == 0
        assert cuda.cuMemRelease(group) == 0
        replay()
        assert cuda.fakeCopiedToHost() == length
        assert cuda.fakeCopiedToDevice() == length
        assert cuda.fakeMulticastBindings(1) == 1
        assert cuda.cuMemRetainAllocationHandle(c.byref(group), c.c_void_p(0x70000000)) == 0
        assert cuda.cuMemRetainAllocationHandle(c.byref(member), c.c_void_p(0x10000000)) == 0
    elif mode == "kind":
        path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
        records = command("inspect")["records"]
        unicast_reference = next(
            record["allocation"]["allocation"]
            for record in records
            if "allocation" in record
        )
        multicast_reference = next(
            record["multicast"]["allocation"]
            for record in records
            if "multicast" in record
        )
        virtual_shareable_handles = []
        for handle, reference, expected in (
            (member, unicast_reference, "unicast_export"),
            (group, multicast_reference, "multicast_export"),
        ):
            fd = c.c_int(-1)
            assert cuda.cuMemExportToShareableHandle(c.byref(fd), handle, 1, 0) == 0
            virtual_shareable_handles.append(fd.value)
            response, exported = request_export(path, reference)
            assert exported is not None
            os.close(exported)
            reply = response["result"]["Ok"]
            assert expected in reply
            if expected == "multicast_export":
                assert reply[expected]["devices"] == 1
        alias = u64()
        assert cuda.cuMemImportFromShareableHandle(
            c.byref(alias), c.c_void_p(virtual_shareable_handles[1]), 1) == 0
        assert cuda.cuMemRelease(alias) == 0
        for fd in virtual_shareable_handles:
            os.close(fd)
        replay()
    elif mode == "access":
        class Access(c.Structure):
            _fields_ = [("kind", c.c_int), ("device", c.c_int), ("flags", c.c_uint)]
        cuda.cuMemSetAccess.argtypes = [u64, size, c.POINTER(Access), size]
        access = Access(1, 0, 3)
        assert cuda.cuMemSetAccess(0x70000000, length, c.byref(access), 1) == 0
        cuda.multicast_fail_access()
        assert cuda.cuMemSetAccess(0x70000000, length, c.byref(access), 1) != 0
        command("inspect")
        replay()
    elif mode == "failure":
        for operation in LIFECYCLE[:5]:
            command(operation)
        command("restore_multicast_importers", False)
        cuda.fakeFailNext.argtypes = [c.c_char_p]
        cuda.fakeFailNext(b"cuMulticastCreate")
        command("restore_multicast_creators", False)
        assert cuda.cuMemRelease(group) == 600
        print("PASS multicast failure")
        return
    elif mode == "tracked-address":
        assert cuda.cuMulticastUnbind(group, 0, 0, length) == 0
        assert cuda.cuMulticastBindAddr_v2(group, 0, 0, 0x10000000, length, 0) == 0
        replay()
        assert cuda.fakeMulticastBindings(2) == 1
        assert cuda.fakeCopiedToHost() == length
    elif mode == "native-address":
        # BindAddr's member may be native-owned and absent from the shim table.
        # Its VA is restored by native CUDA, while only the binding is replayed.
        native_properties = Properties.from_buffer_copy(props)
        native_properties.handles = 0
        native = u64()
        assert cuda.cuMemCreate(c.byref(native), length, c.byref(native_properties), 0) == 0
        assert cuda.cuMemMap(0x30000000, length, 0, native, 0) == 0
        assert cuda.cuMulticastUnbind(group, 0, 0, length) == 0
        assert cuda.cuMulticastBindMem_v2(group, 0, 0, native, 0, length, 0) == 801
        assert cuda.fakeMulticastBindings(0) == 0
        assert cuda.cuMulticastBindAddr_v2(group, 0, 0, 0x30000000, length, 0) == 0
        replay()
        assert cuda.fakeMulticastBindings(2) == 1
        assert cuda.cuMemUnmap(0x30000000, length) == 0
        assert cuda.cuMemRelease(native) == 0
    elif mode in ("inflight", "create-output", "extent"):
        replay()
    else:
        raise AssertionError(mode)

    assert cuda.cuMemUnmap(0x70000000, mapped_size) == 0
    assert cuda.cuMulticastUnbind(group, 0, binding_offset, length) == 0
    assert cuda.cuMemRelease(group) == 0
    assert cuda.cuMemUnmap(0x10000000, length) == 0
    assert cuda.cuMemRelease(member) == 0
    assert command("inspect")["records"] == []
    assert cuda.fakeLiveAllocations() == 0
    assert cuda.fakeMappedCount() == 0 and cuda.fakeMulticastObjects() == 0
    print(f"PASS multicast {mode}")


if __name__ == "__main__":
    main()
