# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import time

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_successful_snapshot_captures_cpu_gpu_and_fs(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        source, source_node = create_ready_source(config, run, gpu=True)
        assert snapshot_annotations(source) == {}
        snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=True,
            minimum=2,
        )
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )

        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
        )
        assert_podsnapshot_ready(pod_snapshot, content, source, source_node)
        content_uid = content["metadata"]["uid"]
        manifest = snap.checkpoint_artifact_manifest(
            config,
            source_node,
            content_uid,
        )
        assert f"contentUID: {content_uid}" in manifest
        assert "containerName: main" in manifest
        assert "criuDump:" in manifest
        assert "cudaRestore:" in manifest
        assert f"podName: {run.source_pod}" in manifest

        artifact_listing = snap.checkpoint_artifact_listing(
            config,
            source_node,
            content_uid,
        )
        assert "./inventory.img" in artifact_listing
        assert "./manifest.yaml" in artifact_listing
        assert "./rootfs-diff.tar" in artifact_listing
        assert "./tmp/e2e-state/file-token" in artifact_listing
        assert "./tmp/e2e-state/observations.log" in artifact_listing

        file_token = snap.checkpoint_rootfs_file(
            config,
            source_node,
            content_uid,
            "./tmp/e2e-state/file-token",
        )
        assert file_token.strip() == run.source_token
    except Exception:
        snap.debug_dump(config, run)
        raise


