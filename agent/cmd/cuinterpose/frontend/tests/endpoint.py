#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Test the Rust control endpoint with the installed NVIDIA driver."""

import ctypes as c
import os
from pathlib import Path
import sys
import threading
import signal
import socket
import stat
import struct
import time

import msgpack


def request(kind, **fields):
    body = msgpack.packb({"version": 2, "body": {
        "kind": kind, "namespace_pid": os.getpid(), **fields,
    }}, use_bin_type=True)
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(path.with_name(f"cuinterpose-{os.getpid()}.sock")))
        connection.sendall(struct.pack("<I", len(body)) + body)
        with connection.makefile("rb") as stream:
            length, = struct.unpack("<I", stream.read(4))
            response = msgpack.unpackb(stream.read(length), raw=False)
    assert response["version"] == 2
    return response["body"]


def inspect():
    response = request("inspect")
    assert "Ok" in response["result"]
    return response


driver = c.CDLL("libcuda.so.1", mode=os.RTLD_LOCAL)
cuda = c.CDLL(None)
mode = sys.argv[1]
path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
if mode == "init-after-exec":
    assert os.getpid() == int(sys.argv[2]) and path.is_socket()
else:
    assert not path.exists()
sockets_before = set(path.parent.glob("cuinterpose-*.sock"))

if mode == "permissive-umask":
    # The child changes umask before initialization so the test can verify that runtime
    # startup leaves it unchanged.
    os.umask(0)

if mode == "relative-preload-chdir":
    assert not Path(os.environ["LD_PRELOAD"]).is_absolute()
    assert Path(os.environ["LD_PRELOAD"]).is_file()
    os.chdir(sys.argv[2])
    assert not Path(os.environ["LD_PRELOAD"]).exists()

if mode == "same-pid-exec":
    assert cuda.cuInit(0) == 0
    assert inspect()["namespace_pid"] == os.getpid()
    os.execv(sys.executable, [sys.executable, __file__, "init-after-exec", str(os.getpid())])

if mode in ("stale", "stale-concurrent"):
    with socket.socket(socket.AF_UNIX) as stale:
        stale.bind(str(path))
        stale.listen()

if mode in ("existing-file", "existing-symlink", "existing-live", "existing-full"):
    if mode == "existing-file":
        path.write_text("preserve this file")
    elif mode == "existing-symlink":
        target = path.with_suffix(".target")
        with socket.socket(socket.AF_UNIX) as stale:
            stale.bind(str(target))
        path.symlink_to(target)
        target_inode = target.stat().st_ino
    else:
        listener = socket.socket(socket.AF_UNIX)
        listener.bind(str(path))
        listener.listen(0 if mode == "existing-full" else 1)
        if mode == "existing-full":
            queued = socket.socket(socket.AF_UNIX)
            queued.connect(str(path))
    before = path.lstat()
    # A full socket backlog must fail without waiting for a timeout under the
    # installation lock, since a loader constructor can hold that lock.
    signal.alarm(5)
    assert cuda.cuInit(0) == 304  # CUDA_ERROR_OPERATING_SYSTEM
    signal.alarm(0)
    after = path.lstat()
    assert (before.st_dev, before.st_ino, before.st_mode) == (after.st_dev, after.st_ino, after.st_mode)
    if mode == "existing-file":
        assert path.read_text() == "preserve this file"
    elif mode == "existing-symlink":
        assert path.is_symlink() and target.stat().st_ino == target_inode
        target.unlink()
    path.unlink()
    assert cuda.cuInit(0) == 3 and not path.exists()  # Later calls still fail.
    print(f"PASS actual Rust endpoint {mode}: preserved existing endpoint")
    sys.exit(0)

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
        c.CDLL(sys.argv[2])

elif mode in ("init", "init-handle", "init-failure", "concurrent", "init-after-exec", "stale", "stale-concurrent",
              "relative-preload-chdir", "permissive-umask", "out-of-order"):
    initialize = driver.cuInit if mode == "init-handle" else cuda.cuInit
    initialize.argtypes = [c.c_uint]

    def activate():
        if mode in ("concurrent", "stale-concurrent"):
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

else:
    runtime = c.CDLL("libcudart.so.13", mode=os.RTLD_LOCAL)
    names = ["cuGetProcAddress", "cuGetProcAddress_v2", "cuGetProcAddress_v2_ptsz",
             "cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
             "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"]
    index = 1 if mode in ("tracked-query", "resolver-startup-failure") else int(mode)
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
        tracked = mode in ("tracked-query", "resolver-startup-failure")
        args = [b"cuMemCreate" if tracked else b"cuDriverGetVersion", c.byref(output)]
        args += [13010, 0] if index < 3 or index >= 5 else [0]
        if index:
            args += [c.byref(status)]
        # Even a blocked endpoint must not turn a successful lookup into failure.
        if mode == "resolver-startup-failure":
            path.touch(exist_ok=False)
        assert query(*args) == 0 and output.value
        if mode == "resolver-startup-failure":
            assert cuda.cuInit(0) == 304
            path.unlink()
            assert query(*args) == 0 and output.value
            assert cuda.cuInit(0) == 3 and not path.exists()
        else:
            # libcudart may call cuInit while resolving an entry point. Driver
            # procedure queries themselves must not activate our endpoint.
            if index < 3:
                assert not path.exists()
            assert cuda.cuInit(0) == 0

activate()
if mode == "out-of-order":
    error = request("save_allocations", owners=[])["result"]["Err"]
    assert "SaveAllocations" in error and "expected MulticastPrepared" in error and "actual Active" in error, error
    assert "Ok" in request("begin_checkpoint")["result"]  # Phase remains unchanged.
if mode == "permissive-umask":
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert os.umask(0) == 0, "runtime startup changed the application's umask"
if mode == "resolver-startup-failure":
    print("PASS lookup independent of runtime startup failure")
    sys.exit(0)
parent = inspect()["namespace_pid"]
if mode in ("concurrent", "stale-concurrent"):
    inode = path.stat().st_ino
    for _ in range(16):
        assert initialize(0) == 0
        assert inspect()["namespace_pid"] == parent
    assert path.stat().st_ino == inode
    assert set(path.parent.glob("cuinterpose-*.sock")) == sockets_before | {path}
    # Count only the shim's named service threads because CUDA also starts its own
    # workers.
    def shim_workers():
        return [thread for thread in Path("/proc/self/task").iterdir()
                if (thread / "comm").read_text().startswith("cuinterpose-")]

    # Workers from unused initialization attempts exit after their callers return.
    deadline = time.monotonic() + 2
    while len(shim_workers()) != 2 and time.monotonic() < deadline:
        time.sleep(0.001)
    assert len(shim_workers()) == 2
print(f"PASS actual Rust endpoint {mode}: starts only after cuInit")
