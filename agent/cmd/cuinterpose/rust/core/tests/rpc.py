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
import struct
import subprocess
import sys
import time

from fork import cuda, props, Stats, stats


class Multicast(c.Structure):
    _fields_ = [("devices", c.c_uint), ("size", c.c_size_t),
                ("handles", c.c_uint64), ("flags", c.c_uint64)]


def request(path, operation, identity):
    connection = socket.socket(socket.AF_UNIX)
    connection.settimeout(10)
    connection.connect(path)
    header = bytearray(256)
    struct.pack_into("<IHH", header, 0, 0x44564D4D, 2, operation)
    header[24:57] = identity
    connection.sendall(header)
    return connection


def reply(connection, success=True):
    with connection:
        data = bytearray()
        while len(data) < 256:
            chunk = connection.recv(256 - len(data))
            assert chunk, "missing control response"
            data.extend(chunk)
        status = struct.unpack_from("<i", data, 8)[0]
        assert (status == 0) == success, data[57:153]
        remaining = struct.unpack_from("<I", data, 12)[0] * 688
        while remaining:
            chunk = connection.recv(remaining)
            assert chunk
            remaining -= len(chunk)
        assert connection.recv(1) == b"", "more than one response"
        return data


def worker(kind, transport_fd, ready_fd, release_fd):
    transport = socket.socket(fileno=transport_fd)
    length = 1 << 20
    handle, imported, ticket = c.c_uint64(), c.c_uint64(), c.c_int(-1)
    if kind == "unicast":
        assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
    else:
        cuda.cuMulticastCreate.argtypes = [c.POINTER(c.c_uint64), c.POINTER(Multicast)]
        assert cuda.cuMulticastCreate(c.byref(handle), c.byref(Multicast(2, length, 1, 0))) == 0
    assert cuda.cuMemExportToShareableHandle(c.byref(ticket), handle, 1, 0) == 0
    transport.sendmsg([b"T"], [(socket.SOL_SOCKET, socket.SCM_RIGHTS,
                              array.array("i", [ticket.value]))])
    _, ancillary, _, _ = transport.recvmsg(1, socket.CMSG_SPACE(4))
    descriptors = array.array("i")
    descriptors.frombytes(ancillary[0][2])
    assert len(descriptors) == 1
    cuda.cuMemImportFromShareableHandle.argtypes = [c.POINTER(c.c_uint64), c.c_void_p, c.c_uint]
    assert cuda.cuMemImportFromShareableHandle(c.byref(imported), c.c_void_p(descriptors[0]), 1) == 0
    os.close(descriptors[0])
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    identity = reply(request(path, 1, bytes(33)))[24:57]
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
    print(json.dumps([path, identity.hex()]), flush=True)
    for command in sys.stdin:
        if command.strip() == "barrier":
            cuda.rpc_peer_barrier(ready_fd, release_fd)
            print("armed", flush=True)
        elif command.strip() == "done":
            assert cuda.rpc_attempts() == baseline and cuda.rpc_refused() == 1
            assert stats().phase == 1
            cuda.fakeCopiedToHost.restype = c.c_uint64
            cuda.fakeCopiedToDevice.restype = c.c_uint64
            expected = length if kind == "unicast" else 0
            assert cuda.fakeCopiedToHost() == expected
            assert cuda.fakeCopiedToDevice() == expected
            assert cuda.cuMemRelease(imported) == 0
            assert cuda.cuMemRelease(handle) == 0
            os.close(ticket.value)
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
            path, identity = json.loads(process.stdout.readline())
            endpoints.append((path, bytes.fromhex(identity)))
        for operation in (3, 4, 5, 7, 8, 9, 10, 11, 12):
            barrier = operation == (8 if kind == "unicast" else 10)
            if barrier:
                for process in workers:
                    process.stdin.write("barrier\n")
                    process.stdin.flush()
                    assert process.stdout.readline().strip() == "armed"
            connections = [request(path, operation, identity) for path, identity in endpoints]
            if barrier:
                # No phase replay: both actual lifecycle workers must have
                # reached their outgoing peer request before either proceeds.
                for ready, _ in pipes:
                    assert select.select([ready], [], [], 5)[0], "import worker did not reach peer request"
                    assert os.read(ready, 1) == b"R"
                # Queue INSPECT behind both blocked lifecycle workers. Neither
                # queued state access may obstruct the independent listeners.
                inspections = [request(path, 2, identity) for path, identity in endpoints]
                for _, release in pipes:
                    os.write(release, b"G")
            for connection in connections:
                reply(connection)
            if barrier:
                for connection in inspections:
                    refused = reply(connection, success=False)
                    assert b"cannot inspect current CUDA state" in refused[57:153]
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