# The value does not matter, only that one is set: a limit neither side records
# compares equal to itself and proves nothing.
RECORDED_MEMORY_LIMIT = "4Gi"


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_snapshot_records_the_facts_a_restore_is_checked_against(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    """The recorded facts have to be the machine's, not merely present.

    Everything the compatibility gates decide on is read at capture and can
    never be recovered afterwards, so this compares each recorded fact against
    the node object and against nvidia-smi inside the pod that was captured.
    """
    try:
        source, source_node = create_ready_source(
            config, run, gpu=True, memory_limit=RECORDED_MEMORY_LIMIT
        )
        snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=True,
            minimum=2,
        )
        # Read before the capture, while the source container is still running.
        visible_gpus = snap.visible_gpus(config.namespace, run.source_pod)
        assert visible_gpus, "the GPU workload could not see a GPU"

        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
        manifest = snap.checkpoint_manifest(
            config, source_node, content["metadata"]["uid"]
        )

        node_info = k8s.read_node(source_node).status.node_info
        host = manifest["host"]
        assert host["kernelVersion"] == node_info.kernel_version
        assert host["cpuArch"] == node_info.architecture

        pod = k8s.read_pod(config.namespace, run.source_pod)
        container = next(c for c in pod.spec.containers if c.name == snap.CONTAINER)
        status = next(s for s in pod.status.container_statuses if s.name == snap.CONTAINER)
        limits = (container.resources.limits or {}) if container.resources else {}
        recorded_pod = manifest["k8s"]
        assert recorded_pod["image"] == container.image
        assert recorded_pod["imageId"] == status.image_id
        assert recorded_pod["memoryLimit"] == limits["memory"]
        # This pod sets no CPU limit, and an absent fact is recorded as absent
        # rather than invented, which is what makes it refuse nothing later.
        assert "cpu" not in limits
        assert "cpuLimit" not in recorded_pod

        cuda = manifest["cudaRestore"]
        assert sorted(
            (gpu["uuid"], gpu["productName"]) for gpu in cuda["sourceGpus"]
        ) == sorted((gpu["uuid"], gpu["name"]) for gpu in visible_gpus)
        assert cuda["sourceDriverVersion"] == visible_gpus[0]["driver"]
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_successful_restore_recovers_cpu_gpu_and_fs_from_snapshot(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node, checkpoint_observations = create_valid_gpu_checkpoint(config, run)

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        k8s.create_pod(
            snap.restore_pod(
                config=config,
                run=run,
                gpu=True,
                source_node=source_node,
            )
        )
        restored_pod = snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "True", "RestoreSucceeded"
        )
        assert snapshot_annotations(restored_pod) == {
            "nvidia.com/restore-from": run.snapshot_name
        }
        snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)

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
        assert_restore_events(
            config.namespace,
            run.restore_pod,
            {"RestoreRequested", "RestoreSucceeded"},
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_failure
@pytest.mark.gpu
def test_failed_restore_gpu_checkpoint_into_non_gpu_target(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node, _ = create_valid_gpu_checkpoint(config, run)
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        k8s.create_pod(
            snap.restore_pod(
                config=config,
                run=run,
                gpu=False,
                source_node=source_node,
            )
        )
        snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "False", "RestoreFailed"
        )

        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
            timeout=60,
        )
        assert snap.condition(pod_snapshot, "Ready")["status"] == "True"
        assert snap.condition(content, "Ready")["status"] == "True"
        assert_restore_events(
            config.namespace,
            run.restore_pod,
            {"RestoreFailed", "RestoreAlreadyFailed"},
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


# A checkpoint captured with more memory than the target offers is the cheapest
# real mismatch to build: nothing about the node has to change for it.
CAPTURE_MEMORY_LIMIT = "4Gi"
SMALLER_MEMORY_LIMIT = "1Gi"

# The restore informer resyncs every 30s, which is what would re-drive a refused
# pod. Waiting past two of them is how a retry loop would show itself.
RESTORE_RESYNC_SECONDS = 30


@pytest.mark.snapshot_failure
@pytest.mark.gpu
def test_refused_restore_says_why_and_does_no_criu_work(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node, _ = create_valid_gpu_checkpoint(
            config, run, memory_limit=CAPTURE_MEMORY_LIMIT
        )
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        k8s.create_pod(
            snap.restore_pod(
                config=config,
                run=run,
                gpu=True,
                source_node=source_node,
                memory_limit=SMALLER_MEMORY_LIMIT,
            )
        )
        pod = snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "False", "RestoreIncompatible"
        )

        refusal = snap.pod_condition(pod, "snapshot/Restored")
        assert "memory-limit" in refusal.message
        assert CAPTURE_MEMORY_LIMIT in refusal.message
        assert SMALLER_MEMORY_LIMIT in refusal.message

        assert_restore_events(config.namespace, run.restore_pod, {"RestoreIncompatible"})
        time.sleep(2 * RESTORE_RESYNC_SECONDS + 5)
        assert restore_event_count(config.namespace, run.restore_pod, "RestoreIncompatible") == 1
        assert "RestoreFailed" not in restore_event_reasons(config.namespace, run.restore_pod)

        # The placeholder is still the placeholder: a refusal costs no CRIU work,
        # so the workload never sees restore-complete.
        assert not snap.file_present(config.namespace, run.restore_pod, snap.RESTORE_DONE)
    except Exception:
        snap.debug_dump(config, run)
        raise


def create_valid_gpu_checkpoint(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    *,
    memory_limit: str | None = None,
) -> tuple[object, str, int]:
    source, source_node = create_ready_source(config, run, gpu=True, memory_limit=memory_limit)
    checkpoint_observations = snap.wait_for_state_observations(
        config.namespace,
        run.source_pod,
        run.source_token,
        gpu=True,
        minimum=2,
    )
    snap.create_podsnapshot(
        config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
    )
    pod_snapshot, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
    assert_podsnapshot_ready(pod_snapshot, content, source, source_node)
    return source, source_node, checkpoint_observations


def create_ready_source(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    *,
    gpu: bool,
    annotations: dict[str, str] | None = None,
    memory_limit: str | None = None,
) -> tuple[object, str]:
    k8s.create_pod(
        snap.source_pod(
            config=config,
            run=run,
            gpu=gpu,
            annotations=annotations,
            memory_limit=memory_limit,
        )
    )
    pod = snap.wait_for_pod_ready(config.namespace, run.source_pod)
    snap.wait_for_file(config.namespace, run.source_pod, snap.SOURCE_READY)
    return pod, pod.spec.node_name


def snapshot_annotations(pod: object) -> dict[str, str]:
    annotations = pod.metadata.annotations or {}
    return {
        key: value
        for key, value in annotations.items()
        if key.startswith(("nvidia.com/restore-", "nvidia.com/snapshot-"))
    }


def assert_podsnapshot_ready(
    pod_snapshot: dict,
    content: dict,
    source: object,
    source_node: str,
) -> None:
    ready = snap.condition(pod_snapshot, "Ready")
    assert ready and ready.get("status") == "True"
    failed = snap.condition(pod_snapshot, "Failed")
    assert failed is None or failed.get("status") != "True"
    assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]

    content_ready = snap.condition(content, "Ready")
    assert content_ready and content_ready.get("status") == "True"
    assert content_ready.get("reason") == "Captured"
    content_failed = snap.condition(content, "Failed")
    assert content_failed is None or content_failed.get("status") != "True"
    assert content["spec"]["source"]["podRef"]["name"] == source.metadata.name
    assert content["spec"]["source"]["podRef"]["uid"] == source.metadata.uid
    assert content["spec"]["source"]["podRef"]["containers"] == [snap.CONTAINER]
    assert content["spec"]["source"]["nodeName"] == source_node
    assert content["metadata"].get("labels", {}).get("nvidia.com/snapshot-node") == source_node


def assert_restore_events(
    namespace: str,
    pod_name: str,
    expected_reasons: set[str],
    *,
    timeout: int = 45,
) -> None:
    def observed() -> set[str] | None:
        reasons = restore_event_reasons(namespace, pod_name)
        return reasons if expected_reasons.issubset(reasons) else None

    def detail() -> str:
        return f"saw={sorted(restore_event_reasons(namespace, pod_name))}"

    reasons = snap.wait_for(
        f"restore events {sorted(expected_reasons)} for {namespace}/{pod_name}",
        observed,
        timeout,
        detail=detail,
    )
    missing = expected_reasons - reasons
    assert not missing, f"missing restore events {missing}; saw {sorted(reasons)}"


def restore_event_reasons(namespace: str, pod_name: str) -> set[str]:
    events = k8s.list_events(namespace)
    return {
        event.reason
        for event in events
        if event.involved_object and event.involved_object.name == pod_name
    }


def restore_event_count(namespace: str, pod_name: str, reason: str) -> int:
    """How many times the pod was told this, not how many objects say it.

    Repeated events are aggregated into one object with a count, so counting
    objects would report one no matter how often the agent repeated itself.
    """
    return sum(
        event.count or 1
        for event in k8s.list_events(namespace)
        if event.involved_object
        and event.involved_object.name == pod_name
        and event.reason == reason
    )
