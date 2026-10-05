# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Real CUDA callbacks expose the interval between unbind and its metadata commit."""

from concurrent.futures import Future
import ctypes
from importlib.util import find_spec
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import threading

import msgpack
import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import cuda_call


@pytest.fixture(scope="module")
def multicast_gate(tmp_path_factory):
    # The CUPTI wheel uses CUDA headers from the installed development toolkit.
    spec = find_spec("nvidia.cu13")
    assert spec is not None, "requires the CUDA 13 CUPTI package installed with torch"
    toolkit = Path(next(iter(spec.submodule_search_locations)))
    output = tmp_path_factory.mktemp("multicast-gate") / "gate.so"
    result = subprocess.run([
        "cc", "-std=c11", "-D_POSIX_C_SOURCE=200809L", "-O2", "-Wall", "-Wextra",
        "-Werror", "-shared", "-fPIC", "-pthread",
        "-I", os.environ.get("CUDA_INCLUDE", "/usr/local/cuda/include"),
        "-I", str(toolkit / "include"),
        str(Path(__file__).with_name("fixtures") / "multicast_gate.c"),
        str(toolkit / "lib/libcupti.so.13"), f"-Wl,-rpath,{toolkit / 'lib'}",
        "-o", str(output),
    ], capture_output=True, text=True, timeout=120)
    if result.returncode != 0:
        pytest.fail(f"fixture build failed:\n{result.stdout}{result.stderr}")
    return output


