# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""A lost successful load reply must not leave a process holding its host carrier."""

from multiprocessing import Pipe
from multiprocessing.connection import Connection
import os
from pathlib import Path
import resource
import signal
import socket
import struct
import subprocess
import sys

import msgpack
import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import POSIX_FD_HANDLE_TYPE, cuda_call


def request(control, pid, body, *, discard_reply=False):
    payload = msgpack.packb({"version": 2, "body": body | {"namespace_pid": pid}},
                           use_bin_type=True)
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(10)
        connection.connect(str(control / f"cuinterpose-{pid}.sock"))
        if discard_reply:
            # UNIX sockets reject the peer's send after local read shutdown, even
            # though this side can still deliver the complete request.
            connection.shutdown(socket.SHUT_RD)
        connection.sendall(struct.pack("<I", len(payload)) + payload)
        if discard_reply:
            return None
        with connection.makefile("rb") as stream:
            size, = struct.unpack("<I", stream.read(4))
            response = msgpack.unpackb(stream.read(size), raw=False)
    assert response["version"] == 2
    assert response["body"]["namespace_pid"] == pid
    return response["body"]["result"]


@pytest.mark.gpu
def test_successful_load_with_closed_reply_aborts(tools, tmp_path):
    """Use real shared VMM backing and carrier copies, without CRIU or native suspend."""
    control = tmp_path / "control"
    control.mkdir()
    environment = os.environ | {"LD_PRELOAD": str(tools.interposer),
                                "SNAPSHOT_CONTROL_DIR": str(control)}
    channel, child = Pipe()
    process = None
    try:
        process = subprocess.Popen(
            [sys.executable, str(Path(__file__).resolve()), str(child.fileno())],
            pass_fds=(child.fileno(),), env=environment,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        child.close()
        assert channel.poll(20), "CUDA worker did not become ready"
        allocation_size = channel.recv()
        load = {"kind": "execute", "operation": "load_allocations"}

        # Read-only and rejected operation reply failures must not abort. The next
        # response also confirms the serial control worker processed both requests.
        request(control, process.pid, {"kind": "inspect"}, discard_reply=True)
        request(control, process.pid, load, discard_reply=True)
        reply = request(control, process.pid, load)
        assert "Err" in reply and "LoadAllocations" in reply["Err"], reply
        assert process.poll() is None

        checkpoint = tmp_path / "checkpoint"
        checkpoint.mkdir()
        prepared = subprocess.run([
            str(tools.coordinator), "--prepare", "--control-dir", str(control),
            "--checkpoint-dir", str(checkpoint), "--process", str(process.pid),
        ], capture_output=True, text=True, timeout=30)
        assert prepared.returncode == 0, prepared.stdout + prepared.stderr
        state = msgpack.unpackb((checkpoint / "cuinterpose.state").read_bytes(), raw=False,
                                strict_map_key=False)
        allocations = [record["allocation"] for record in state["body"][process.pid]
                       if "allocation" in record]
        assert len(allocations) == 1 and allocations[0]["shared"]
        assert allocations[0]["size"] == allocation_size

        request(control, process.pid, load, discard_reply=True)
        stdout, stderr = process.communicate(timeout=20)
        assert process.returncode == -signal.SIGABRT, stdout + stderr
        # This diagnostic is emitted only after the real load operation succeeds.
        assert "cannot send successful LoadAllocations reply" in stderr, stderr
    finally:
        child.close()
        channel.close()
        if process is not None:
            if process.poll() is None:
                process.kill()
            process.communicate(timeout=10)


def worker(channel_fd):
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    channel = Connection(channel_fd)
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    address = cuda_driver.map_allocation(handle, size, 0)
    cuda_driver.write_bytes(address, b"load reply carrier")
    descriptor = int(cuda_call(driver.cuMemExportToShareableHandle,
                               handle, POSIX_FD_HANDLE_TYPE, 0))
    os.close(descriptor)
    channel.send(size)
    channel.recv()


if __name__ == "__main__":
    worker(int(sys.argv[1]))
