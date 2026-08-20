# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fast, cluster-free checks of the workload scripts themselves.

The SnapshotJob source establishes restorable state and declares readiness.
It does not own a post-capture handshake: checkpoint may terminate it, and the
SnapshotJob controller deletes the owned source Job after capture. These checks
therefore cover only the pre-capture source contract.
"""

from __future__ import annotations

import os
import re
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

from snapshot_e2e import workloads


def render_cpu_source(control_dir: Path, state_dir: Path) -> str:
    """Rewrite pod-absolute paths to temp dirs, leaving logic untouched."""
    script = workloads.snapshotjob_source_command("test-image", gpu=False)
    return script.replace(workloads.CONTROL_DIR, str(control_dir)).replace(
        workloads.STATE_DIR, str(state_dir)
    )


@pytest.mark.workload
def test_snapshotjob_cpu_source_establishes_state_before_capture(tmp_path: Path) -> None:
    control_dir = tmp_path / "snapshot-control"
    state_dir = tmp_path / "e2e-state"
    control_dir.mkdir()

    token = "unit-source-token"
    env = {**os.environ, workloads.SOURCE_TOKEN_ENV: token}
    proc = subprocess.Popen(
        ["bash", "-c", render_cpu_source(control_dir, state_dir)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        ready = control_dir / "ready-for-snapshot"
        observations = state_dir / "observations.log"
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if ready.exists() and observations.exists():
                break
            time.sleep(0.05)

        assert ready.exists(), "workload never signalled ready-for-snapshot"
        assert observations.exists(), "workload wrote no pre-capture observation"
        text = observations.read_text()
        assert "observation seq=" in text
        assert f"cpu={token}" in text
        assert f"file={token}" in text
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.wait(timeout=10)


@pytest.mark.workload
@pytest.mark.parametrize("gpu", [False, True])
def test_snapshotjob_source_has_no_post_capture_handshake(gpu: bool) -> None:
    script = workloads.snapshotjob_source_command("test-image", gpu=gpu)

    assert "snapshot-complete" not in script
    expected_source = workloads.CUDA_SOURCE if gpu else workloads.CPU_SOURCE
    assert expected_source in script


def cuda_c_source(script: str) -> str:
    """Extract the C program the CUDA workload heredocs into a file."""
    body = script.split("<<'C_EOF'\n", 1)[1]
    return body.split("\nC_EOF", 1)[0]


@pytest.mark.workload
def test_snapshotjob_cuda_source_compiles() -> None:
    """Compile the CUDA source without running it or requiring a GPU."""
    source = cuda_c_source(workloads.CUDA_SOURCE)
    assert "int main(void)" in source

    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "cuda_hold.c"
        src.write_text(source)
        try:
            result = subprocess.run(
                ["cc", "-c", str(src), "-o", str(Path(tmp) / "cuda_hold.o")],
                capture_output=True,
                text=True,
            )
        except FileNotFoundError:
            pytest.skip("no C compiler available")
    assert result.returncode == 0, f"CUDA workload does not compile:\n{result.stderr}"


@pytest.mark.workload
def test_workload_paths_match_the_operator_contract() -> None:
    """Guard the readiness and restore handshakes against silent path drift."""
    constants = Path(__file__).resolve().parents[2] / "api" / "v1alpha1" / "constants.go"
    text = constants.read_text()

    def go_const(name: str) -> str:
        match = re.search(rf'^\s*{name}\s*=\s*"([^"]*)"', text, re.MULTILINE)
        assert match, f"{name} not found in {constants} - constant renamed or removed?"
        return match.group(1)

    assert workloads.CONTROL_DIR == go_const("SnapshotControlMountPath")
    assert workloads.SOURCE_READY == (
        f"{go_const('SnapshotControlMountPath')}/{go_const('ReadyForSnapshotFile')}"
    )
    assert workloads.RESTORE_DONE == (
        f"{go_const('SnapshotControlMountPath')}/{go_const('RestoreCompleteFile')}"
    )
