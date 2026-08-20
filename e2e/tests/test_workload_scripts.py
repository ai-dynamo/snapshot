# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fast, cluster-free checks of the workload scripts themselves.

These run the same shell text the pod runs, in a temp dir, in seconds — no
cluster, no images, no vCluster. They exist because the SnapshotJob completion
gate depends on a handshake no Go unit test can cover: the agent writes
`snapshot-complete` into the control volume, the workload must notice it and
exit 0, and only then does the source Job reach Complete=True. A reconciler
unit test hand-writes `Job.Complete=True` against a fake client, so it proves
nothing about whether a real workload ever terminates.

Scope: the CPU script only. The CUDA variant compiles and links against
libcuda at runtime, so it cannot run here — its sentinel loop is structurally
identical and reviewed alongside this one.
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
    """Rewrites the pod-absolute paths to temp dirs, leaving logic untouched."""
    script = workloads.snapshotjob_source_command("test-image", gpu=False)
    return script.replace(workloads.CONTROL_DIR, str(control_dir)).replace(
        workloads.STATE_DIR, str(state_dir)
    )


@pytest.mark.workload
def test_snapshotjob_cpu_source_exits_zero_once_sentinel_appears(tmp_path: Path) -> None:
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
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and not ready.exists():
            time.sleep(0.05)
        assert ready.exists(), "workload never signalled ready-for-snapshot"

        observations = state_dir / "observations.log"
        # The capture-side assertion needs at least one pre-dump observation
        # (minimum=1); anything higher is not reliably achievable because the
        # dump starts well under a second after readiness.
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline and not observations.exists():
            time.sleep(0.05)
        assert observations.exists(), "workload wrote no observations before the sentinel"
        before = observations.read_text().count("observation seq=")
        assert before >= 1

        assert proc.poll() is None, "workload exited before the sentinel was written"

        # The agent's post-dump handshake.
        (control_dir / "snapshot-complete").touch()

        # 1s poll interval, so this must be prompt. A workload that never
        # exits leaves the Job incomplete and the SnapshotJob stuck at
        # Completed=False/WaitingForPodCompletion until its deadline.
        exit_code = proc.wait(timeout=15)
        assert exit_code == 0, f"expected clean exit, got {exit_code}"

        after = observations.read_text()
        assert after.count("observation seq=") > before, (
            "workload must record a final observation after the sentinel"
        )
        assert f"cpu={token}" in after
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)


def cuda_c_source(script: str) -> str:
    """Extracts the C program the CUDA workload heredocs into a file."""
    body = script.split("<<'C_EOF'\n", 1)[1]
    return body.split("\nC_EOF", 1)[0]


@pytest.mark.workload
def test_snapshotjob_cuda_source_has_the_sentinel_exit_contract() -> None:
    """Structural parity with the CPU variant, which we can actually run.

    The GPU test is the one currently hanging at Completed=False, and this
    script cannot be executed here (it dlopens libcuda.so.1), so asserting the
    contract structurally is the closest cheap check available: poll the
    sentinel, leave the loop, exit 0.
    """
    snapshotjob = workloads.SNAPSHOTJOB_CUDA_SOURCE
    assert workloads.SNAPSHOT_COMPLETE in snapshotjob
    assert f'while (access("{workloads.SNAPSHOT_COMPLETE}", F_OK) != 0)' in snapshotjob
    assert "return 0;" in snapshotjob.split("F_OK) != 0)", 1)[1], (
        "the CUDA loop must exit 0 after the sentinel, or the Job never completes"
    )
    assert "sleep(1);" in snapshotjob, "poll interval must match the CPU variant"

    # The plain (non-SnapshotJob) variant must keep looping forever: it is
    # driven by PodSnapshot directly and has no Job completion to satisfy.
    assert workloads.SNAPSHOT_COMPLETE not in workloads.CUDA_SOURCE
    assert "while (1)" in workloads.CUDA_SOURCE


@pytest.mark.workload
def test_snapshotjob_cuda_source_compiles() -> None:
    """Compiles the CUDA workload's C program without running it.

    CUDA symbols are resolved at runtime via dlopen, so compilation needs no
    GPU and no CUDA toolkit — but a syntax error here would otherwise only
    surface as a crash-looping pod inside a 20-minute GPU e2e run.
    """
    source = cuda_c_source(workloads.SNAPSHOTJOB_CUDA_SOURCE)
    assert "int main(void)" in source

    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "cuda_hold.c"
        src.write_text(source)
        try:
            result = subprocess.run(
                ["cc", str(src), "-o", str(Path(tmp) / "cuda_hold")],
                capture_output=True,
                text=True,
            )
        except FileNotFoundError:
            pytest.skip("no C compiler available")
    assert result.returncode == 0, f"CUDA workload does not compile:\n{result.stderr}"


@pytest.mark.workload
def test_workload_paths_match_the_operator_contract() -> None:
    """Guards the operator/workload handshake against silent drift.

    The workload polls paths it hardcodes; the operator mounts the control
    volume and the agent writes the sentinel using the Go constants. If either
    side moves, nothing fails loudly — the workload simply waits forever and
    the SnapshotJob hangs at Completed=False until its deadline. Parsing the
    Go constants keeps that a one-second failure instead of a 20-minute one.
    """
    constants = Path(__file__).resolve().parents[2] / "api" / "v1alpha1" / "constants.go"
    text = constants.read_text()

    def go_const(name: str) -> str:
        match = re.search(rf'^\s*{name}\s*=\s*"([^"]*)"', text, re.MULTILINE)
        assert match, f"{name} not found in {constants} — constant renamed or removed?"
        return match.group(1)

    assert workloads.CONTROL_DIR == go_const("SnapshotControlMountPath")
    assert workloads.SNAPSHOT_COMPLETE == (
        f"{go_const('SnapshotControlMountPath')}/{go_const('SnapshotCompleteFile')}"
    )
    assert workloads.SOURCE_READY == (
        f"{go_const('SnapshotControlMountPath')}/{go_const('ReadyForSnapshotFile')}"
    )


@pytest.mark.workload
def test_snapshotjob_cpu_source_keeps_running_without_sentinel(tmp_path: Path) -> None:
    """The other half of the contract: it must not exit on its own.

    An early exit would let the Job complete before the dump, so the
    SnapshotJob could reach Completed=True having captured nothing.
    """
    control_dir = tmp_path / "snapshot-control"
    state_dir = tmp_path / "e2e-state"
    control_dir.mkdir()

    env = {**os.environ, workloads.SOURCE_TOKEN_ENV: "unit-source-token"}
    proc = subprocess.Popen(
        ["bash", "-c", render_cpu_source(control_dir, state_dir)],
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        with pytest.raises(subprocess.TimeoutExpired):
            proc.wait(timeout=5)
    finally:
        proc.kill()
        proc.wait(timeout=10)
