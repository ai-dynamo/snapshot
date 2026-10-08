# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Verify that memory exported by a process without the shim imports natively and
blocks checkpoint until every handle and mapping is released."""

import os
from pathlib import Path
import subprocess
import sys

import pytest

from cuda.bindings import driver  # noqa: E402

import cuda_driver  # noqa: E402
from cuda_driver import POSIX_FD_HANDLE_TYPE, cuda_call  # noqa: E402

BYTE = 0x5a


@pytest.mark.gpu
def test_foreign_import_blocks_checkpoint_until_released(tools, tmp_path):
    allocations = cuda_driver.create_external_allocations(1, BYTE)
    try:
        allocation = allocations[0]
        result = subprocess.run(
            [sys.executable, str(Path(__file__).resolve()), str(allocation.fd),
             str(allocation.size), str(tools.coordinator)],
            env=os.environ | {
                "LD_PRELOAD": str(tools.interposer),
                "CUINTERPOSE_SOCKET_DIR": str(tmp_path),
            },
            pass_fds=(allocation.fd,), capture_output=True, text=True, timeout=60,
        )
    finally:
        cuda_driver.destroy_external_allocations(allocations)
    assert result.returncode == 0, result.stdout + result.stderr


def run_worker(fd, size, coordinator):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    handle = cuda_call(driver.cuMemImportFromShareableHandle, fd, POSIX_FD_HANDLE_TYPE)
    os.close(fd)
    cuda_driver.assert_handle_namespace(handle, False, "foreign import")
    address = cuda_driver.map_allocation(handle, size, 0)
    cuda_driver.assert_bytes(address, bytes([BYTE]) * 32, "foreign mapping")

    socket_dir = Path(os.environ["CUINTERPOSE_SOCKET_DIR"])
    checkpoint = socket_dir / "checkpoint"
    checkpoint.mkdir()

    def coordinate(phase):
        command = [coordinator, phase, "--socket-dir", str(socket_dir),
                   "--process", str(os.getpid())]
        if phase != "--inspect":
            command += ["--checkpoint-dir", str(checkpoint)]
        return subprocess.run(command, capture_output=True, text=True, timeout=20)

    def assert_refused(handles, mappings):
        result = coordinate("--inspect")
        expected = f"{handles} foreign allocation handles and {mappings} foreign mappings"
        assert result.returncode != 0 and expected in result.stderr, result.stderr

    assert_refused(1, 1)
    cuda_call(driver.cuMemRelease, handle)
    assert_refused(0, 1)
    retained = cuda_call(driver.cuMemRetainAllocationHandle, address)
    assert_refused(1, 1)
    cuda_call(driver.cuMemUnmap, address, size)
    cuda_call(driver.cuMemAddressFree, address, size)
    assert_refused(1, 0)
    cuda_call(driver.cuMemRelease, retained)
    for phase in ("--inspect", "--prepare", "--restore"):
        result = coordinate(phase)
        assert result.returncode == 0, result.stderr
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    run_worker(int(sys.argv[1]), int(sys.argv[2]), sys.argv[3])
