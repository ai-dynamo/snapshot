#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Prestarted control dispatch: reciprocal imports, pressure, and refusal."""

import array
import ctypes as c
import json
import os
from pathlib import Path
import select
import socket
import subprocess
import sys
import time

from support import driver, props

cuda = driver()
from protocol_client import LIFECYCLE, request, reply


class Multicast(c.Structure):
    _fields_ = [("devices", c.c_uint), ("size", c.c_size_t),
                ("handles", c.c_uint64), ("flags", c.c_uint64)]


def worker(kind, transport_fd, ready_fd, release_fd):
    transport = socket.socket(fileno=transport_fd)
    length = 1 << 20
    handle, imported = c.c_uint64(), c.c_uint64()
    virtual_shareable_handle = c.c_int(-1)
    if kind == "unicast":
        assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
    else:
        cuda.cuMulticastCreate.argtypes = [c.POINTER(c.c_uint64), c.POINTER(Multicast)]
        assert cuda.cuMulticastCreate(c.byref(handle), c.byref(Multicast(2, length, 1, 0))) == 0
    assert (
        cuda.cuMemExportToShareableHandle(
            c.byref(virtual_shareable_handle), handle, 1, 0
        )
        == 0
    )
    transport.sendmsg([b"T"], [(socket.SOL_SOCKET, socket.SCM_RIGHTS,
                              array.array("i", [virtual_shareable_handle.value]))])
    _, ancillary, _, _ = transport.recvmsg(1, socket.CMSG_SPACE(4))
    descriptors = array.array("i")
    descriptors.frombytes(ancillary[0][2])
    assert len(descriptors) == 1
    cuda.cuMemImportFromShareableHandle.argtypes = [c.POINTER(c.c_uint64), c.c_void_p, c.c_uint]
    assert cuda.cuMemImportFromShareableHandle(c.byref(imported), c.c_void_p(descriptors[0]), 1) == 0
    os.close(descriptors[0])
    u64, size = c.c_uint64, c.c_size_t
    cuda.cuMemMap.argtypes = [u64, size, size, u64, u64]
    cuda.cuMemUnmap.argtypes = [u64, size]
    if kind == "multicast":
        cuda.cuMulticastAddDevice.argtypes = [u64, c.c_int]
        cuda.cuMulticastBindMem_v2.argtypes = [u64, c.c_int, size, u64, size, size, u64]
        cuda.cuMulticastUnbind.argtypes = [u64, c.c_int, size, size]
        member = u64()
        assert cuda.cuMemCreate(c.byref(member), length, c.byref(props), 0) == 0
        for group in (handle, imported):
            assert cuda.cuMulticastAddDevice(group, 0) == 0
            assert cuda.cuMulticastBindMem_v2(group, 0, 0, member, 0, length, 0) == 0
    for address, allocation in ((0x10000000, handle), (0x20000000, imported)):
        assert cuda.cuMemMap(address, length, 0, allocation, 0) == 0
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    namespace_pid = os.getpid()
    assert cuda.rpc_attempts() == 2, "only the two mandatory threads should exist"
    # Confirm pressure is real, then leave it armed for the entire lifecycle.
    cuda.rpc_fail_workers(1000)
    try:
        import threading
        threading.Thread(target=lambda: None).start()
    except RuntimeError:
        pass
    else:
        raise AssertionError("pthread_create pressure was not installed")
    baseline = cuda.rpc_attempts()
    print(json.dumps([path, namespace_pid]), flush=True)
    for command in sys.stdin:
        if command.strip() == "barrier":
            cuda.rpc_peer_barrier(ready_fd, release_fd)
            print("armed", flush=True)
        elif command.strip() == "done":
            assert cuda.rpc_attempts() == baseline and cuda.rpc_refused() == 1
            reply(request(path, "inspect", namespace_pid))
            cuda.fakeCopiedToHost.restype = c.c_uint64
            cuda.fakeCopiedToDevice.restype = c.c_uint64
            expected = length
            assert cuda.fakeCopiedToHost() == expected
            assert cuda.fakeCopiedToDevice() == expected
            assert cuda.fakeMappedCount() == 2
            if kind == "multicast":
                assert cuda.fakeMulticastBindings(1) == 2
                for group in (handle, imported):
                    assert cuda.cuMulticastUnbind(group, 0, 0, length) == 0
                assert cuda.cuMemRelease(member) == 0
            assert cuda.cuMemUnmap(0x10000000, length) == 0
            assert cuda.cuMemUnmap(0x20000000, length) == 0
            assert cuda.cuMemRelease(imported) == 0
            assert cuda.cuMemRelease(handle) == 0
            os.close(virtual_shareable_handle.value)
            print("done", flush=True)
            return


