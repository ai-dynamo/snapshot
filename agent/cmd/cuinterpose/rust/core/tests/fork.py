#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Process-isolated fake-driver fork regressions; no real CUDA fork claim."""

import ctypes as c
import errno
import os
from pathlib import Path
import socket
import struct
import sys
import time
from protocol_client import command, inspect


class Location(c.Structure):
    _fields_ = [("kind", c.c_int), ("device", c.c_int)]


class Properties(c.Structure):
    _fields_ = [
        ("kind", c.c_int), ("handles", c.c_uint),
        ("location", Location), ("win32", c.c_void_p), ("flags", c.c_byte * 8),
    ]


class Stats(c.Structure):
    _fields_ = [(name, c.c_uint64) for name in (
        "allocations", "handles", "mappings", "multicasts", "exports", "raw", "unsupported",
    )] + [("phase", c.c_uint)]


cuda = c.CDLL(None)
cuda.cuMemCreate.argtypes = [c.POINTER(c.c_uint64), c.c_size_t, c.POINTER(Properties), c.c_uint64]
cuda.cuMemRelease.argtypes = [c.c_uint64]
cuda.cuMemExportToShareableHandle.argtypes = [c.c_void_p, c.c_uint64, c.c_uint, c.c_uint64]
cuda.cuinterpose_debug_stats.argtypes = [c.POINTER(Stats)]
cuda.fakeEnableTrackedBehavior()
props = Properties(1, 1, Location(1, 0), None)


def stats():
    value = Stats()
    cuda.cuinterpose_debug_stats(c.byref(value))
    return value


def wait(child):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        pid, status = os.waitpid(child, os.WNOHANG)
        if pid:
            assert os.waitstatus_to_exitcode(status) == 0, status
            return
        time.sleep(0.01)
    os.kill(child, 9)
    os.waitpid(child, 0)
    raise AssertionError("fork child did not complete")

def child_checks(parent_id, inherited=(), ticket=None, application_socket=None):
    try:
        # Check before any child shim work can reuse these descriptor numbers.
        for fd in inherited:
            try:
                os.fstat(fd)
            except OSError as error:
                assert error.errno == errno.EBADF
            else:
                raise AssertionError(f"inherited shim fd {fd} remains open")
        if ticket is not None:
            assert os.pread(ticket, 4, 0) == struct.pack("<I", 0x44564D43)
        if application_socket is not None:
            os.fstat(application_socket)
        assert stats().allocations == 0
        identity = inspect()["participant"]
        assert identity != parent_id
        assert identity.hex() != os.environ["CUINTERPOSE_PARTICIPANT_ID"]
        value = c.c_uint64()
        assert cuda.cuMemCreate(c.byref(value), 4096, c.byref(props), 0) == 0
        assert cuda.cuMemRelease(value) == 0
        os._exit(0)
    except BaseException:
        import traceback
        traceback.print_exc()
        os._exit(1)


