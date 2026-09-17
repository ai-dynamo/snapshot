# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Real-driver private-memory qualification; no preload, IPC or launch-job.

Run inside a one-GPU container with the PageBroker GPU worker installed:
    python3 custom_storage_gpu_test.py /checkpoints/test-directory 64
The last argument is MiB per allocation (one cuMemAlloc, one cuMemCreate).
"""

import ctypes as C
import argparse
import json
import os
from pathlib import Path
import selectors
import subprocess
import sys
import time


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
    rank = int(os.environ.get("CUSTOM_STORAGE_TEST_RANK", "0"))
    pattern = bytes((value + rank * 17) % 256 for value in range(256))
    patterns = [pattern * 4096, pattern[::-1] * 4096]
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


def run_multiple(directory, mib, count):
    """Drive independent workers; native calls never execute concurrently."""
    assert "CUDA_CHECKPOINT_JOB_FILE" not in os.environ
    assert not os.environ.get("LD_PRELOAD")
    uuids = subprocess.check_output(
        ["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], text=True,
    ).splitlines()
    assert len(uuids) >= count
    assert len(set(uuids)) == len(uuids)
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    workloads, workers = [], []
    try:
        admission_start = time.monotonic()
        # Pin both processes by UUID, not ordinal; each sees exactly one GPU.
        for rank, uuid in enumerate(uuids[:count]):
            env = dict(os.environ, CUDA_VISIBLE_DEVICES=uuid,
                       CUSTOM_STORAGE_TEST_RANK=str(rank))
            path = directory / str(rank)
            path.mkdir(mode=0o700)
            workload = subprocess.Popen(
                [sys.executable, "-u", __file__, "--target", str(mib * 1024 * 1024)],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, env=env,
            )
            workloads.append(workload)
            assert line(workload) == "ready"
            environ = Path(f"/proc/{workload.pid}/environ").read_bytes()
            assert b"CUDA_CHECKPOINT_JOB_FILE=" not in environ
            worker = subprocess.Popen(
                ["pagebroker-custom-storage-worker", str(workload.pid), str(path)],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, env=env,
            )
            workers.append(worker)
            ready = json.loads(line(worker))
            assert ready["event"] == "ready"
            print(json.dumps(dict(ready, rank=rank, uuid=uuid)), flush=True)
        print(json.dumps({"event": "admitted", "seconds": time.monotonic() - admission_start,
                          "includes_target_allocation_and_seeding": True}), flush=True)
        for workload in workloads:
            workload.stdin.write("verify\n")
            workload.stdin.flush()
        for workload in workloads:
            assert line(workload) == "verified"

        # ABBA balances first-touch and run-order effects. Save uses the same
        # schedule in every cycle; only the LOAD schedule changes.
        for iteration, mode in enumerate(("sequential", "pipeline", "pipeline", "sequential")):
            for operation in ("save", "load"):
                started = time.monotonic()
                if operation == "save":
                    for worker in workers:
                        worker.stdin.write("lock\n")
                        worker.stdin.flush()
                        assert json.loads(line(worker))["event"] == "locked"
                pipeline = operation == "load" and mode == "pipeline"
                preparations, transfers = [], []
                for rank, worker in enumerate(workers):
                    worker.stdin.write(f"prepare-{operation}\n")
                    worker.stdin.flush()
                    prepared = json.loads(line(worker))
                    assert prepared["event"] == "prepared"
                    preparations.append(prepared)
                    if pipeline:
                        worker.stdin.write("transfer\n")
                        worker.stdin.flush()
                if not pipeline:
                    for worker in workers:
                        worker.stdin.write("transfer\n")
                        worker.stdin.flush()
                # All transfers must succeed before ANY target receives COMPLETE.
                for rank, worker in enumerate(workers):
                    transferred = json.loads(line(worker))
                    assert transferred["event"] == "transferred"
                    assert transferred["bytes"] >= 2 * mib * 1024 * 1024
                    transfers.append(transferred)
                results = []
                for worker in workers:
                    worker.stdin.write("complete\n")
                    worker.stdin.flush()
                    result = json.loads(line(worker))
                    assert result["event"] == "complete"
                    results.append(result)
                ended = time.monotonic()
                # Native intervals must be disjoint. Compute actual overlap with
                # the UNION of copy intervals, not a sum of concurrent copies.
                spans = sorted((x["transfer_start_ns"], x["transfer_end_ns"]) for x in transfers)
                merged = []
                for begin, end in spans:
                    if merged and begin <= merged[-1][1]:
                        merged[-1][1] = max(merged[-1][1], end)
                    else:
                        merged.append([begin, end])
                overlap = 0
                previous = 0
                for prepared in preparations:
                    begin, end = prepared["prepare_start_ns"], prepared["prepare_end_ns"]
                    assert begin >= previous
                    previous = end
                    overlap += sum(max(0, min(end, right) - max(begin, left))
                                   for left, right in merged)
                print(json.dumps({
                    "event": "batch", "operation": operation, "iteration": iteration,
                    "mode": mode if operation == "load" else "sequential",
                    "total_seconds": ended - started,
                    "native_transfer_overlap_seconds": overlap / 1e9,
                    "bytes": sum(item["bytes"] for item in transfers),
                    "preparations": preparations, "transfers": transfers, "results": results,
                }), flush=True)
            for workload in workloads:
                workload.stdin.write("verify\n")
                workload.stdin.flush()
            for workload in workloads:
                assert line(workload) == "verified"
            print(f"iteration {iteration}: all {count} ranks' private bytes verified", flush=True)
        for workload in workloads:
            workload.stdin.close()
        for workload in workloads:
            assert workload.wait(timeout=30) == 0
        for worker in workers:
            worker.stdin.close()
        for worker in workers:
            assert worker.wait(timeout=30) == 0
    finally:
        # No public abort exists: stop all targets before discarding worker contexts.
        for process in workloads + workers:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=30)


if __name__ == "__main__":
    if sys.argv[1] == "--target":
        target(int(sys.argv[2]))
    else:
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("directory", type=Path)
        parser.add_argument("mib", type=int)
        parser.add_argument("--targets", type=int, default=1)
        args = parser.parse_args()
        if args.mib <= 0 or args.targets <= 0:
            parser.error("allocation size and target count must be positive")
        if args.targets == 1:
            run(args.directory, args.mib)
        else:
            run_multiple(args.directory, args.mib, args.targets)
