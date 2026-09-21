# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Real-driver private-memory qualification; no preload, IPC or launch-job.

Generate Python protobufs with make generate-python. Run in a GPU container with
the persistent engine installed:
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


class NativeEngine:
    def __init__(self):
        import socket
        self.control, peer = socket.socketpair()
        self.control.settimeout(240)
        self.process = subprocess.Popen(
            ["sh", "-c", 'exec 3<&"$1"; exec pagebroker-gpu-engine', "engine", str(peer.fileno())],
            pass_fds=(peer.fileno(),),
        )
        peer.close()
        self.reply(self.control)

    @staticmethod
    def reply(connection):
        result, fds = receive(connection, pb.NativeSessionReply)
        assert result is not None and not fds
        if result.HasField("failure"):
            raise RuntimeError(result.failure.message)
        return result.report

    def bind(self, workload, directory, uuid, direction):
        import socket
        connection, peer = socket.socketpair()
        connection.settimeout(240)
        descriptor = os.open(directory, os.O_DIRECTORY)
        binding = internal.NativeBinding(host_pid=workload.pid)
        binding.binding.direction = direction
        binding.binding.visible_devices.append(uuid)
        try:
            send(self.control, binding, (peer.fileno(), descriptor))
        finally:
            peer.close()
            os.close(descriptor)
        assert json.loads(self.reply(connection))["persistent_engine"]
        return connection

    @staticmethod
    def command(connection, operation):
        send(connection, internal.NativeCommand(execute=pb.NativeSessionRequest(operation=operation)))

    def drain(self, connection):
        send(connection, internal.NativeCommand(drain=True))
        assert self.reply(connection) == "drained"
        connection.close()
        assert self.process.poll() is None

    def close(self):
        self.control.close()
        self.process.wait(timeout=30)


def run_multiple(directory, mib, count):
    """Reuse one engine across mixed-visibility targets, concurrent copies and cancellation."""
    uuids = subprocess.check_output(
        ["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], text=True,
    ).splitlines()
    if os.environ.get("CUDA_VISIBLE_DEVICES"):
        uuids = os.environ["CUDA_VISIBLE_DEVICES"].split(",")
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    workloads = []
    processes = []
    engine = None
    try:
        start = time.monotonic()
        engine = NativeEngine()
        print(json.dumps({"event": "engine_initialized", "seconds": time.monotonic() - start,
                          "pid": engine.process.pid}), flush=True)
        for rank in range(count):
            uuid = uuids[rank % len(uuids)]
            path = directory / str(rank)
            path.mkdir(mode=0o700)
            workload = subprocess.Popen(
                [sys.executable, "-u", __file__, "--target", str(mib * 1024 * 1024)],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
                env=dict(os.environ, CUDA_VISIBLE_DEVICES=uuid, CUSTOM_STORAGE_TEST_RANK=str(rank)),
            )
            workloads.append((workload, path, uuid))
            processes.append(workload)
            assert line(workload) == "ready"
        for cycle in range(3):
            for direction in (pb.BindAllocationSession.SAVE, pb.BindAllocationSession.LOAD):
                start = time.monotonic()
                sessions = [engine.bind(*workload, direction) for workload in workloads]
                if direction == pb.BindAllocationSession.SAVE:
                    for session in sessions:
                        engine.command(session, pb.NativeSessionRequest.LOCK)
                        assert json.loads(engine.reply(session))["event"] == "locked"
                preparations = []
                for session in sessions:
                    engine.command(session, pb.NativeSessionRequest.PREPARE)
                    preparations.append(json.loads(engine.reply(session)))
                    engine.command(session, pb.NativeSessionRequest.TRANSFER)
                transfers = [json.loads(engine.reply(session)) for session in sessions]
                assert all(result["bytes"] >= 2 * mib * 1024 * 1024 for result in transfers)
                results = []
                for session in sessions:
                    engine.command(session, pb.NativeSessionRequest.COMPLETE)
                    results.append(json.loads(engine.reply(session)))
                    engine.drain(session)
                print(json.dumps({"cycle": cycle, "direction": direction, "seconds": time.monotonic()-start,
                                  "preparations": preparations, "transfers": transfers,
                                  "results": results}), flush=True)
            for workload, _, _ in workloads:
                workload.stdin.write("verify\n")
                workload.stdin.flush()
                assert line(workload) == "verified"
            print(f"cycle {cycle}: every application byte verified across all targets", flush=True)

        # Prepared SAVE and LOAD cancellation must drain without terminating the
        # shared engine. Other restored targets remain live and byte-correct.
        for direction in (pb.BindAllocationSession.SAVE, pb.BindAllocationSession.LOAD):
            victim, path, uuid = workloads.pop()
            if direction == pb.BindAllocationSession.LOAD:
                session = engine.bind(victim, path, uuid, pb.BindAllocationSession.SAVE)
                for operation in (pb.NativeSessionRequest.LOCK, pb.NativeSessionRequest.PREPARE,
                                  pb.NativeSessionRequest.TRANSFER, pb.NativeSessionRequest.COMPLETE):
                    engine.command(session, operation)
                    engine.reply(session)
                engine.drain(session)
            session = engine.bind(victim, path, uuid, direction)
            if direction == pb.BindAllocationSession.SAVE:
                engine.command(session, pb.NativeSessionRequest.LOCK)
                engine.reply(session)
            engine.command(session, pb.NativeSessionRequest.PREPARE)
            engine.reply(session)
            engine.drain(session)
            assert victim.wait(timeout=30) == -9
            for workload, _, _ in workloads:
                workload.stdin.write("verify\n")
                workload.stdin.flush()
                assert line(workload) == "verified"
            print(json.dumps({"event": "cancelled_and_drained", "direction": direction,
                              "engine_pid": engine.process.pid}), flush=True)
        print("PERSISTENT_ENGINE_ALL_PASSED", flush=True)
    finally:
        for workload in processes:
            if workload.poll() is None:
                workload.kill()
                workload.wait(timeout=30)
        if engine:
            engine.close()


if __name__ == "__main__":
    if sys.argv[1] == "--target":
        target(int(sys.argv[2]))
    else:
        sys.path.insert(0, str(Path(__file__).resolve().parent / "build"))
        from allocation_session_test import send, receive
        from v1 import pagebroker_pb2 as pb
        import gpu_engine_pb2 as internal
        parser = argparse.ArgumentParser(description=__doc__)
        parser.add_argument("directory", type=Path)
        parser.add_argument("mib", type=int)
        parser.add_argument("--targets", type=int, default=3)
        args = parser.parse_args()
        if args.mib <= 0 or args.targets < 3:
            parser.error("use a positive allocation size and at least three targets (two are cancelled)")
        run_multiple(args.directory, args.mib, args.targets)
