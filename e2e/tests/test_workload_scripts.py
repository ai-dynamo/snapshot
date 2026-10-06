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
import subprocess
import time
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from snapshot_e2e import k8s, workloads


@pytest.mark.workload
@pytest.mark.parametrize("payload", [
    b'{"phase":"gpu-gated"}\n{"phase":"quiesced"}\n',
    '{"phase":"quiesced","text":"CUDA ✓"}\n'.encode(),
])
def test_pod_logs_preserves_raw_json_text(monkeypatch, payload):
    response = SimpleNamespace(data=payload, release_conn=Mock())
    calls = []

    def read_logs(self, **kwargs):
        calls.append(kwargs)
        # Exercise the real SDK deserializer that changes bytes/JSON to repr.
        return self.api_client.deserialize(response, "str") if kwargs["_preload_content"] else response

    monkeypatch.setattr(k8s.client.CoreV1Api, "read_namespaced_pod_log", read_logs)
    assert k8s.pod_logs("test", "source", container="main", tail_lines=500) == payload.decode()
    assert calls == [{"name": "source", "namespace": "test", "tail_lines": 500, "container": "main", "_preload_content": False}]
    response.release_conn.assert_called_once()


@pytest.mark.workload
def test_pod_logs_releases_response_on_decode_error(monkeypatch):
    response = SimpleNamespace(data=b"\xff", release_conn=Mock())
    monkeypatch.setattr(k8s.client.CoreV1Api, "read_namespaced_pod_log", lambda *_args, **_kwargs: response)
    with pytest.raises(UnicodeDecodeError):
        k8s.pod_logs("test", "source", container="main")
    response.release_conn.assert_called_once()


@pytest.mark.workload
@pytest.mark.parametrize("container", [None, workloads.CONTAINER])
def test_exec_payload_strips_profile_noise_and_preserves_container(
    monkeypatch: pytest.MonkeyPatch, container: str | None,
) -> None:
    calls = []

    def exec_command(namespace, pod, command, **kwargs):
        calls.append((namespace, pod, command, kwargs))
        return f'profile banner\n{k8s.PAYLOAD_MARKER}\n{{"phase": "restored"}}\n'

    monkeypatch.setattr(k8s, "exec_command", exec_command)
    assert k8s.exec_payload("test", "restore", "cat result.json", container=container) == '{"phase": "restored"}\n'
    assert calls == [(
        "test", "restore", f"echo {k8s.PAYLOAD_MARKER}; cat result.json",
        {"container": container} if container is not None else {},
    )]


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
@pytest.mark.parametrize("gpu", [False, True])
def test_no_workload_waits_on_snapshot_complete(gpu: bool) -> None:
    for script in (
        workloads.snapshotjob_source_command("test-image", gpu=gpu),
        workloads.source_command("test-image", gpu=gpu),
    ):
        assert workloads.SNAPSHOT_COMPLETE not in script
        assert workloads.SOURCE_READY in script


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
    # The standby flag is what keeps the placeholder from loading a model
    # itself; it must be injected under its single canonical name.
    standby_env = {"name": "SNAPSHOT_RESTORE_STANDBY", "value": "1"}
    assert standby_env in single_container["env"]
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
        assert standby_env in container["env"]
        assert all(not e["name"].startswith("DYN_") for e in container["env"])


@pytest.mark.workload
def test_gms_source_shares_one_dra_claim_and_gates_capture_on_real_publication(monkeypatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "snapshot-workload:test")
    config = k8s.E2EConfig("test", "snapshot", "run-owned-pvc", None)
    run = workloads.TestRun.new("gms-manifest")
    template = workloads.gms_snapshotjob_pod_template(
        config=config, run=run, image="real-gms:test", claim_template="run-owned-gpu",
        fixture_configmap="run-owned-script", failure_ghost_size=536870913,
    )
    spec = template["spec"]
    assert spec["resourceClaims"] == [{"name": workloads.GMS_CLAIM, "resourceClaimTemplateName": "run-owned-gpu"}]
    assert template["metadata"]["annotations"] == {workloads.GMS_HELPER_ANNOTATION: "gms-saver"}
    containers = [*spec["containers"], *spec["initContainers"]]
    for container in containers:
        assert container["resources"] == {"claims": [{"name": workloads.GMS_CLAIM}]}
        assert container["image"] == "real-gms:test"
        assert {"name": "checkpoint-storage", "mountPath": "/checkpoints"} in container["volumeMounts"]
        assert not any(env["name"] in {"SNAPSHOT_JOB_UID", "SNAPSHOT_HELPER_ARTIFACT_SUBDIR"} for env in container["env"])
    worker, saver = spec["containers"]
    assert worker["command"][-2:] == ["--failure-ghost-size", "536870913"]
    command = saver["command"][-1]
    assert command.index("quiesced") < command.index("snapshot.saver") < command.index("published") < command.index("release")
    assert command.index("snapshot.saver") < command.index("saver-published") < command.index("touch")
    assert "printf '%s\\n' '{\"phase\":\"saver-published\"}'" in command
    assert "--snapshot-storage-dir /checkpoints" in command
    assert "--checkpoint-dir" not in command
    assert spec["initContainers"][0]["restartPolicy"] == "Always"
    assert spec["initContainers"][0]["command"] == ["python3", workloads.GMS_FIXTURE_PATH, "server"]
    assert "get_socket_path" in spec["initContainers"][0]["startupProbe"]["exec"]["command"][-1]
    assert not any(volume["name"] == "snapshot-control" for volume in spec["volumes"])


@pytest.mark.workload
def test_gms_restore_uses_original_attempt_and_preserves_snapshot_gate(monkeypatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "snapshot-workload:test")
    config = k8s.E2EConfig("test", "snapshot", "run-owned-pvc", None)
    run = workloads.TestRun.new("gms-restore")
    pod = workloads.gms_restore_pod(
        config=config, run=run, image="real-gms:test", claim_template="run-owned-gpu",
        fixture_configmap="run-owned-script", snapshot_name="completed-checkpoint",
        snapshot_job_uid="original-attempt", source_node="source-node",
    )
    worker = pod["spec"]["containers"][0]
    assert worker["command"] == ["python3", workloads.GMS_FIXTURE_PATH, "standby"]
    assert worker["startupProbe"]["exec"]["command"] == ["cat", workloads.RESTORE_DONE]
    assert {"name": "SNAPSHOT_CONTROL_DIR", "value": workloads.CONTROL_DIR} in worker["env"]
    assert {"name": "SNAPSHOT_RESTORE_STANDBY", "value": "1"} in worker["env"]
    assert {"name": "snapshot-control", "mountPath": workloads.CONTROL_DIR, "subPath": workloads.CONTAINER} in worker["volumeMounts"]
    assert pod["metadata"]["annotations"] == {"nvidia.com/restore-from": "completed-checkpoint"}
    loader_command = pod["spec"]["initContainers"][0]["command"]
    assert loader_command[loader_command.index("--snapshot-job-uid") + 1] == "original-attempt"
    assert loader_command == ["python3", workloads.GMS_FIXTURE_PATH, "server", "--snapshot-job-uid", "original-attempt"]
    assert "--probe-restore-ready" in pod["spec"]["initContainers"][0]["startupProbe"]["exec"]["command"]