@pytest.mark.gpu
@pytest.mark.multicast
@pytest.mark.parametrize("case", ["rebind", "native-failure", "membership"])
def test_multicast_unbind_locking(case, multicast_supported, multicast_gate, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    environment = os.environ | {
        "LD_PRELOAD": str(tools.interposer),
        "SNAPSHOT_CONTROL_DIR": str(control),
    }
    result = subprocess.run(
        [sys.executable, str(Path(__file__).resolve()), case, str(multicast_gate)],
        env=environment, capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, result.stdout + result.stderr


def inspect_reply(kind="inspect", pid=None):
    """Use the same inspection protocol as the coordinator to assert exact records."""
    pid = os.getpid() if pid is None else pid
    request = msgpack.packb({"version": 1, "body": {
        "kind": kind, "namespace_pid": pid,
    }}, use_bin_type=True)
    endpoint = Path(os.environ["SNAPSHOT_CONTROL_DIR"]) / f"cuinterpose-{pid}.sock"
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(5)
        connection.connect(str(endpoint))
        connection.sendall(struct.pack("<I", len(request)) + request)
        with connection.makefile("rb") as stream:
            size, = struct.unpack("<I", stream.read(4))
            response = msgpack.unpackb(stream.read(size), raw=False)
    assert response["version"] == 1
    assert response["body"]["namespace_pid"] == pid
    return response["body"]["result"]


def records(pid=None):
    reply = inspect_reply(pid=pid)
    assert "Ok" in reply, reply
    return reply["Ok"]["inspection"]["records"]


def start_thread(context, operation):
    future = Future()

    def run():
        try:
            cuda_call(driver.cuCtxSetCurrent, context)
            future.set_result(operation())
        except BaseException as error:
            future.set_exception(error)

    threading.Thread(target=run, daemon=True).start()
    return future


def start_gate(path):
    gate = ctypes.CDLL(path)
    gate.multicast_gate_wait.argtypes = [ctypes.c_int, ctypes.c_int]
    gate.multicast_gate_release.restype = None
    assert gate.multicast_gate_start() == 0, "CUPTI subscription failed"
    return gate


def run_worker(case, gate_path):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = driver.CUmulticastObjectProp()
    properties.numDevices = 2
    properties.handleTypes = cuda_driver.POSIX_FD_HANDLE_TYPE
    granularity = int(cuda_call(
        driver.cuMulticastGetGranularity, properties,
        driver.CUmulticastGranularity_flags.CU_MULTICAST_GRANULARITY_MINIMUM,
    ))
    properties.size = granularity
    group = cuda_call(driver.cuMulticastCreate, properties)
    cuda_driver.assert_handle_namespace(group, True, "multicast group")
    cuda_call(driver.cuMulticastAddDevice, group, 0)
    if case != "membership":
        cuda_call(driver.cuMulticastAddDevice, group, 1)
    allocation_properties = cuda_driver.allocation_properties(0)
    original = cuda_call(driver.cuMemCreate, granularity, allocation_properties, 0)
    replacement = cuda_call(driver.cuMemCreate, 2 * granularity, allocation_properties, 0)
    if case == "membership":
        gate = start_gate(gate_path)
        binding = start_thread(context, lambda: cuda_call(
            driver.cuMulticastBindMem, group, 0, original, 0, granularity, 0,
        ))
        assert gate.multicast_gate_wait(1, 10000), "native bind entry was not observed"
        assert not binding.done(), "bind completed before all devices joined"
        joining = start_thread(context, lambda: cuda_call(driver.cuMulticastAddDevice, group, 1))
        joining.result(timeout=10)
        binding.result(timeout=10)
        assert gate.multicast_gate_stop() == 0
        current = records()
        assert len([record for record in current if "multicast_binding" in record]) == 1
        assert len([record for record in current if "multicast_device" in record]) == 2
        cuda_call(driver.cuMulticastUnbind, group, 0, 0, granularity)
        for handle in (group, original, replacement):
            cuda_call(driver.cuMemRelease, handle)
        cuda_call(driver.cuDevicePrimaryCtxRelease, 0)
        return
    cuda_call(driver.cuMulticastBindMem, group, 0, original, 0, granularity, 0)
    before = records()
    replacement_reference = next(
        record["allocation"]["allocation"] for record in before
        if "allocation" in record and record["allocation"]["size"] == 2 * granularity
    )

    gate = start_gate(gate_path)
    unbind = rebind = query = None
    try:
        # An invalid device reaches the real driver without invoking CUDA's
        # undefined behavior for an unbind range that differs from the bind range.
        device = 0 if case == "rebind" else -1
        unbind = start_thread(context, lambda: driver.cuMulticastUnbind(group, device, 0, granularity))
        assert gate.multicast_gate_wait(0, 10000), "native unbind exit was not observed"
        native_status = gate.multicast_gate_status()
        assert (native_status == 0) == (case == "rebind"), native_status

        # This query needs ProcessState but no access to the multicast object. The
        # old implementation keeps that mutex in unbind and stalls here.
        query = start_thread(context, lambda: cuda_call(
            driver.cuMemGetAllocationPropertiesFromHandle, replacement,
        ))
        assert query.result(timeout=5).location.id == 0
        for kind in ("inspect", "begin_checkpoint"):
            reply = inspect_reply(kind)
            assert "Err" in reply, reply

        if case == "rebind":
            rebind = start_thread(context, lambda: cuda_call(
                driver.cuMulticastBindMem, group, 0, replacement, 0, granularity, 0,
            ))
            assert not gate.multicast_gate_wait(1, 1000), (
                "same-object bind entered CUDA before unbind committed its metadata"
            )
            assert not rebind.done(), "same-object bind did not wait for unbind"
            reply = inspect_reply("begin_checkpoint")
            assert "Err" in reply, reply
    finally:
        gate.multicast_gate_release()
        for future in (unbind, query, rebind):
            if future is not None:
                future.result(timeout=10)
        assert gate.multicast_gate_stop() == 0

    assert int(unbind.result()[0]) == native_status
    after = records()
    if case == "rebind":
        bindings = [record["multicast_binding"] for record in after if "multicast_binding" in record]
        assert len(bindings) == 1, bindings
        assert bindings[0]["source"]["memory"]["allocation"] == replacement_reference
        assert bindings[0]["size"] == granularity
        assert bindings[0]["offset"] == 0 and bindings[0]["device"] == 0
    else:
        assert after == before, "failed unbind changed tracked state"
    cuda_call(driver.cuMulticastUnbind, group, 0, 0, granularity)
    for handle in (group, original, replacement):
        cuda_call(driver.cuMemRelease, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    run_worker(*sys.argv[1:])
