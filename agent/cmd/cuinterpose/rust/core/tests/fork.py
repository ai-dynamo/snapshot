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
from support import driver, props

cuda = driver()


def initialize():
    handle = c.c_uint64(42)
    assert cuda.cuMemCreate(c.byref(handle), 4096, None, 0) == 1
    assert handle.value == 42


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
        initialize()
        assert command("inspect")["records"] == []
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
        initialize()
        assert command("inspect")["records"] == []
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
        records = command("inspect")["records"]
        assert sum("allocation" in record for record in records) == 1
        assert inspect()["participant"] == parent_id
        idle.close()
    elif mode == "poison":
        # Protocol/order rejection must not poison the workload. A real copy
        # failure does, and only that generation's poison is reset by fork.
        command("prepare_unicast", False)
        command("inspect")
        command("prepare_multicast")
        cuda.fakeFailNext.argtypes = [c.c_char_p]
        cuda.fakeFailNext(b"cuMemcpyDtoHAsync_v2")
        command("save_allocations", False)
        assert cuda.cuMemRelease(value) == 600
        child = os.fork()
        if child == 0:
            child_checks(parent_id, ticket=ticket.value)
        wait(child)
        assert cuda.cuMemRelease(value) == 600
        os.close(ticket.value)
        return
    elif mode == "nested":
        # Find the parent's listening socket. No child CUDA/control call
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
    else:
        raise AssertionError(mode)
    os.close(ticket.value)
    assert cuda.cuMemRelease(value) == 0


if __name__ == "__main__":
    main()
    print(f"PASS fork {sys.argv[1]}", flush=True)
