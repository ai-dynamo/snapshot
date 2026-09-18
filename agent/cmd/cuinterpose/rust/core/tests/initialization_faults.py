#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Deterministic private preparation failures, reentry, and cancellation."""

import ctypes as c
import os
from pathlib import Path
import sys
import threading
import time

cuda = c.CDLL(None)
cuda.fault_call.argtypes = [c.c_char_p]
mode = sys.argv[1]
path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
before_fd = len(os.listdir("/proc/self/fd"))
if mode in ("failure-race", "delayed"):
    result = []
    worker = threading.Thread(target=lambda: result.append(cuda.fault_call(mode.encode())))
    worker.start()
    deadline = time.monotonic() + 2
    while not cuda.fault_entered() and time.monotonic() < deadline:
        time.sleep(0.001)
    assert cuda.fault_entered()
    assert not path.exists()
    assert cuda.cuInit(0) == 0
    cuda.fault_release()
    worker.join(2)
    assert not worker.is_alive()
    # Both dispensable preparation failure and cancellation reuse the healthy
    # winner. Neither may poison it or wait for the private workers to run.
    assert result == [0], result
    assert cuda.cuInit(0) == 0
    if mode == "delayed":
        assert len(os.listdir("/proc/self/task")) == 5
        child = os.fork()
        if child == 0:
            assert cuda.cuInit(0) == 0
            os._exit(0)
        assert os.waitpid(child, 0)[1] == 0
        cuda.fault_release_workers()
elif mode == "recursive":
    assert cuda.fault_call(mode.encode()) == 0
    assert cuda.fault_recursive_result() == 3
    assert cuda.cuInit(0) == 0
elif mode in ("first-spawn", "second-spawn", "collision", "permissions"):
    if mode == "collision":
        path.write_text("owned by application")
    if mode in ("first-spawn", "second-spawn"):
        assert cuda.fault_call(mode.encode()) == 3
    else:
        assert cuda.cuInit(0) == 3
    if mode == "collision":
        assert path.read_text() == "owned by application"
        path.unlink()
    else:
        assert not path.exists()
    os.environ.pop("CUINTERPOSE_TEST_CHMOD_FAILURE", None)
    # No installed winner: failure remains sticky after the fault is removed.
    assert cuda.cuInit(0) == 3
else:
    raise AssertionError(mode)
expected = 3 if mode in ("failure-race", "delayed", "recursive") else 1
deadline = time.monotonic() + 2
while len(os.listdir("/proc/self/task")) != expected and time.monotonic() < deadline:
    time.sleep(0.001)
assert len(os.listdir("/proc/self/task")) == expected
assert len(os.listdir("/proc/self/fd")) == before_fd + (expected == 3)
print("PASS initialization", mode, "tasks", expected, "fd_delta", expected == 3, flush=True)
