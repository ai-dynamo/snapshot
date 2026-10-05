# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Release real imported backing when the core cannot accept its properties."""

from importlib.util import find_spec
import os
from pathlib import Path
import select
import subprocess
import sys

import pytest
from cuda.bindings import driver

import cuda_driver
from cuda_driver import POSIX_FD_HANDLE_TYPE, cuda_call
from test_multicast_locking import records


@pytest.fixture(scope="module")
def import_cleanup_client(tools, tmp_path_factory):
    spec = find_spec("nvidia.cu13")
    assert spec is not None, "requires the CUDA 13 headers installed with torch"
    toolkit = Path(next(iter(spec.submodule_search_locations)))
    output = tmp_path_factory.mktemp("import-cleanup") / "import-cleanup"
    result = subprocess.run([
        "cc", "-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror",
        "-I", str(toolkit / "include"), "-I", str(tools.interposer.parent),
        str(Path(__file__).with_name("fixtures") / "import_cleanup.c"),
        "-ldl", "-o", str(output),
    ], capture_output=True, text=True, timeout=120)
    if result.returncode != 0:
        pytest.fail(f"fixture build failed:\n{result.stdout}{result.stderr}")
    return output


@pytest.mark.gpu
@pytest.mark.parametrize("fault", [1, 2], ids=["property-query", "property-validation"])
def test_import_cleanup(fault, import_cleanup_client, tools, tmp_path):
    control = tmp_path / "control"
    control.mkdir()
    result = subprocess.run([
        sys.executable, str(Path(__file__).resolve()), str(import_cleanup_client),
        str(tools.interposer.with_name("libcuinterpose_core.so")), str(fault),
    ], env=os.environ | {
        "LD_PRELOAD": str(tools.interposer), "SNAPSHOT_CONTROL_DIR": str(control),
    }, capture_output=True, text=True, timeout=60)
    assert result.returncode == 0, result.stdout + result.stderr


def run_creator(client, core, fault):
    cuda_call(driver.cuInit, 0)
    context = cuda_call(driver.cuDevicePrimaryCtxRetain, 0)
    cuda_call(driver.cuCtxSetCurrent, context)
    properties = cuda_driver.allocation_properties(0)
    size = int(cuda_call(driver.cuMemGetAllocationGranularity, properties,
                        driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_MINIMUM))
    handle = cuda_call(driver.cuMemCreate, size, properties, 0)
    address = cuda_driver.map_allocation(handle, size, 0)
    contents = b"rollback import still works\0"
    cuda_driver.write_bytes(address, contents)
    descriptor = int(cuda_call(driver.cuMemExportToShareableHandle, handle, POSIX_FD_HANDLE_TYPE, 0))
    environment = os.environ.copy()
    environment.pop("LD_PRELOAD", None)
    child = subprocess.Popen(
        [client, core, str(descriptor), str(size), fault], env=environment,
        pass_fds=(descriptor,), stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, text=True,
    )
    os.close(descriptor)
    try:
        ready = select.select([child.stdout], [], [], 20)[0]
        if not ready or child.stdout.readline().strip() != "rejected":
            if child.poll() is None:
                child.kill()
            stdout, stderr = child.communicate(timeout=5)
            raise AssertionError(f"import cleanup client did not reject the import: {stdout}{stderr}")
        assert records(child.pid) == [], "failed import published allocation or handle metadata"
        stdout, stderr = child.communicate("x", timeout=20)
        assert child.returncode == 0, stdout + stderr
        cuda_driver.assert_bytes(address, contents, "creator after failed and successful imports")
    finally:
        if child.poll() is None:
            child.kill()
        child.wait(timeout=5)
    cuda_driver.destroy_mapped_allocation(address, size, handle)
    cuda_call(driver.cuDevicePrimaryCtxRelease, 0)


if __name__ == "__main__":
    run_creator(*sys.argv[1:])
