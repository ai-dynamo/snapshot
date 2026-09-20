#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Fork before CUDA initialization, and rejection of inherited runtimes."""

import ctypes as c
import fcntl
import os
from pathlib import Path
import sys
import time
from protocol_client import inspect
from support import driver, props

cuda = driver()


def endpoint():
    return Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"


def descriptors():
    result = {}
    for path in Path("/proc/self/fd").iterdir():
        try:
            result[int(path.name)] = os.readlink(path)
        except FileNotFoundError:
            pass
    return result


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


def allocate():
    value = c.c_uint64()
    assert cuda.cuMemCreate(c.byref(value), 4096, c.byref(props), 0) == 0
    return value


def main():
    mode = sys.argv[1]
    if mode == "exec-check":
        assert not set(sys.argv[2:]) & set(descriptors().values())
        assert not endpoint().exists()
        assert cuda.cuInit(0) == 0
        assert inspect()["namespace_pid"] == os.getpid()
        return

    if mode == "preinit":
        before_threads = set(os.listdir("/proc/self/task"))
        before_fds = descriptors()
        # Resolve the initializer before fork: lookup must not start the shim.
        query = cuda.cuGetProcAddress
        query.argtypes = [c.c_char_p, c.POINTER(c.c_void_p), c.c_int, c.c_uint64]
        pointer = c.c_void_p()
        assert query(b"cuInit", c.byref(pointer), 13010, 0) == 0 and pointer.value
        initialize = c.CFUNCTYPE(c.c_int, c.c_uint)(pointer.value)
        assert cuda.cuInit(1) == 1
        value = c.c_uint64(42)
        assert cuda.cuMemCreate(c.byref(value), 4096, c.byref(props), 0) == 3
        assert value.value == 42
        assert not endpoint().exists()
        assert set(os.listdir("/proc/self/task")) == before_threads
        assert descriptors() == before_fds
        child = os.fork()
        if child == 0:
            assert initialize(0) == 0
            assert inspect()["namespace_pid"] == os.getpid()
            assert cuda.cuMemRelease(allocate()) == 0
            os._exit(0)
        wait(child)
        assert not endpoint().exists()
        assert initialize(0) == 0
        assert inspect()["namespace_pid"] == os.getpid()
        return

    assert cuda.cuInit(0) == 0
    value = allocate()
    share = c.c_int(-1)
    assert cuda.cuMemExportToShareableHandle(c.byref(share), value, 1, 0) == 0
    owned = {fd: name for fd, name in descriptors().items()
             if fd != share.value and ("fake-cuda" in name or name.startswith("socket:"))}
    assert len(owned) >= 2, owned
    for fd in owned:
        assert fcntl.fcntl(fd, fcntl.F_GETFD) & fcntl.FD_CLOEXEC
    child = os.fork()
    if child == 0:
        # CUDA itself rejects initialization. Shim-only operations must reject
        # too, without touching the inherited mutexes or virtual handle table.
        assert cuda.cuInit(0) == 3
        assert cuda.cuMemRelease(value) == 3
        assert cuda.cuMemCreate(c.byref(value), 4096, c.byref(props), 0) == 3
        assert not endpoint().exists()
        if mode == "exec":
            os.execv(sys.executable, [sys.executable, __file__, "exec-check", *owned.values()])
        assert mode == "rejected"
        os._exit(0)
    wait(child)
    assert inspect()["namespace_pid"] == os.getpid()
    assert cuda.cuMemRelease(value) == 0
    os.close(share.value)


if __name__ == "__main__":
    main()
    print(f"PASS fork {sys.argv[1]}", flush=True)
