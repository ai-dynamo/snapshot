# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint and restore of POSIX-shared CUDA memory on real GPUs.

Run with matched shim and coordinator artifacts:

    uv run --project agent/cmd/cuinterpose/tests/gpu \\
        pytest agent/cmd/cuinterpose/tests/gpu -v -m gpu

Knobs: ``CUINTERPOSE_TEST_CARRIER_MIB`` (size of the large per-rank allocation,
default 256), ``CUINTERPOSE_TEST_SEED``.
"""

from __future__ import annotations

import os

import pytest

pytest.importorskip("torch")
pytest.importorskip("cuda.bindings")

import cuda_driver  # noqa: E402
import harness  # noqa: E402
from harness import Workload  # noqa: E402

CARRIER_MIB = int(os.environ.get("CUINTERPOSE_TEST_CARRIER_MIB", "256"))
@pytest.mark.gpu
def test_checkpoint_restores_shared_posix_memory(
    gpu_environment, tmp_path, seed
) -> None:
    with Workload(
        tmp_path, gpu_environment, mode="unicast", carrier_bytes=CARRIER_MIB << 20, seed=seed
    ) as workload:
        workload.start()

        prepare = harness.run_coordinator(
            workload.environment.tools.coordinator,
            "--prepare",
            workload.checkpoint_dir,
            workload.control_dir,
            workload.child_pids,
        )
        assert prepare.status == 0, (
            f"coordinator exited {prepare.status}\nstdout:\n{prepare.out}\nstderr:\n{prepare.err}"
        )
        state = workload.checkpoint_dir / harness.STATE_FILENAME
        assert state.is_file() and state.stat().st_size > 0, "coordinator wrote no state file"
        stray = [path.name for path in workload.checkpoint_dir.iterdir() if path != state]
        assert not stray, f"host carriers must stay in process memory, found {stray}"

        cuda_driver.native_checkpoint(
            workload.child_pids,
            command_timeout_seconds=harness.COMMAND_TIMEOUT_SECONDS,
            checkpoint_timeout_seconds=harness.CHECKPOINT_TIMEOUT_SECONDS,
        )

        restore = harness.run_coordinator(
            workload.environment.tools.coordinator,
            "--restore",
            workload.checkpoint_dir,
            workload.control_dir,
            workload.child_pids,
        )
        assert restore.status == 0, (
            f"coordinator exited {restore.status}\nstdout:\n{restore.out}\nstderr:\n{restore.err}"
        )
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
