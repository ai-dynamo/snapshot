# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint and restore of POSIX-shared CUDA memory on real GPUs.

Run through the launch-job wrapper so the workers are checkpointable:

    cuda-checkpoint --launch-job uv run --project agent/cmd/cuinterpose/tests/gpu \\
        pytest agent/cmd/cuinterpose/tests/gpu -v -m gpu

Knobs: ``CUINTERPOSE_TEST_CARRIER_MIB`` (size of the large per-rank allocation,
default 256), ``CUINTERPOSE_TEST_MIN_H2D_FRACTION`` (how close the host carrier
restore must come to a plain pinned copy, default 0.8), ``CUINTERPOSE_TEST_SEED``.
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
MIN_H2D_FRACTION = float(os.environ.get("CUINTERPOSE_TEST_MIN_H2D_FRACTION", "0.8"))


@pytest.fixture(scope="session")
def h2d_baseline_gbps(gpu_environment) -> float:
    """A plain pinned host-to-device copy of one rank's large allocation on the
    first GPU: the speed the carrier restore is held to."""
    return cuda_driver.measure_h2d_gbps(0, CARRIER_MIB << 20)


@pytest.mark.gpu
def test_checkpoint_restores_shared_posix_memory(
    gpu_environment, tmp_path, seed, h2d_baseline_gbps
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

        # Only actually shared creators use host carriers. The worker also
        # checks a never-exported VMM allocation left to native CUDA.
        expected_count, expected_bytes = workload.worker_carriers()
        saved = prepare.phases["save_allocations"]
        assert int(saved["allocation_count"]) >= expected_count, prepare.out
        assert int(saved["allocation_bytes"]) >= expected_bytes, prepare.out

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
        restored = restore.phases["load_allocations"]
        assert restored["allocation_count"] == saved["allocation_count"], restore.out
        assert restored["allocation_bytes"] == saved["allocation_bytes"], restore.out
        # The phase figure includes creating and mapping the fresh device memory
        # and the socket round trip; the copy figure is the transfer alone, and
        # that is what must run at the link's speed.
        phase = float(restored["gb_per_s"])
        copied = float(restored["copy_gb_per_s"])
        print(
            f"\nhost carrier restore: copies {copied:.2f} GB/s, whole phase {phase:.2f} GB/s over "
            f"{restored['allocation_bytes']} bytes; pinned copy baseline {h2d_baseline_gbps:.2f} GB/s"
        )
        assert copied >= MIN_H2D_FRACTION * h2d_baseline_gbps, (
            f"host carrier copies reached {copied:.2f} GB/s, below "
            f"{MIN_H2D_FRACTION:.0%} of the {h2d_baseline_gbps:.2f} GB/s pinned copy baseline"
        )

        workload.hand_fresh_imports()
        (workload.sync_dir / "continue").touch()
        workload.finish()


@pytest.mark.gpu
def test_prepare_is_refused_while_a_raw_import_is_alive(gpu_environment, tmp_path, seed) -> None:
    """An import of a descriptor from a process without the shim cannot be
    repaired after restore, so the coordinator refuses to checkpoint while one
    is alive, and the workload keeps running untouched."""
    with Workload(
        tmp_path,
        gpu_environment,
        mode="unicast",
        carrier_bytes=1 << 20,
        seed=seed,
        hold_raw_import=True,
    ) as workload:
        workload.start()

        prepare = harness.run_coordinator(
            workload.environment.tools.coordinator,
            "--prepare",
            workload.checkpoint_dir,
            workload.control_dir,
            workload.child_pids,
        )
        assert prepare.status != 0, "prepare must be refused while a raw import is alive"
        assert "live raw imports" in prepare.err, prepare.err
        assert not (workload.checkpoint_dir / harness.STATE_FILENAME).exists()

        (workload.sync_dir / "continue").touch()
        workload.finish()
