#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Actual Rust core endpoints without VMM activity; provider has no GPU state."""

import ctypes as c
import os
from pathlib import Path
import sys
import threading
import signal
import socket
import struct
import time

import msgpack


def inspect():
    body = msgpack.packb({"version": 3, "body": {
        "kind": "inspect", "namespace_pid": os.getpid(),
    }}, use_bin_type=True)
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(path.with_name(f"cuinterpose-{os.getpid()}.sock")))
        connection.sendall(struct.pack("<I", len(body)) + body)
        with connection.makefile("rb") as stream:
            length, = struct.unpack("<I", stream.read(4))
            response = msgpack.unpackb(stream.read(length), raw=False)
    assert response["version"] == 3 and "Ok" in response["body"]["result"]
    return response["body"]


driver = c.CDLL("libcuda.so.1", mode=os.RTLD_LOCAL)
cuda = c.CDLL(None)
runtime = c.CDLL("libcudart.so.13", mode=os.RTLD_LOCAL)
mode = sys.argv[1]
path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
assert not path.exists()
sockets_before = set(path.parent.glob("cuinterpose-*.sock"))

if mode in ("fork-before-init", "fork-after-init", "exec"):
    if mode != "fork-before-init":
        assert cuda.cuInit(0) == 0
    child = os.fork()
    if child == 0:
        signal.alarm(10)
        if mode == "fork-before-init":
            assert cuda.cuInit(0) == 0
            assert inspect()["namespace_pid"] == os.getpid()
        else:
            assert cuda.cuInit(0) == 3
            assert cuda.cuMemRelease(c.c_uint64(42)) == 3
            assert not path.with_name(f"cuinterpose-{os.getpid()}.sock").exists()
            if mode == "exec":
                os.execv(sys.executable, [sys.executable, __file__, "init"])
        os._exit(0)
    assert os.waitstatus_to_exitcode(os.waitpid(child, 0)[1]) == 0
    assert cuda.cuInit(0) == 0
    assert inspect()["namespace_pid"] == os.getpid()
    sys.exit(0)

if mode == "constructor":
    def activate():
        plugin = c.CDLL(sys.argv[2])
        plugin.fixture_join_generation_worker()

elif mode in ("init", "init-handle", "init-failure", "concurrent"):
    initialize = driver.cuInit if mode == "init-handle" else cuda.cuInit
    initialize.argtypes = [c.c_uint]

    def activate():
        if mode == "concurrent":
            barrier = threading.Barrier(16)
            results = [None] * 16

            def call(index):
                barrier.wait()
                results[index] = initialize(0)

            workers = [threading.Thread(target=call, args=(index,)) for index in range(16)]
            for worker in workers:
                worker.start()
            for worker in workers:
                worker.join()
            assert results == [0] * 16, results
        if mode == "init-failure":
            assert initialize(1) == 1
            assert not (Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock").exists()
        assert initialize(0) == 0

elif mode == "private":
    def activate():
        assert driver.fixture_private_runtime() == 0

else:
    names = ["cuGetProcAddress", "cuGetProcAddress_v2", "cuGetProcAddress_v2_ptsz",
             "cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
             "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"]
    index = 1 if mode == "tracked-query" else 3 if mode == "resolver-startup-failure" else int(mode)
    query = getattr(cuda if index < 3 else runtime, names[index])
    arguments = [c.c_char_p, c.POINTER(c.c_void_p)]
    if index < 3:
        arguments += [c.c_int, c.c_uint64]
    elif index < 5:
        arguments += [c.c_uint64]
    else:
        arguments += [c.c_uint, c.c_uint64]
    if index:
        arguments += [c.POINTER(c.c_int)]
    query.argtypes = arguments

    def activate():
        output = c.c_void_p()
        status = c.c_int(-1)
        tracked = mode == "tracked-query" or os.environ.get("CUINTERPOSE_TEST_NESTED_RUNTIME")
        args = [b"cuMemCreate" if tracked else b"cuFixtureUnwrapped", c.byref(output)]
        args += [13010, 0] if index < 3 or index >= 5 else [0]
        if index:
            args += [c.byref(status)]
        # Even a blocked endpoint must not turn a successful lookup into failure.
        if mode == "resolver-startup-failure":
            path.touch(exist_ok=False)
        before_tasks = set(os.listdir("/proc/self/task"))
        before_fds = set(os.listdir("/proc/self/fd"))
        assert query(*args) == 0 and output.value
        assert set(os.listdir("/proc/self/task")) == before_tasks
        assert set(os.listdir("/proc/self/fd")) == before_fds
        if mode == "resolver-startup-failure":
            assert cuda.cuInit(0) == 3
            path.unlink()
            assert query(*args) == 0 and output.value
            assert cuda.cuInit(0) == 3 and not path.exists()
        else:
            assert not path.exists()
            assert cuda.cuInit(0) == 0

activate()
if mode == "resolver-startup-failure":
    print("PASS lookup independent of runtime startup failure")
    sys.exit(0)
parent = inspect()["namespace_pid"]
if mode == "concurrent":
    inode = path.stat().st_ino
    for _ in range(16):
        assert initialize(0) == 0
        assert inspect()["namespace_pid"] == parent
    assert path.stat().st_ino == inode
    assert set(path.parent.glob("cuinterpose-*.sock")) == sockets_before | {path}
    # Losing initialization workers retire after their callers return.
    deadline = time.monotonic() + 2
    while len(os.listdir("/proc/self/task")) != 3 and time.monotonic() < deadline:
        time.sleep(0.001)
    assert len(os.listdir("/proc/self/task")) == 3
print(f"PASS actual Rust endpoint {mode}: starts only after cuInit")
