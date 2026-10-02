# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Physical-GPU fixtures using a matched, prebuilt Rust artifact directory.

The suite requires two real GPUs and a CUDA 13 driver. Missing dependencies,
artifacts, or hardware fail the run; they never turn qualification into a skip.
"""

from __future__ import annotations

import os
import random
from pathlib import Path

import pytest


def pytest_configure(config: pytest.Config) -> None:
    config.addinivalue_line(
        "markers", "gpu: requires two CUDA GPUs and a CUDA 13 driver"
    )
    config.addinivalue_line(
        "markers", "multicast: additionally needs NVLink between the two GPUs"
    )
    config.addinivalue_line(
        "markers", "host_numa: requires POSIX-shareable HOST_NUMA VMM"
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


@pytest.fixture(scope="session", autouse=True)
def gpu_environment(tools):
    import harness
    from cuda.bindings import driver
    from cuda_driver import cuda_call

    cuda_call(driver.cuInit, 0)
    assert cuda_call(driver.cuDriverGetVersion) >= 13000, "requires a CUDA 13 driver"
    gpus = harness.visible_gpus()
    if gpus is None:
        pytest.fail(f"requires {harness.WORLD_SIZE} distinct real GPUs (CUDA_VISIBLE_DEVICES)")
    return harness.Environment(tools, gpus)


@pytest.fixture(scope="session")
def multicast_supported(gpu_environment):
    """A selected multicast test requires capable GPUs and NVLink / NVSwitch."""
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
            pytest.fail(f"GPU {ordinal} does not support CUDA multicast; select a capable host")
    return True


@pytest.fixture
def seed(record_property) -> int:
    """Seed for the random buffer contents; set CUINTERPOSE_TEST_SEED to replay."""
    value = int(os.environ.get("CUINTERPOSE_TEST_SEED") or random.getrandbits(32))
    record_property("cuinterpose_test_seed", value)
    print(f"\ncuinterpose test seed: {value} (CUINTERPOSE_TEST_SEED={value} to replay)")
    return value
