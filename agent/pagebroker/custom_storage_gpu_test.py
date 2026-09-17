# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Real-driver private-memory qualification; no preload, IPC or launch-job.

Run inside a one-GPU container with the PageBroker GPU worker installed:
    python3 custom_storage_gpu_test.py /checkpoints/test-directory 64
The last argument is MiB per allocation (one cuMemAlloc, one cuMemCreate).
"""

import ctypes as C
import json
import os
from pathlib import Path
import selectors
import subprocess
import sys


def target(size):
    assert "CUDA_CHECKPOINT_JOB_FILE" not in os.environ
    assert not os.environ.get("LD_PRELOAD")
    cuda = C.CDLL("libcuda.so.1")

    def call(name, *args):
        status = getattr(cuda, name)(*args)
        if status:
            raise RuntimeError(f"{name}: CUDA error {status}")

    class Location(C.Structure):
        _fields_ = [("type", C.c_int), ("id", C.c_int)]

    class Properties(C.Structure):
        _fields_ = [
            ("type", C.c_int), ("handles", C.c_int), ("location", Location),
            ("win32", C.c_void_p), ("flags", C.c_ubyte * 8),
        ]

    class Access(C.Structure):
        _fields_ = [("location", Location), ("flags", C.c_ulonglong)]

    call("cuInit", 0)
    context = C.c_void_p()
    call("cuDevicePrimaryCtxRetain", C.byref(context), 0)
    call("cuCtxSetCurrent", context)
    legacy, vmm, handle = C.c_ulonglong(), C.c_ulonglong(), C.c_ulonglong()
    call("cuMemAlloc_v2", C.byref(legacy), C.c_size_t(size))
    properties = Properties(type=1, handles=0, location=Location(1, 0))
    granularity = C.c_size_t()
    call("cuMemGetAllocationGranularity", C.byref(granularity), C.byref(properties), 0)
    assert size % granularity.value == 0
    call("cuMemCreate", C.byref(handle), C.c_size_t(size), C.byref(properties), C.c_ulonglong(0))
    call("cuMemAddressReserve", C.byref(vmm), C.c_size_t(size), C.c_size_t(0),
         C.c_ulonglong(0), C.c_ulonglong(0))
    call("cuMemMap", vmm, C.c_size_t(size), C.c_size_t(0), handle, C.c_ulonglong(0))
    access = Access(Location(1, 0), 3)
    call("cuMemSetAccess", vmm, C.c_size_t(size), C.byref(access), C.c_size_t(1))
    # Distinct nonconstant patterns, checked over every byte, not sampled.
    patterns = [bytes(range(256)) * 4096, bytes(reversed(range(256))) * 4096]
    for address, pattern in zip((legacy, vmm), patterns):
        buffer = C.create_string_buffer(pattern)
        for offset in range(0, size, len(pattern)):
            call("cuMemcpyHtoD_v2", C.c_ulonglong(address.value + offset), buffer, C.c_size_t(len(pattern)))
    print("ready", flush=True)
    for command in sys.stdin:
        if command.strip() != "verify":
            raise RuntimeError("unexpected target command")
        for address, pattern in zip((legacy, vmm), patterns):
            buffer = C.create_string_buffer(len(pattern))
            for offset in range(0, size, len(pattern)):
                call("cuMemcpyDtoH_v2", buffer, C.c_ulonglong(address.value + offset), C.c_size_t(len(pattern)))
                assert buffer.raw == pattern, f"mismatch at {offset}"
        print("verified", flush=True)
    call("cuMemUnmap", vmm, C.c_size_t(size))
    call("cuMemAddressFree", vmm, C.c_size_t(size))
    call("cuMemRelease", handle)
    call("cuMemFree_v2", legacy)
    call("cuDevicePrimaryCtxRelease_v2", 0)


def line(process):
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        if not selector.select(timeout=240):
            raise TimeoutError(f"process {process.pid} did not respond")
    value = process.stdout.readline().strip()
    if not value:
        raise RuntimeError(f"process {process.pid} exited: {process.poll()}")
    return value


def run(directory, mib):
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    workload = subprocess.Popen(
        [sys.executable, "-u", __file__, "--target", str(mib * 1024 * 1024)],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
    )
    worker = None
    try:
        assert line(workload) == "ready"
        environ = Path(f"/proc/{workload.pid}/environ").read_bytes()
        assert b"CUDA_CHECKPOINT_JOB_FILE=" not in environ
        workload.stdin.write("verify\n")
        workload.stdin.flush()
        assert line(workload) == "verified"
        worker = subprocess.Popen(
            ["pagebroker-custom-storage-worker", str(workload.pid), str(directory)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
        )
        ready = json.loads(line(worker))
        assert ready["event"] == "ready"
        print(json.dumps(ready), flush=True)
        for iteration in range(2):
            for operation in ("save", "load"):
                worker.stdin.write(operation + "\n")
                worker.stdin.flush()
                result = json.loads(line(worker))
                assert result["event"] == operation
                assert result["bytes"] >= 2 * mib * 1024 * 1024
                print(json.dumps(dict(result, iteration=iteration)), flush=True)
            workload.stdin.write("verify\n")
            workload.stdin.flush()
            assert line(workload) == "verified"
            print(f"iteration {iteration}: all private cuMemAlloc/cuMemCreate bytes verified", flush=True)
        workload.stdin.close()
        assert workload.wait(timeout=30) == 0
        worker.stdin.close()
        assert worker.wait(timeout=30) == 0
    finally:
        if workload.poll() is None:
            workload.kill()
            workload.wait(timeout=30)
        if worker and worker.poll() is None:
            worker.kill()
            worker.wait(timeout=30)


if __name__ == "__main__":
    if sys.argv[1] == "--target":
        target(int(sys.argv[2]))
    else:
        run(Path(sys.argv[1]), int(sys.argv[2]))
