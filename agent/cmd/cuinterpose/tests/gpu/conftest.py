# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Physical-GPU fixtures using a matched, prebuilt artifact directory.

No fake CUDA provider or C-stack build is used here. GPU prerequisites skip when
unavailable; a configured artifact directory with missing binaries is an error.
"""

from __future__ import annotations

import os
import random
from pathlib import Path

import pytest


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers", "gpu: needs two GPUs, a CUDA 13 driver, and cuda-checkpoint --launch-job"
    )
    config.addinivalue_line(
        "markers", "multicast: additionally needs NVLink between the two GPUs"
    )


@pytest.fixture(scope="session")
def tools():
    """Use CUINTERPOSE_BUILD_DIR or the sibling build directory."""
    from harness import Tools

    build_dir = Path(os.environ.get("CUINTERPOSE_BUILD_DIR",
                                  Path(__file__).resolve().parents[2] / "build"))
    for name in ("libcuinterpose.so", "libcuinterpose_core.so", "cuinterpose-coordinator"):
        if not (build_dir / name).is_file():
            pytest.fail(f"missing packaged artifact: {build_dir / name}")
    interposer = (build_dir / "libcuinterpose.so").resolve()
    coordinator = (build_dir / "cuinterpose-coordinator").resolve()
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
    import cuda_driver
    import harness
    from cuda.bindings import driver

    cuda_driver.cuda_call(driver.cuInit, 0)
    for ordinal in range(harness.WORLD_SIZE):
        device = cuda_driver.cuda_call(driver.cuDeviceGet, ordinal)
        supported = cuda_driver.cuda_call(
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
