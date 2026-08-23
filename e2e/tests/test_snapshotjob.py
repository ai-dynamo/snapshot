# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""SnapshotJob lifecycle e2e under capture-driven completion.

Unlike test_snapshot_lifecycle.py (which drives PodSnapshot directly against a
plain pod the test creates and annotates itself), these tests exercise the
SnapshotJob CRD end to end. The contract under test: the controller creates
the source batch/v1 Job; once the pod is ready, the agent dumps it and the
dump terminates the source process (there is no leave-running mode); the pod's
death is the expected success sequence, the capture result alone decides the
outcome (Completed=True/CaptureCompleted), and the source Job is deleted on
completion. Failure paths preserve the Job and stamp all four conditions.

Terminal reasons that race (the Job controller vs. the capture pipeline
observing the same dead workload) are asserted as reason *sets* via
assert_snapshotjob_failure_vector — pinning one raced reason would codify a
race as a contract.
"""

from __future__ import annotations

from typing import Any

import pytest
from kubernetes import client
from kubernetes.client import ApiException

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


def assert_snapshotjob_completed(sj: dict[str, Any]) -> None:
    """Asserts the full success vector: completion is capture-driven."""
    completed = snap.condition(sj, "Completed")
    assert completed and completed.get("reason") == "CaptureCompleted"
    captured = snap.condition(sj, "Captured")
    assert captured and captured.get("status") == "True"
    assert captured.get("reason") == "CaptureCompleted"
    # The source Job fails by design (the dump kills its process); a completed
    # SnapshotJob must not advertise that as JobFailed.
    running = snap.condition(sj, "Running")
    assert running and running.get("status") == "False"
    assert running.get("reason") == "CaptureCompleted"
    failed = snap.condition(sj, "Failed")
    assert failed is None or failed.get("status") != "True"
    assert sj["status"]["completedAt"]
    # status.startedAt is deliberately not asserted: it is recorded only if a
    # reconcile observes job.status.ready > 0, and under kill-based capture
    # that readiness window can be shorter than the watch latency.


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

        # Record the source pod's name for artifact assertions, but never poll
        # its readiness or exec into it: the dump starts on readiness and kills
        # the process, so both are races the test cannot win. The workload
        # guarantees observation seq=0 exists before it signals ready.
        source_pod = snap.wait_for_job_source_pod(config.namespace, snapshotjob_name)
        source_pod_name = source_pod.metadata.name

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        assert_snapshotjob_completed(sj)

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

        # Success is the only path that deletes the source Job; the killed pod
        # goes with it. This is the intrinsic "capture success wins over the
        # source Job's failure" check — by the time Completed=True is
        # observable, the failed Job must already be on its way out.
        snap.wait_for_pod_deleted(config.namespace, source_pod_name, timeout=120)
        assert k8s.read_job(config.namespace, snapshotjob_name) is None

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

        # checkpoint_observations=1: the workload writes observation seq=0
        # before signalling ready, so at least one observation is guaranteed
        # in the captured state without any pre-capture polling.
        output = snap.assert_restored_state(
            config.namespace,
            run.restore_pod,
            source_token=run.source_token,
            restore_token=run.restore_token,
            checkpoint_observations=1,
            gpu=True,
        )
        assert f"source_token={run.source_token}" in output
        assert f"restore_token={run.restore_token}" in output

    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_success
def test_snapshotjob_cpu_only_captures(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # First non-GPU SnapshotJob coverage: capture and artifact only, no
    # restore round trip (that is the GPU test's job).
    try:
        snapshotjob_name = run.checkpoint_id
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_pod_template(config=config, run=run, gpu=False),
        )

        source_pod = snap.wait_for_job_source_pod(config.namespace, snapshotjob_name)
        source_pod_name = source_pod.metadata.name

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        assert_snapshotjob_completed(sj)

        _, content = snap.wait_for_snapshot_ready(
            config.namespace,
            sj["status"]["podSnapshotName"],
            timeout=60,
        )
        source_node = content["spec"]["source"]["nodeName"]
        manifest = snap.checkpoint_artifact_manifest(config, source_node, snapshotjob_name)
        assert f"podName: {source_pod_name}" in manifest

        snap.wait_for_pod_deleted(config.namespace, source_pod_name, timeout=120)
        assert k8s.read_job(config.namespace, snapshotjob_name) is None
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
        # The deadline kills the never-ready pod; the raced capture failure
        # that follows is collateral, and the explicit deadline must win.
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"DeadlineExceeded"})

        # Failed=True preserves the source Job for debugging. The batch Job
        # controller may delete its pod when activeDeadlineSeconds expires, so
        # pod retention is not part of the SnapshotJob contract.
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_on_job_name_conflict(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.checkpoint_id
    foreign_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": snapshotjob_name,
            "namespace": config.namespace,
            "labels": {**run.labels},
        },
        "spec": {
            "backoffLimit": 0,
            "template": {
                "metadata": {"labels": {**run.labels}},
                "spec": {
                    "restartPolicy": "Never",
                    "containers": [
                        {
                            "name": "sleeper",
                            "image": run.image,
                            "command": ["/bin/bash", "-lc", "sleep 300"],
                        }
                    ],
                    **workloads.workload_scheduling(),
                },
            },
        },
    }
    try:
        k8s.create_job(foreign_job)
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"JobNameConflict"})

        # The foreign Job must be left untouched: never adopted, never deleted.
        job = k8s.read_job(config.namespace, snapshotjob_name)
        assert job is not None
        assert not (job.metadata.owner_references or [])
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise
    finally:
        k8s.delete_job(config.namespace, snapshotjob_name)


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_on_podsnapshot_name_conflict(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.checkpoint_id
    try:
        # A pre-existing PodSnapshot at the SnapshotJob's deterministic name,
        # owned by nobody. Its own fate (it fails on a missing source pod) is
        # irrelevant — holding the name is what makes it a terminal conflict.
        snap.create_podsnapshot(
            config.namespace,
            snapshotjob_name,
            pod_name="no-such-pod",
            pod_uid="00000000-0000-0000-0000-000000000000",
        )
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"PodSnapshotNameConflict"})
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_job_deleted(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.checkpoint_id
        # The hang workload never becomes ready, so the capture stays pending
        # forever — deleting the Job mid-capture is deterministic, not a race
        # against a dump that could complete first.
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        snap.wait_for_status_field(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            field="podSnapshotName",
        )
        assert k8s.delete_job(config.namespace, snapshotjob_name)

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        # One-shot: a deleted Job with an unresolved capture can never
        # complete. (A deleted Job with a *Ready* capture completes instead —
        # unit-covered; not deterministically reachable in e2e.)
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"JobDeleted"})
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_workload_exits_nonzero_before_capture(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.checkpoint_id
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_exit_pod_template(config=config, run=run, exit_code=1),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=300,
        )
        # The Job controller (JobFailed) races the capture pipeline observing
        # the same dead workload (SourcePodGone → CaptureFailed); the class is
        # deterministic, the reporting component is not.
        snap.assert_snapshotjob_failure_vector(
            sj, allowed_reasons={"JobFailed", "CaptureFailed"}
        )
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_workload_exits_zero_before_capture(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.checkpoint_id
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_exit_pod_template(config=config, run=run, exit_code=0),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=300,
        )
        # A zero exit before capture cannot be a success: nothing was
        # captured. The operator's SourceCompletedWithoutCapture races the
        # agent's SourcePodGone (→ CaptureFailed) on the same succeeded pod.
        snap.assert_snapshotjob_failure_vector(
            sj, allowed_reasons={"SourceCompletedWithoutCapture", "CaptureFailed"}
        )
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_spec_admission(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.checkpoint_id

    # targetContainers must name containers present in podTemplate (CEL).
    with pytest.raises(ApiException) as excinfo:
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
            target_containers=["not-in-pod-template"],
        )
    assert excinfo.value.status in (400, 422)

    # spec is immutable (self == oldSelf).
    snap.create_snapshotjob(
        config.namespace,
        snapshotjob_name,
        workloads.snapshotjob_hang_pod_template(config=config, run=run),
    )
    with pytest.raises(ApiException) as excinfo:
        client.CustomObjectsApi().patch_namespaced_custom_object(
            snap.GROUP,
            snap.VERSION,
            config.namespace,
            snap.SNAPSHOTJOBS,
            snapshotjob_name,
            {"spec": {"activeDeadlineSeconds": 60}},
        )
    assert excinfo.value.status in (400, 422)
