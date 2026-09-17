# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint and restore of a CUDA multicast group on real GPUs.

Same flow as test_posix.py, but the two workers share their symmetric-memory
buffer through a multicast object (PyTorch's multimem all-reduce), and one rank
rebinds its slice with cuMulticastBindAddr so both bind entry points are
exercised. Needs NVLink between the two GPUs.
"""

from __future__ import annotations

import pytest

pytest.importorskip("torch")
pytest.importorskip("cuda.bindings")

import cuda_driver  # noqa: E402
import harness  # noqa: E402
from harness import Workload  # noqa: E402

from test_posix import CARRIER_MIB  # noqa: E402


@pytest.mark.gpu
@pytest.mark.multicast
def test_checkpoint_restores_multicast_group(gpu_environment, multicast_supported, tmp_path, seed) -> None:
    with Workload(
        tmp_path, gpu_environment, mode="multicast", carrier_bytes=CARRIER_MIB << 20, seed=seed
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
        assert (workload.checkpoint_dir / harness.STATE_FILENAME).is_file()
        expected_count, expected_bytes = workload.worker_carriers()
        saved = prepare.phases["save_allocations"]
        assert int(saved["allocation_count"]) >= expected_count, prepare.out
        assert int(saved["allocation_bytes"]) >= expected_bytes, prepare.out
        # The multicast phases ran on both sides.
        assert prepare.phases["prepare_multicast"]["status"] == "ok", prepare.out

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
        assert restore.phases["restore_multicast"]["status"] == "ok", restore.out

        workload.hand_fresh_imports()
        (workload.sync_dir / "continue").touch()
        workload.finish()
