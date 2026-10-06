# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Reconstruct backing retained by importers after its creator releases local state."""

import ctypes
from multiprocessing import Pipe
from multiprocessing.connection import Connection
from multiprocessing.reduction import recv_handle, send_handle
import os
from pathlib import Path
import subprocess
import sys

import msgpack
import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import POSIX_FD_HANDLE_TYPE, cuda_call


HEAD = b"full backing starts here"
TAIL = b"full backing ends here"


def receive(channel, timeout=30):
    assert channel.poll(timeout), "VMM worker did not reply"
    return channel.recv()


def map_range(handle, size, offset, device, node):
    address = int(cuda_call(driver.cuMemAddressReserve, size, 0, 0, 0))
    cuda_call(driver.cuMemMap, address, size, offset, handle, 0)
    access = driver.CUmemAccessDesc()
    access.location.type = (driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_DEVICE if node < 0
                            else driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_HOST_NUMA)
    access.location.id = device if node < 0 else node
    access.flags = driver.CUmemAccess_flags.CU_MEM_ACCESS_FLAGS_PROT_READWRITE
    cuda_call(driver.cuMemSetAccess, address, size, [access], 1)
    return address


def unmap(address, size):
    cuda_call(driver.cuMemUnmap, address, size)
    cuda_call(driver.cuMemAddressFree, address, size)


def write(address, value, node):
    if node < 0:
        cuda_driver.write_bytes(address, value)
    else:
        ctypes.memmove(address, value, len(value))


def verify(address, value, node):
    if node < 0:
        cuda_driver.assert_bytes(address, value, "orphan VMM reconstruction")
    else:
        assert ctypes.string_at(address, len(value)) == value