def startup(nth):
    before = len(os.listdir("/proc/self/task"))
    cuda.rpc_fail_startup(nth)
    handle = c.c_uint64(0xAAAA)
    assert cuda.cuMemCreate(c.byref(handle), 4096, c.byref(props), 0) != 0
    assert handle.value == 0xAAAA
    assert cuda.rpc_refused() == 1 and cuda.rpc_attempts() == nth
    path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
    assert not path.exists(), "failed startup left a socket"
    # No restart after removing the cause: the unpublished generation failed.
    cuda.rpc_fail_startup(0)
    value = Stats()
    value.phase = 99
    cuda.cuinterpose_debug_stats(c.byref(value))
    assert value.phase == 99 and cuda.rpc_attempts() == nth
    assert cuda.cuMemCreate(c.byref(handle), 4096, c.byref(props), 0) != 0
    assert handle.value == 0xAAAA and not path.exists()
    deadline = time.monotonic() + 5
    while len(os.listdir("/proc/self/task")) != before and time.monotonic() < deadline:
        time.sleep(0.001)
    assert len(os.listdir("/proc/self/task")) == before, "orphan startup worker"
    print(f"PASS RPC mandatory startup failure {nth}: sticky, no published state, no orphan")


def constructor_child(nth, library):
    before_fds = set(os.listdir("/proc/self/fd"))
    before_tasks = set(os.listdir("/proc/self/task"))
    started = time.monotonic()
    fixture = c.CDLL(library)
    assert time.monotonic() - started < 5, "constructor did not return promptly"
    result = c.c_int.in_dll(fixture, "rpc_constructor_result").value
    handle = c.c_uint64.in_dll(fixture, "rpc_constructor_handle")
    value = Stats.in_dll(fixture, "rpc_constructor_stats")
    path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
    assert cuda.rpc_attempts() == (nth or 2)
    if nth:
        assert result != 0 and handle.value == 0xAAAA
        assert bytes(value) == b"\xA5" * c.sizeof(Stats), "failure changed stats output"
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
        cuda.cuinterpose_debug_stats(c.byref(value))
        assert handle.value == 0xAAAA and bytes(value) == b"\xA5" * c.sizeof(Stats)
        assert cuda.rpc_attempts() == nth and not path.exists(), "failed generation restarted"
    else:
        assert result == 0 and value.allocations == 1 and value.phase == 1
        assert path.exists() and cuda.rpc_refused() == 0
        assert len(os.listdir("/proc/self/task")) == len(before_tasks) + 2
        reply(request(str(path), 1, bytes(33)))
        assert cuda.cuMemRelease(handle) == 0
    print(f"PASS RPC constructor child {nth}: prompt return, "
          + ("sticky failure, FD/path cleanup, eventual worker exit" if nth else "live endpoint"))


def constructor(nth, library):
    handle = c.c_uint64()
    assert cuda.cuMemCreate(c.byref(handle), 4096, c.byref(props), 0) == 0
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    identity = reply(request(path, 1, bytes(33)))[24:57]
    subprocess.run([sys.executable, __file__, "constructor-child", str(nth), library],
                   env=os.environ | {"CUINTERPOSE_TEST_STARTUP_FAILURE": str(nth)},
                   check=True, timeout=15)
    assert stats().allocations == 1 and stats().phase == 1
    assert reply(request(path, 1, bytes(33)))[24:57] == identity
    assert cuda.rpc_attempts() == 2 and cuda.rpc_refused() == 0
    assert cuda.cuMemRelease(handle) == 0
    print(f"PASS RPC constructor {nth}: parent generation unaffected")


def queue_full():
    handle, ticket = c.c_uint64(), c.c_int(-1)
    assert cuda.cuMemCreate(c.byref(handle), 1 << 20, c.byref(props), 0) == 0
    assert cuda.cuMemExportToShareableHandle(c.byref(ticket), handle, 1, 0) == 0
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    identity = reply(request(path, 1, bytes(33)))[24:57]
    reply(request(path, 3, identity))
    cuda.rpc_block_copy()
    saving = request(path, 4, identity)
    deadline = time.monotonic() + 5
    while not cuda.rpc_copy_entered() and time.monotonic() < deadline:
        time.sleep(0.001)
    assert cuda.rpc_copy_entered()
    queued = [request(path, 2, identity) for _ in range(8)]
    # A lifecycle request that would mutate state after SAVE completes is
    # rejected before it enters execution. It must not be replayed later.
    refused = reply(request(path, 5, identity), success=False)
    assert b"control queue full; refused without mutation" in refused[57:153]
    cuda.rpc_release_copy()
    reply(saving)
    for connection in queued:
        refused = reply(connection, success=False)
        assert b"cannot inspect current CUDA state" in refused[57:153]
    assert stats().phase == 2  # Preparing (saved), not fully prepared.
    assert cuda.fakeLiveAllocations() == 1
    # Explicit subsequent request is a new caller action, not an internal retry.
    reply(request(path, 5, identity))
    assert cuda.fakeLiveAllocations() == 0
    os.close(ticket.value)
    print("PASS RPC bounded queue: eight waiting requests, rejected mutation never executed")


if __name__ == "__main__":
    mode = sys.argv[1]
    if mode == "worker":
        worker(sys.argv[2], *map(int, sys.argv[3:]))
    elif mode == "constructor-child":
        constructor_child(int(sys.argv[2]), sys.argv[3])
    elif mode.startswith("constructor"):
        constructor(int(mode[-1]), sys.argv[2])
    elif mode.startswith("startup"):
        startup(int(mode[-1]))
    elif mode == "queue":
        queue_full()
    else:
        reciprocal(mode)