def reciprocal(kind):
    workers, endpoints, pipes = [], [], []
    left, right = socket.socketpair()
    try:
        for transport in (left, right):
            ready_read, ready_write = os.pipe()
            release_read, release_write = os.pipe()
            process = subprocess.Popen(
                [sys.executable, __file__, "worker", kind, str(transport.fileno()),
                 str(ready_write), str(release_read)],
                pass_fds=(transport.fileno(), ready_write, release_read),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
            workers.append(process)
            os.close(ready_write)
            os.close(release_read)
            pipes.append((ready_read, release_write))
        for process in workers:
            endpoints.append(json.loads(process.stdout.readline()))
        for operation in LIFECYCLE:
            barrier = operation == (
                "restore_unicast" if kind == "unicast" else "restore_multicast_importers")
            if barrier:
                for process in workers:
                    process.stdin.write("barrier\n")
                    process.stdin.flush()
                    assert process.stdout.readline().strip() == "armed"
            connections = [request(path, operation, namespace_pid)
                           for path, namespace_pid in endpoints]
            if barrier:
                # No phase replay: both actual lifecycle workers must have
                # reached their outgoing peer request before either proceeds.
                for ready, _ in pipes:
                    assert select.select([ready], [], [], 5)[0], "import worker did not reach peer request"
                    assert os.read(ready, 1) == b"R"
                # Queue INSPECT behind both blocked lifecycle workers. Neither
                # queued state access may obstruct the independent listeners.
                inspections = [request(path, "inspect", namespace_pid)
                               for path, namespace_pid in endpoints]
                for _, release in pipes:
                    os.write(release, b"G")
            for connection in connections:
                reply(connection)
            if barrier:
                for connection in inspections:
                    refused = reply(connection, success=False)
                    assert "cannot inspect current CUDA state" in refused["result"]["Err"]
        for process in workers:
            process.stdin.write("done\n")
            process.stdin.flush()
            assert process.stdout.readline().strip() == "done"
            assert process.wait(5) == 0
    finally:
        left.close()
        right.close()
        for process in workers:
            if process.poll() is None:
                process.kill()
            process.wait()
        for ready, release in pipes:
            os.close(ready)
            os.close(release)
    print(f"PASS RPC reciprocal {kind}: peer and queued INSPECT progress with pthread_create unavailable")


def constructor(nth, library):
    before_fds = set(os.listdir("/proc/self/fd"))
    before_tasks = set(os.listdir("/proc/self/task"))
    started = time.monotonic()
    fixture = c.CDLL(library)
    assert time.monotonic() - started < 5, "constructor did not return promptly"
    result = c.c_int.in_dll(fixture, "rpc_constructor_result").value
    handle = c.c_uint64.in_dll(fixture, "rpc_constructor_handle")
    path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
    assert cuda.rpc_attempts() == (nth or 2)
    if nth:
        assert result != 0 and handle.value == 0xAAAA
        assert cuda.rpc_refused() == 1 and not path.exists()
        # Loader lock has now been released. No join was possible inside the
        # constructor; poll for actual kernel thread disappearance afterward.
        deadline = time.monotonic() + 5
        while set(os.listdir("/proc/self/task")) != before_tasks and time.monotonic() < deadline:
            time.sleep(0.001)
        assert set(os.listdir("/proc/self/task")) == before_tasks, "orphan startup worker"
        assert set(os.listdir("/proc/self/fd")) == before_fds, "startup leaked an FD"
        cuda.rpc_fail_startup(0)
        assert cuda.cuMemCreate(c.byref(handle), 4096, c.byref(props), 0) != 0
        assert handle.value == 0xAAAA
        assert cuda.rpc_attempts() == nth and not path.exists(), "failed generation restarted"
    else:
        assert result == 0
        assert path.exists() and cuda.rpc_refused() == 0
        assert len(os.listdir("/proc/self/task")) == len(before_tasks) + 2
        inspection = reply(request(str(path), "inspect", os.getpid()))
        records = inspection["result"]["Ok"]["inspection"]["records"]
        assert sum("allocation" in entry for entry in records) == 1
        assert cuda.cuMemRelease(handle) == 0
    print(f"PASS RPC constructor {nth}: prompt return, "
          + ("sticky failure, FD/path cleanup, eventual worker exit" if nth else "live endpoint"))


def queue_full():
    handle, virtual_shareable_handle = c.c_uint64(), c.c_int(-1)
    assert cuda.cuMemCreate(c.byref(handle), 1 << 20, c.byref(props), 0) == 0
    assert (
        cuda.cuMemExportToShareableHandle(
            c.byref(virtual_shareable_handle), handle, 1, 0
        )
        == 0
    )
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    namespace_pid = os.getpid()
    reply(request(path, "prepare_multicast", namespace_pid))
    cuda.rpc_block_copy()
    saving = request(path, "save_allocations", namespace_pid)
    deadline = time.monotonic() + 5
    while not cuda.rpc_copy_entered() and time.monotonic() < deadline:
        time.sleep(0.001)
    assert cuda.rpc_copy_entered()
    queued = [request(path, "inspect", namespace_pid) for _ in range(8)]
    # A lifecycle request that would mutate state after SAVE completes is
    # rejected before it enters execution. It must not be replayed later.
    refused = reply(request(path, "prepare_unicast", namespace_pid), success=False)
    assert "control queue full; refused without mutation" in refused["result"]["Err"]
    cuda.rpc_release_copy()
    reply(saving)
    for connection in queued:
        refused = reply(connection, success=False)
        assert "cannot inspect current CUDA state" in refused["result"]["Err"]
    refused = reply(request(path, "inspect", namespace_pid), success=False)
    assert "cannot inspect current CUDA state" in refused["result"]["Err"]
    assert cuda.fakeLiveAllocations() == 1
    # Explicit subsequent request is a new caller action, not an internal retry.
    reply(request(path, "prepare_unicast", namespace_pid))
    assert cuda.fakeLiveAllocations() == 0
    os.close(virtual_shareable_handle.value)
    print("PASS RPC bounded queue: eight waiting requests, rejected mutation never executed")


if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "worker":
        worker(sys.argv[2], *map(int, sys.argv[3:]))
    elif mode.startswith("constructor"):
        constructor(int(mode[-1]), sys.argv[2])
    elif mode == "queue":
        queue_full()
    else:
        reciprocal(mode)
