# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""SnapshotJob lifecycle e2e: capture+restore and DeadlineExceeded.

Unlike test_snapshot_lifecycle.py (which drives PodSnapshot directly against a
plain pod the test creates and annotates itself), these tests exercise the
SnapshotJob CRD end to end: the controller creates the source batch/v1 Job,
derives Running from it, creates the PodSnapshot once the pod exists, derives
Captured from it, marks Completed once capture succeeds, and deletes the source
Job. The test intentionally does not require the source pod to execute or the
batch Job to complete naturally after capture: checkpoint is allowed to
terminate the source process.
"""

from __future__ import annotations

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import workloads


@pytest.fixture
def run(request: pytest.FixtureRequest, config: k8s.E2EConfig) -> snap.TestRun:
    # Overrides the conftest.py `run` fixture for this module: SnapshotJob
    # cleanup is shaped differently (delete the SnapshotJob, not a bare
    # PodSnapshot by run.snapshot_name — see cleanup_snapshotjob's docstring).
    value = snap.TestRun.new(request.node.name.replace("_", "-")[:24])
    yield value
    snap.cleanup_snapshotjob(config, value)


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_snapshotjob_captures_and_restore_recovers_state(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.checkpoint_id
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_pod_template(config=config, run=run, gpu=True),
        )

        source_pod = snap.wait_for_job_source_pod(config.namespace, snapshotjob_name)
        source_pod_name = source_pod.metadata.name
        snap.wait_for_pod_ready(config.namespace, source_pod_name, timeout=300)
        # One observation proves that CPU, filesystem, and GPU state existed
        # before capture. No assertion below requires the source to run again.
        checkpoint_observations = snap.wait_for_state_observations(
            config.namespace,
            source_pod_name,
            run.source_token,
            gpu=True,
            minimum=1,
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        completed = snap.condition(sj, "Completed")
        assert completed and completed.get("reason") == "CaptureCompleted"
        captured = snap.condition(sj, "Captured")
        assert captured and captured.get("reason") == "CaptureCompleted"
        running = snap.condition(sj, "Running")
        assert running and running.get("reason") == "PodReady"
        assert sj["status"]["startedAt"]
        assert sj["status"]["completedAt"]

        pod_snapshot_name = sj["status"]["podSnapshotName"]
        assert pod_snapshot_name == snapshotjob_name

        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            pod_snapshot_name,
            timeout=60,
        )
        assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert content["spec"]["source"]["podRef"]["name"] == source_pod_name
        assert content["spec"]["source"]["podRef"]["containers"] == [workloads.CONTAINER]
        source_node = content["spec"]["source"]["nodeName"]

        # Completion asks Kubernetes to reap the owned Job/pod. With
        # leaveRunning disabled the source may already be gone; either outcome
        # is valid because post-capture source execution is not the contract.
        snap.wait_for_pod_deleted(config.namespace, source_pod_name, timeout=120)

        k8s.create_pod(
            workloads.restore_pod(
                config=config,
                run=run,
                gpu=True,
                source_node=source_node,
            )
        )
        snap.wait_for_restore_status(config.namespace, run.restore_pod, "completed")
        snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)

        # Inspect the shared artifact through the snapshot agent on the source
        # node. The source pod is already gone, but the agent and PVC remain.
        manifest = snap.checkpoint_artifact_manifest(
            config,
            source_node,
            snapshotjob_name,
        )
        assert "criuDump:" in manifest
        assert f"podName: {source_pod_name}" in manifest

        artifact_listing = snap.checkpoint_artifact_listing(
            config,
            source_node,
            snapshotjob_name,
        )
        assert "./inventory.img" in artifact_listing
        assert "./manifest.yaml" in artifact_listing

        output = snap.assert_restored_state(
            config.namespace,
            run.restore_pod,
            source_token=run.source_token,
            restore_token=run.restore_token,
            checkpoint_observations=checkpoint_observations,
            gpu=True,
        )
        assert f"source_token={run.source_token}" in output
        assert f"restore_token={run.restore_token}" in output

    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_deadline_exceeded_when_never_ready(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.checkpoint_id
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
            active_deadline_seconds=30,
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        failed = snap.condition(sj, "Failed")
        assert failed and failed.get("reason") == "DeadlineExceeded"
        completed = snap.condition(sj, "Completed")
        assert completed is None or completed.get("status") != "True"
        assert sj["status"]["completedAt"]

        # Not asserting status.podSnapshotName either way here: the controller
        # creates the PodSnapshot as soon as the source pod object exists,
        # independent of pod readiness, so one may already have been created
        # (stuck at Captured=False/CaptureInProgress) before the Job's
        # activeDeadlineSeconds fired — a race with reconcile timing, not a
        # documented guarantee in either direction.

        # Failed=True preserves the source Job for debugging. The batch Job
        # controller may delete its pod when activeDeadlineSeconds expires, so
        # pod retention is not part of the SnapshotJob contract.
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise
