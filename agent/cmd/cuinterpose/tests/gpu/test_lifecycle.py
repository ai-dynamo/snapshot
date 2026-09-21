# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint/restore contents and collectives on two real GPUs."""

import os

import pytest

pytest.importorskip("torch")
pytest.importorskip("cuda.bindings")

import cuda_driver  # noqa: E402
import harness  # noqa: E402
from harness import Workload  # noqa: E402

CARRIER_MIB = int(os.environ.get("CUINTERPOSE_TEST_CARRIER_MIB", "256"))


@pytest.mark.gpu
@pytest.mark.parametrize("mode", ["unicast", pytest.param("multicast", marks=pytest.mark.multicast)])
def test_checkpoint_restores_shared_memory(mode, gpu_environment, tmp_path, seed, request):
    if mode == "multicast":
        request.getfixturevalue("multicast_supported")
    with Workload(tmp_path, gpu_environment, mode=mode,
                  carrier_bytes=CARRIER_MIB << 20, seed=seed) as workload:
        workload.start()
        workload.coordinate("--prepare")
        state = workload.checkpoint_dir / harness.STATE_FILENAME
        assert state.is_file() and state.stat().st_size > 0
        assert list(workload.checkpoint_dir.iterdir()) == [state], "host carriers must stay in memory"
        cuda_driver.native_checkpoint(
            workload.child_pids,
            command_timeout_seconds=harness.COMMAND_TIMEOUT_SECONDS,
            checkpoint_timeout_seconds=harness.CHECKPOINT_TIMEOUT_SECONDS,
        )
        workload.coordinate("--restore")
        workload.hand_fresh_imports()
        (workload.sync_dir / "continue").touch()
        workload.finish()


@pytest.mark.gpu
def test_foreign_import_is_rejected_before_checkpoint(gpu_environment, tmp_path, seed) -> None:
    """Workers reject foreign descriptors and continue with supported allocations."""
    with Workload(
        tmp_path,
        gpu_environment,
        mode="unicast",
        carrier_bytes=1 << 20,
        seed=seed,
        admission_only=True,
    ) as workload:
        workload.start()

        assert not (workload.checkpoint_dir / harness.STATE_FILENAME).exists()

        (workload.sync_dir / "continue").touch()
        workload.finish()
