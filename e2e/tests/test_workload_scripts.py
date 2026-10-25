# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fast, cluster-free checks of the workload scripts themselves.

The capture terminates the source process (there is no leave-running mode and
no snapshot-complete release sentinel), so the SnapshotJob source is a plain
state loop with two contractual properties this file pins down:

1. Observation seq=0 is written BEFORE ready-for-snapshot. The dump starts on
   readiness and kills the process, so "at least one pre-capture observation
   exists" must be a workload ordering guarantee — cluster tests cannot poll
   for it against a pod that dies with the dump.
2. Nothing waits on snapshot-complete. A stray sentinel write must not stop
   the workload: termination is the agent's job (via the dump), not a file
   protocol.
"""

from __future__ import annotations

import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import workloads


def render_cpu_source(control_dir: Path, state_dir: Path) -> str:
    """Rewrite pod-absolute paths to temp dirs, leaving logic untouched."""
    script = workloads.snapshotjob_source_command("test-image", gpu=False)
    return script.replace(workloads.CONTROL_DIR, str(control_dir)).replace(
        workloads.STATE_DIR, str(state_dir)
    )


@pytest.mark.workload
def test_snapshotjob_cpu_source_observes_before_ready_and_ignores_sentinel(
    tmp_path: Path,
) -> None:
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
            if ready.exists():
                break
            time.sleep(0.05)

        assert ready.exists(), "workload never signalled ready-for-snapshot"
        # The ordering contract: by the time readiness is observable, the
        # first observation must already be durable in the state dir.
        assert observations.exists(), "ready was signalled before any observation"
        text = observations.read_text()
        assert "observation seq=0" in text
        assert f"cpu={token}" in text
        assert f"file={token}" in text

        # The legacy release sentinel is dead protocol: writing it must not
        # release or stop anything. The dump (SIGKILL) is the only exit path.
        (control_dir / "snapshot-complete").write_text("complete\n")
        time.sleep(2)
        assert proc.poll() is None, "workload exited on the legacy sentinel"
    finally:
        if proc.poll() is None:
            proc.kill()
        proc.wait(timeout=10)


@pytest.mark.workload
def test_cpu_source_logs_and_exits_on_request(tmp_path: Path) -> None:
    # The restored-workload e2e test reads these lines from the restore Pod's
    # log, and ends the workload through the exit request.
    control_dir = tmp_path / "snapshot-control"
    state_dir = tmp_path / "e2e-state"
    control_dir.mkdir()
    proc = subprocess.Popen(
        ["bash", "-c", render_cpu_source(control_dir, state_dir)],
        env={**os.environ, workloads.SOURCE_TOKEN_ENV: "unit-source-token"},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        deadline = time.monotonic() + 10
        while not (control_dir / "ready-for-snapshot").exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        (state_dir / "exit-request").write_text("7\n")
        output, _ = proc.communicate(timeout=15)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)

    assert proc.returncode == 7
    assert "[source] observation seq=0" in output


@pytest.mark.workload
def test_cpu_source_exits_with_its_code_on_sigterm_when_asked(tmp_path: Path) -> None:
    # The pod deletion e2e test checks this code: the restored workload
    # handled the SIGTERM the restore proxy forwarded.
    control_dir = tmp_path / "snapshot-control"
    state_dir = tmp_path / "e2e-state"
    control_dir.mkdir()
    script = workloads.source_command("test-image", gpu=False, handle_sigterm=True)
    script = script.replace(workloads.CONTROL_DIR, str(control_dir)).replace(
        workloads.STATE_DIR, str(state_dir)
    )
    proc = subprocess.Popen(
        ["bash", "-c", script],
        env={**os.environ, workloads.SOURCE_TOKEN_ENV: "unit-source-token"},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        deadline = time.monotonic() + 10
        while not (control_dir / "ready-for-snapshot").exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        proc.send_signal(signal.SIGTERM)
        # bash runs the trap once the loop's sleep returns.
        proc.communicate(timeout=15)
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)

    assert proc.returncode == workloads.SIGTERM_EXIT


@pytest.mark.workload
def test_only_an_opted_in_cpu_source_handles_sigterm() -> None:
    assert "trap" not in workloads.source_command("test-image", gpu=False)
    assert "trap" not in workloads.snapshotjob_source_command("test-image", gpu=False)
    with pytest.raises(ValueError):
        workloads.source_command("test-image", gpu=True, handle_sigterm=True)


@pytest.mark.workload
@pytest.mark.parametrize("gpu", [False, True])
def test_no_workload_waits_on_snapshot_complete(gpu: bool) -> None:
    for script in (
        workloads.snapshotjob_source_command("test-image", gpu=gpu),
        workloads.source_command("test-image", gpu=gpu),
    ):
        assert workloads.SNAPSHOT_COMPLETE not in script
        assert workloads.SOURCE_READY in script


@pytest.mark.workload
def test_snapshotjob_helper_waits_before_signalling_capture_readiness(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "snapshot-workload:test")
    config = k8s.E2EConfig(
        namespace="snapshot-e2e",
        release="snapshot",
        pvc_name="snapshot-pvc",
        kubeconfig=None,
    )
    template = workloads.snapshotjob_helper_pod_template(
        config=config,
        run=workloads.TestRun.new("helper-ready"),
        helper_command="true",
        wait_for_helper_before_capture=True,
    )

    command = template["spec"]["containers"][0]["command"][2]
    target = template["spec"]["containers"][0]
    helper_wait = f"while [ ! -f {workloads.HELPER_SYNC_DIR}/helper-finished ]; do sleep 0.1; done"
    capture_ready = f"echo ready > {workloads.SOURCE_READY}"
    assert command.index(helper_wait) < command.index(capture_ready)
    assert target["readinessProbe"]["exec"]["command"] == [
        "cat",
        f"{workloads.HELPER_SYNC_DIR}/helper-finished",
    ]

    release_gated_template = workloads.snapshotjob_helper_pod_template(
        config=config,
        run=workloads.TestRun.new("helper-release-gated"),
        helper_command="true",
        wait_for_helper_before_capture=False,
        wait_for_capture_release=True,
    )
    release_gated_target = release_gated_template["spec"]["containers"][0]
    release_command = release_gated_target["command"][2]
    capture_release = (
        f"while [ ! -f {workloads.HELPER_SYNC_DIR}/capture-release ]; do sleep 0.1; done"
    )
    assert release_command.index(capture_release) < release_command.index(capture_ready)
    assert "readinessProbe" not in release_gated_target


@pytest.mark.workload
def test_snapshotjob_exit_template_never_signals_ready() -> None:
    # The exit templates drive the died-before-capture failure classes; they
    # must terminate without touching the quiesce protocol at all.
    for exit_code in (0, 1):
        proc = subprocess.run(
            [
                "bash",
                "-c",
                "set -euo pipefail\n"
                f'echo "[snapshotjob-exit] exiting with {exit_code} before capture"\n'
                f"exit {exit_code}\n",
            ],
            capture_output=True,
            text=True,
            timeout=10,
        )
        assert proc.returncode == exit_code


@pytest.mark.workload
def test_restore_manifests_use_canonical_control_mount_and_startup_gate(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "snapshot-workload:test")
    config = k8s.E2EConfig(
        namespace="snapshot-e2e",
        release="snapshot",
        pvc_name="snapshot-pvc",
        kubeconfig=None,
    )
    run = workloads.TestRun.new("manifest")

    single = workloads.restore_pod(config=config, run=run, gpu=False)
    single_container = single["spec"]["containers"][0]
    assert single_container["volumeMounts"][0] == {
        "name": "snapshot-control",
        "mountPath": workloads.CONTROL_DIR,
        "subPath": workloads.CONTAINER,
    }
    assert single_container["startupProbe"]["exec"]["command"] == [
        "cat",
        workloads.RESTORE_DONE,
    ]
    # The restore proxy replaces the command, so nothing needs the old
    # standby flag, under either name.
    assert all("RESTORE_STANDBY" not in e["name"] for e in single_container["env"])
    assert all(not e["name"].startswith("DYN_") for e in single_container["env"])

    multi, _ = workloads.multi_restore_pod(
        config=config,
        run=run,
        source_node="source-node",
    )
    for container in multi["spec"]["containers"]:
        assert container["volumeMounts"][0]["subPath"] == container["name"]
        assert container["startupProbe"]["exec"]["command"] == [
            "cat",
            workloads.RESTORE_DONE,
        ]
        assert all("RESTORE_STANDBY" not in e["name"] for e in container["env"])
        assert all(not e["name"].startswith("DYN_") for e in container["env"])
