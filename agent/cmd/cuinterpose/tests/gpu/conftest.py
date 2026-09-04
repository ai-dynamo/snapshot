# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fixtures for the GPU tests.

Everything here skips rather than fails when the machine cannot run the tests:
no torch or cuda-bindings, fewer than two GPUs, not started through
``cuda-checkpoint --launch-job``, or no CUDA headers to build the shim with.
Set ``CUINTERPOSE_BUILD_DIR`` to a directory holding ``libcuinterpose.so`` and
``cuinterpose-coordinator`` to use prebuilt binaries (for example copied out of
the agent image) instead of building them here.
"""

from __future__ import annotations

import os
import random
import shutil
import subprocess
from pathlib import Path

import pytest

BUILD_TIMEOUT_SECONDS = 300


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers", "gpu: needs two GPUs, a CUDA 13 driver, and cuda-checkpoint --launch-job"
    )
    config.addinivalue_line(
        "markers", "multicast: additionally needs NVLink between the two GPUs"
    )


@pytest.fixture(scope="session")
def tools(tmp_path_factory: pytest.TempPathFactory):
    """The shim and coordinator under test, prebuilt or built once per session."""
    from harness import Tools

    prebuilt = os.environ.get("CUINTERPOSE_BUILD_DIR")
    if prebuilt:
        build_dir = Path(prebuilt)
    else:
        source_dir = Path(__file__).resolve().parents[2]
        cuda_home = Path(os.environ.get("CUDA_HOME", "/usr/local/cuda"))
        compiler = os.environ.get("CC", "cc")
        missing = [
            requirement
            for requirement, present in (
                ("make on PATH", shutil.which("make") is not None),
                ("readelf on PATH", shutil.which("readelf") is not None),
                (f"{compiler} on PATH", shutil.which(compiler) is not None),
                (f"{cuda_home}/include/cuda.h", (cuda_home / "include" / "cuda.h").is_file()),
            )
            if not present
        ]
        if missing:
            pytest.skip(
                "cannot build the shim: missing "
                + ", ".join(missing)
                + "; set CUDA_HOME, or CUINTERPOSE_BUILD_DIR to prebuilt binaries"
            )
        build_dir = tmp_path_factory.mktemp("cuinterpose-build")
        result = subprocess.run(
            ["make", "-C", str(source_dir), f"BUILD_DIR={build_dir}", f"CUDA_HOME={cuda_home}", "all"],
            check=False,
            capture_output=True,
            text=True,
            timeout=BUILD_TIMEOUT_SECONDS,
        )
        if result.returncode != 0:
            pytest.fail(f"shim build failed ({result.returncode}):\n{result.stdout}{result.stderr}")
    interposer = (build_dir / "libcuinterpose.so").resolve()
    coordinator = (build_dir / "cuinterpose-coordinator").resolve()
    if not interposer.is_file() or not coordinator.is_file():
        pytest.fail(f"{build_dir} does not hold libcuinterpose.so and cuinterpose-coordinator")
    return Tools(interposer, coordinator)


@pytest.fixture(scope="session")
def gpu_environment(tools):
    pytest.importorskip("torch")
    pytest.importorskip("cuda.bindings")
    import harness

    launch_job = harness.launch_job_fds()
    if launch_job is None:
        pytest.skip("run through cuda-checkpoint --launch-job (CUDA_CHECKPOINT_JOB_FILE is unset)")
    gpus = harness.visible_gpus()
    if gpus is None:
        pytest.skip(f"needs {harness.WORLD_SIZE} distinct GPUs (CUDA_VISIBLE_DEVICES)")
    return harness.Environment(tools, gpus, launch_job)


@pytest.fixture(scope="session")
def multicast_supported(gpu_environment):
    """Skips unless both GPUs report multicast support (NVLink / NVSwitch)."""
    import harness
    from cuda.bindings import driver

    harness.cuda_call(driver.cuInit, 0)
    for ordinal in range(harness.WORLD_SIZE):
        device = harness.cuda_call(driver.cuDeviceGet, ordinal)
        supported = harness.cuda_call(
            driver.cuDeviceGetAttribute,
            driver.CUdevice_attribute.CU_DEVICE_ATTRIBUTE_MULTICAST_SUPPORTED,
            device,
        )
        if not int(supported):
            pytest.skip(f"GPU {ordinal} does not support CUDA multicast")
    return True


@pytest.fixture
def seed(record_property) -> int:
    """Seed for the random buffer contents; set CUINTERPOSE_TEST_SEED to replay."""
    value = int(os.environ.get("CUINTERPOSE_TEST_SEED") or random.getrandbits(32))
    record_property("cuinterpose_test_seed", value)
    print(f"\ncuinterpose test seed: {value} (CUINTERPOSE_TEST_SEED={value} to replay)")
    return value
