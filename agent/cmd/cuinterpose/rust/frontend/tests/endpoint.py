#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Actual Rust core endpoints without VMM activity; provider has no GPU state."""

import ctypes as c
import os
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "core/tests"))
from protocol_client import inspect


driver = c.CDLL("libcuda.so.1", mode=os.RTLD_LOCAL)
cuda = c.CDLL(None)
runtime = c.CDLL("libcudart.so.13", mode=os.RTLD_LOCAL)
mode = sys.argv[1]
path = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{os.getpid()}.sock"
assert not path.exists()

if mode in ("init", "init-handle", "init-failure", "constructor"):
    initialize = driver.cuInit if mode == "init-handle" else cuda.cuInit
    initialize.argtypes = [c.c_uint]

    def activate():
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
        if mode == "resolver-startup-failure":
            # The inner resolver cannot establish an endpoint. The outer
            # runtime must preserve that refusal, not return a usable pointer.
            path.touch(exist_ok=False)
            assert query(*args) == 3 and output.value is None and status.value == 0
            path.unlink()
            output.value = 0x1234
            assert query(*args) == 3 and output.value is None and not path.exists()
            print("PASS actual Rust endpoint resolver-startup-failure: sticky refusal")
            sys.exit(0)
        assert query(*args) == 0 and output.value

activate()
parent = inspect()["participant"]
child = os.fork()
if child == 0:
    try:
        if mode == "constructor":
            os.environ["CUINTERPOSE_TEST_GENERATION_QUERY"] = "1"
            plugin = c.CDLL(sys.argv[2])
            plugin.fixture_join_generation_worker()
        else:
            activate()
        assert inspect()["participant"] != parent
        os._exit(0)
    except BaseException:
        import traceback
        traceback.print_exc()
        os._exit(1)
assert os.waitpid(child, 0)[1] == 0
assert inspect()["participant"] == parent
print(f"PASS actual Rust endpoint {mode}: parent and child, no VMM")