@pytest.mark.gpu
@pytest.mark.parametrize("retention", ["handle", "mapping"])
@pytest.mark.parametrize("location", ["device", pytest.param("host_numa", marks=pytest.mark.host_numa)])
def test_importers_outlive_creator_local_references(retention, location, gpu_environment, tmp_path):
    """Two holders share one full restored backing, with the creator still captured.

    CUDA native checkpoint runs between shim prepare and restore. This does not run
    CRIU. The elected holder uses GPU 1 while the DEVICE backing originates on GPU 0.
    """
    control = tmp_path / "control"
    control.mkdir()
    env = os.environ | {"LD_PRELOAD": str(gpu_environment.tools.interposer),
                        "SNAPSHOT_CONTROL_DIR": str(control),
                        "CUDA_VISIBLE_DEVICES": ",".join(gpu_environment.gpus)}
    processes, channels = [], []
    descriptor = None

    def start(role, device, *arguments, pass_fds=()):
        channel, child = Pipe()
        try:
            process = subprocess.Popen(
                [sys.executable, str(Path(__file__).resolve()), role, str(device),
                 str(child.fileno()), *map(str, arguments)],
                pass_fds=(child.fileno(), *pass_fds), env=env,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
        except BaseException:
            channel.close()
            raise
        finally:
            child.close()
        processes.append(process)
        channels.append(channel)
        return process, channel

    def coordinate(phase, participants, checkpoint=None):
        command = [str(gpu_environment.tools.coordinator), phase, "--control-dir", str(control)]
        for participant in participants:
            command += ["--process", str(participant.pid)]
        if checkpoint is not None:
            command += ["--checkpoint-dir", str(checkpoint)]
        return subprocess.run(command, capture_output=True, text=True, timeout=30)

    try:
        creator, creator_channel = start("creator", 0, location)
        size, node = receive(creator_channel)
        descriptor = recv_handle(creator_channel)
        for device in (1, 0):
            _, channel = start("importer", device, size, node, descriptor, retention,
                               pass_fds=(descriptor,))
            assert receive(channel) == "ready"
        os.close(descriptor)
        descriptor = None
        creator_channel.send("release")
        assert receive(creator_channel) == "released"

        # A surviving import does not permit omitting its original creator process.
        omitted = coordinate("--inspect", processes[1:])
        assert omitted.returncode != 0 and "missing creator" in omitted.stderr, omitted.stderr
        for cycle in range(2):
            checkpoint = tmp_path / f"checkpoint-{cycle}"
            checkpoint.mkdir()
            result = coordinate("--prepare", processes, checkpoint)
            assert result.returncode == 0, result.stderr
            state = msgpack.unpackb((checkpoint / "cuinterpose.state").read_bytes(), raw=False,
                                    strict_map_key=False)
            manifest = state["body"]
            assert manifest[creator.pid] == [], "the creator must not retain synthetic allocation state"
            for peer in processes[1:]:
                allocations = [entry["allocation"] for entry in manifest[peer.pid] if "allocation" in entry]
                assert len(allocations) == 1 and allocations[0]["size"] == size
            cuda_driver.native_checkpoint(tuple(process.pid for process in processes),
                                          command_timeout_seconds=30, checkpoint_timeout_seconds=90)
            result = coordinate("--restore", processes, checkpoint)
            assert result.returncode == 0, result.stderr
            for channel in channels[1:]:
                channel.send(("verify", HEAD))
                assert receive(channel) == "ok"
            # A write through one holder must be seen through the other holder.
            changed = bytes([0x60 + cycle]) * len(HEAD)
            channels[1].send(("write", changed))
            assert receive(channels[1]) == "ok"
            channels[2].send(("verify", changed))
            assert receive(channels[2]) == "ok"
            channels[2].send(("write", HEAD))
            assert receive(channels[2]) == "ok"
        for channel in channels:
            channel.send("stop")
        for process in processes:
            stdout, stderr = process.communicate(timeout=20)
            assert process.returncode == 0, stdout + stderr
    finally:
        if descriptor is not None:
            os.close(descriptor)
        for channel in channels:
            channel.close()
        for process in processes:
            if process.poll() is None:
                process.kill()
            stdout, stderr = process.communicate(timeout=10)
            if process.returncode:
                print(f"worker {process.pid}: {stdout}\n{stderr}")


def allocation_properties(handle):
    properties = cuda_call(driver.cuMemGetAllocationPropertiesFromHandle, handle)
    return (int(properties.type), int(properties.requestedHandleTypes),
            int(properties.location.type), properties.location.id,
            properties.allocFlags.compressionType, properties.allocFlags.gpuDirectRDMACapable,
            properties.allocFlags.usage)


def worker(role, device, channel_fd, arguments):
    channel = Connection(channel_fd)
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, device)
    cuda_call(driver.cuCtxSetCurrent, context)
    if role == "creator":
        properties = cuda_driver.allocation_properties(device)
        node = -1
        if arguments[0] == "host_numa":
            node = max(0, int(cuda_call(driver.cuDeviceGetAttribute,
                       driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID, device)))
            properties.location.type = driver.CUmemLocationType.CU_MEM_LOCATION_TYPE_HOST_NUMA
            properties.location.id = node
        granularity = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                          driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
        size = 2 * granularity
        handle = cuda_call(driver.cuMemCreate, size, properties, 0)
        address = map_range(handle, size, 0, device, node)
        write(address, HEAD, node)
        write(address + size - len(TAIL), TAIL, node)
        descriptor = int(cuda_call(driver.cuMemExportToShareableHandle, handle, POSIX_FD_HANDLE_TYPE, 0))
        channel.send((size, node))
        send_handle(channel, descriptor, os.getppid())
        os.close(descriptor)
        assert receive(channel) == "release"
        unmap(address, size)
        cuda_call(driver.cuMemRelease, handle)
        channel.send("released")
        assert receive(channel, 180) == "stop"
    else:
        size, node, descriptor = map(int, arguments[:3])
        retention = arguments[3]
        handle = cuda_call(driver.cuMemImportFromShareableHandle, descriptor, POSIX_FD_HANDLE_TYPE)
        os.close(descriptor)
        original_properties = allocation_properties(handle)
        mapped = 0
        if retention == "mapping":
            mapped = map_range(handle, size, 0, device, node)
            cuda_call(driver.cuMemRelease, handle)
        channel.send("ready")
        while True:
            command = receive(channel, 180)
            if command == "stop":
                break
            operation, value = command
            if mapped:
                handle = cuda_call(driver.cuMemRetainAllocationHandle, mapped)
            assert allocation_properties(handle) == original_properties, "restored properties changed"
            address = map_range(handle, size, 0, device, node)
            if operation == "verify":
                verify(address, value, node)
                verify(address + size - len(TAIL), TAIL, node)
            else:
                assert operation == "write"
                write(address, value, node)
            unmap(address, size)
            if mapped:
                cuda_call(driver.cuMemRelease, handle)
            channel.send("ok")
        if mapped:
            unmap(mapped, size)
        else:
            cuda_call(driver.cuMemRelease, handle)
    cuda_call(driver.cuCtxSetCurrent, 0)
    cuda_call(driver.cuDevicePrimaryCtxRelease, device)
    channel.close()


if __name__ == "__main__":
    worker(sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4:])
