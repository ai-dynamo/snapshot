#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Strict cold/postfork concurrency and bounded private-worker retirement."""

import ctypes as c
import os
from pathlib import Path
import sys
import threading
import time

from protocol_client import request, reply

root = Path(os.environ["SNAPSHOT_CONTROL_DIR"])
cuda = c.CDLL(None)
cuda.cuInit.argtypes = [c.c_uint]
mode = sys.argv[1]
forked = mode in ("fork", "fork-constructor")
if forked:
    assert cuda.cuInit(0) == 0
    child = os.fork()
    if child:
        assert os.waitpid(child, 0)[1] == 0
        sys.exit(0)

baseline_fds = len(list(Path("/proc/self/fd").iterdir()))
if mode.endswith("constructor"):
    plugin = c.CDLL(sys.argv[2])
    plugin.fixture_join_generation_worker()
else:
    count = int(sys.argv[2])
    barrier = threading.Barrier(count)
    results = [None] * count

    def call(index):
        barrier.wait()
        results[index] = cuda.cuInit(0)

    threads = [threading.Thread(target=call, args=(i,)) for i in range(count)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    assert results == [0] * count, results

# Detached losing workers may still be starting when callers return. They must
# eventually retire, not be joined by a caller that might hold the loader lock.
deadline = time.monotonic() + 2
while len(list(Path("/proc/self/task").iterdir())) != 3 and time.monotonic() < deadline:
    time.sleep(0.001)
assert len(list(Path("/proc/self/task").iterdir())) == 3
names = [task.joinpath("comm").read_text().strip() for task in Path("/proc/self/task").iterdir()]
assert names.count("cuinterpose-pee") == names.count("cuinterpose-con") == 1, names
path = root / f"cuinterpose-{os.getpid()}.sock"
inode = path.stat().st_ino
identity = reply(request(str(path), "identify"))["participant"]
for _ in range(16):
    assert cuda.cuInit(0) == 0
    assert reply(request(str(path), "identify"))["participant"] == identity
    assert path.stat().st_ino == inode
deadline = time.monotonic() + 2
while len(list(Path("/proc/self/fd").iterdir())) != baseline_fds + 1 and time.monotonic() < deadline:
    time.sleep(0.001)
assert len(list(Path("/proc/self/fd").iterdir())) == baseline_fds + 1
print(f"PASS initialization {mode}: all callers succeed, singleton workers/endpoint/fd", flush=True)
if forked:
    os._exit(0)