def main():
    mode = sys.argv[1]
    if mode == "preinit":
        child = os.fork()
        if child == 0:
            child_checks(b"")
        wait(child)
        assert stats().allocations == 0
        assert inspect()["participant"].hex() == os.environ["CUINTERPOSE_PARTICIPANT_ID"]
        return

    value = c.c_uint64()
    assert cuda.cuMemCreate(c.byref(value), 4096, c.byref(props), 0) == 0
    ticket = c.c_int(-1)
    assert cuda.cuMemExportToShareableHandle(c.byref(ticket), value, 1, 0) == 0
    parent_id = inspect()["participant"]
    assert parent_id.hex() == os.environ["CUINTERPOSE_PARTICIPANT_ID"]

    if mode == "descriptors":
        # An idle accepted socket belongs to the shim's FD inventory, while the
        # application's end survives. No CUDA or lifecycle call is in flight.
        before = set(os.listdir("/proc/self/fd"))
        idle = socket.socket(socket.AF_UNIX)
        idle.connect(f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock")
        deadline = time.monotonic() + 5
        while len(set(os.listdir("/proc/self/fd")) - before) < 2:
            assert time.monotonic() < deadline
            time.sleep(0.001)
        inherited = []
        for path in Path("/proc/self/fd").iterdir():
            try:
                name = os.readlink(path)
            except FileNotFoundError:
                continue
            fd = int(path.name)
            if fd != idle.fileno() and ("fake-cuda" in name or name.startswith("socket:")):
                inherited.append(fd)
        assert len(inherited) >= 2, inherited
        child = os.fork()
        if child == 0:
            child_checks(parent_id, inherited, ticket.value, idle.fileno())
        wait(child)
        assert stats().allocations == 1 and stats().exports == 1
        assert inspect()["participant"] == parent_id
        idle.close()
    elif mode == "poison":
        # Protocol/order rejection must not poison the workload. A real copy
        # failure does, and only that generation's poison is reset by fork.
        command("prepare_unicast", False)
        assert stats().phase == 1
        command("prepare_multicast")
        cuda.fakeFailNext.argtypes = [c.c_char_p]
        cuda.fakeFailNext(b"cuMemcpyDtoHAsync_v2")
        command("save_allocations", False)
        assert stats().phase == 5
        child = os.fork()
        if child == 0:
            child_checks(parent_id, ticket=ticket.value)
        wait(child)
        assert stats().phase == 5
        os.close(ticket.value)
        return
    elif mode == "nested":
        # Find the parent's listening socket. No child CUDA/debug/control call
        # is allowed before the second fork: it would mask stale-registry bugs.
        sockets = []
        for path in Path("/proc/self/fd").iterdir():
            try:
                if os.readlink(path).startswith("socket:"):
                    sockets.append(int(path.name))
            except FileNotFoundError:
                continue
        assert sockets
        listener = min(sockets)
        child = os.fork()
        if child == 0:
            try:
                replacement = os.open("/dev/null", os.O_RDONLY)
                if replacement != listener:
                    os.dup2(replacement, listener)
                    os.close(replacement)
                grandchild = os.fork()
                if grandchild == 0:
                    try:
                        assert os.read(listener, 1) == b""
                        assert os.pread(ticket.value, 4, 0) == struct.pack("<I", 0x44564D43)
                        os._exit(0)
                    except BaseException:
                        os._exit(77)
                wait(grandchild)
                assert os.read(listener, 1) == b""
                os._exit(0)
            except BaseException:
                import traceback
                traceback.print_exc()
                os._exit(1)
        wait(child)
        assert inspect()["participant"] == parent_id
    elif mode == "carrier":
        def mappings():
            return [tuple(int(part, 16) for part in line.split()[0].split("-"))
                    for line in Path("/proc/self/maps").read_text().splitlines()]
        before = mappings()
        command("prepare_multicast")
        command("save_allocations")
        assert cuda.fakeRegisteredHostRanges() == 1
        cuda.cuMemHostGetFlags.argtypes = [c.POINTER(c.c_uint), c.c_void_p]
        base = None
        # The new anonymous carrier may coalesce with an adjacent VMA. Subtract
        # the pre-save intervals rather than assuming a new /proc/maps entry.
        for start, end in mappings():
            gaps = [(start, end)]
            for old_start, old_end in before:
                remaining = []
                for low, high in gaps:
                    if old_end <= low or old_start >= high:
                        remaining.append((low, high))
                    else:
                        if low < old_start:
                            remaining.append((low, old_start))
                        if old_end < high:
                            remaining.append((old_end, high))
                gaps = remaining
            for low, high in gaps:
                for address in range(low, high, 4096):
                    flags = c.c_uint()
                    if cuda.cuMemHostGetFlags(c.byref(flags), address) == 0:
                        base = address
        assert base is not None, "saved arena was not mapped and registered"
        libc = c.CDLL("libc.so.6", use_errno=True)
        libc.mincore.argtypes = [c.c_void_p, c.c_size_t, c.c_void_p]
        resident = c.c_ubyte()
        assert libc.mincore(base, 4096, c.byref(resident)) == 0
        allocations = cuda.fakeLiveAllocations()
        child = os.fork()
        if child == 0:
            try:
                assert libc.mincore(base, 4096, c.byref(resident)) == -1
                assert c.get_errno() == errno.ENOMEM
                # Fake model is inherited deliberately: changing these counts
                # would expose forbidden CUDA unregister/release in the hook.
                assert cuda.fakeRegisteredHostRanges() == 1
                assert cuda.fakeLiveAllocations() == allocations
                child_checks(parent_id, ticket=ticket.value)
            except BaseException:
                import traceback
                traceback.print_exc()
                os._exit(1)
        wait(child)
        assert libc.mincore(base, 4096, c.byref(resident)) == 0
        assert cuda.fakeRegisteredHostRanges() == 1
        assert cuda.fakeLiveAllocations() == allocations
        os.close(ticket.value)
        return  # Parent intentionally remains prepared until process exit.
    elif mode == "startup-failure":
        child = os.fork()
        if child == 0:
            try:
                endpoint = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
                # Bind cannot succeed over this application-owned file.
                endpoint.write_text("not a socket")
                failed = Stats()
                failed.phase = 99
                cuda.cuinterpose_debug_stats(c.byref(failed))
                assert failed.phase == 99
                endpoint.unlink()
                # Removing the cause must not turn a real setup failure into a
                # retry, or expose the unpublished generation as ready.
                cuda.cuinterpose_debug_stats(c.byref(failed))
                assert failed.phase == 99
                assert not endpoint.exists()
                temporary = c.c_uint64()
                assert cuda.cuMemCreate(c.byref(temporary), 4096, c.byref(props), 0) == 600
                os._exit(0)
            except BaseException:
                import traceback
                traceback.print_exc()
                os._exit(1)
        wait(child)
        assert stats().allocations == 1 and inspect()["participant"] == parent_id
    else:
        raise AssertionError(mode)
    os.close(ticket.value)
    assert cuda.cuMemRelease(value) == 0


if __name__ == "__main__":
    main()
    print(f"PASS fork {sys.argv[1]}", flush=True)
