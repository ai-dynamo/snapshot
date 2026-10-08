# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import time

import pytest
from kubernetes import client

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


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_successful_snapshot_captures_cpu_and_fs(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        source, source_node = create_ready_source(config, run, gpu=False)
        assert snapshot_annotations(source) == {}
        snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=False,
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
        assert f"podName: {run.source_pod}" in manifest
        # The agent records cudaRestore only when the dump found CUDA processes,
        # so a capture that advertised one here would be claiming device state
        # it never took.
        assert "cudaRestore:" not in manifest

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
def test_snapshot_records_the_environment_a_restore_is_checked_against(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    """The recorded environment has to be the machine's, not merely present.

    Everything the compatibility gates decide on is read at capture and can
    never be recovered afterwards, so this compares each recorded value against
    the node object and against nvidia-smi inside the pod that was captured.

    The values the content publishes are compared against the same ground truth
    rather than against the manifest, so a projection that quietly renamed or
    dropped one is caught here too.
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
        source_status = next(
            status
            for status in source.status.container_statuses
            if status.name == snap.CONTAINER
        )
        source_image_id = snap.runtime_image_id(
            config, source_node, source_status.container_id
        )

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

        assert_recorded_environment(
            config, run, source_node, source_image_id, manifest, content
        )

        # The device half, which only a GPU cluster can prove. The call above
        # is the contract this shares with the CPU test.
        cuda = manifest["cudaRestore"]
        assert sorted(
            (gpu["uuid"], gpu["productName"]) for gpu in cuda["sourceGpus"]
        ) == sorted((gpu["uuid"], gpu["name"]) for gpu in visible_gpus)
        assert cuda["sourceDriverVersion"] == visible_gpus[0]["driver"]

        nvidia = content["status"]["source"]["devices"]["nvidia"]
        assert nvidia["driverVersion"] == visible_gpus[0]["driver"]
        assert sorted(
            instance["productName"] for instance in nvidia["instances"]
        ) == sorted(gpu["name"] for gpu in visible_gpus)
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_snapshot_records_the_cpu_environment_a_restore_is_checked_against(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    """The device-free half of the same contract, against the same ground truth.

    The GPU test covers the device facts; these are the ones a cluster without
    a GPU can still prove, and they are the facts most of the compatibility
    gates actually decide on: the kernel and architecture the node reports, and
    the image, image ID and memory limit the pod was admitted with. All of them
    are read at capture and can never be recovered afterwards.
    """
    try:
        source, source_node = create_ready_source(
            config, run, gpu=False, memory_limit=RECORDED_MEMORY_LIMIT
        )
        snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=False,
            minimum=2,
        )
        # Read before the capture, while the source container is still running.
        source_status = next(
            status
            for status in source.status.container_statuses
            if status.name == snap.CONTAINER
        )
        source_image_id = snap.runtime_image_id(
            config, source_node, source_status.container_id
        )

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

        assert_recorded_environment(
            config, run, source_node, source_image_id, manifest, content
        )
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
        _, source_node, checkpoint_observations = create_valid_checkpoint(config, run, gpu=True)

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


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_one_cpu_snapshot_restores_into_two_containers(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        source, source_node = create_ready_source(config, run, gpu=False)
        checkpoint_observations = snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=False,
            minimum=2,
        )
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )
        snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        restore_pod, restore_tokens = snap.multi_restore_pod(
            config=config,
            run=run,
            source_node=source_node,
        )
        k8s.create_pod(restore_pod)
        restored_pod = snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "True", "RestoreSucceeded"
        )
        assert snapshot_annotations(restored_pod) == {
            "nvidia.com/restore-from": run.snapshot_name,
            "nvidia.com/restore-container-map": "main=engine-0,main=engine-1",
        }
        snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)

        for destination in ("engine-0", "engine-1"):
            output = snap.assert_restored_state(
                config.namespace,
                run.restore_pod,
                source_token=run.source_token,
                restore_token=restore_tokens[destination],
                checkpoint_observations=checkpoint_observations,
                gpu=False,
                container=destination,
            )
            assert f"source_token={run.source_token}" in output
            assert f"restore_token={restore_tokens[destination]}" in output

        assert_restore_events(
            config.namespace,
            run.restore_pod,
            {"RestoreRequested", "RestoreSucceeded"},
        )
        requested_messages = [
            event.message or ""
            for event in k8s.list_events(config.namespace)
            if event.involved_object
            and event.involved_object.name == run.restore_pod
            and event.reason == "RestoreRequested"
        ]
        for destination in ("engine-0", "engine-1"):
            assert any(destination in message for message in requested_messages)
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
        _, source_node, _ = create_valid_checkpoint(config, run, gpu=True)
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
        # RestoreAlreadyFailed is deliberately not asserted: the agent marks a
        # failed restore as handled in-process, so that event only fires when a
        # fresh agent process re-encounters the already-failed pod (e.g. after
        # an agent restart) — unreachable in this single-agent flow. The sticky
        # failure itself is covered by the Restored=False/RestoreFailed
        # condition asserted above.
        assert_restore_events(
            config.namespace,
            run.restore_pod,
            {"RestoreFailed"},
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_failure
def test_restore_from_a_damaged_checkpoint_fails_and_keeps_the_snapshot(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    """A hard failure, which is not the same thing as a refusal.

    This is deliberately not a port of the GPU test above. That one fails
    because GPU discovery cannot read a device that is not there, and every
    path that produces RestoreFailed in the restore executor sits behind a
    CUDA manifest a CPU capture never writes — so no arrangement of pods or
    annotations reaches it without a GPU.

    Damaging the artifact reaches the same verdict by the only route a CPU
    cluster has: the restore is attempted and loses, rather than being turned
    away before any work happens. The property worth holding either way is
    that the failure sticks to the pod and does not spread to the snapshot,
    which is still a correct record of a capture that did succeed.
    """
    probe_value = os.environ.get("SNAPSHOT_E2E_RESTORE_FAILURE_PROBE", "false")
    if probe_value not in ("", "false", "true"):
        raise ValueError("SNAPSHOT_E2E_RESTORE_FAILURE_PROBE must be true or false")
    probe = probe_value == "true"
    if probe and not config.cpu_only:
        raise ValueError("restore-failure qualification requires CPU-only mode")
    try:
        _, source_node, _ = create_valid_checkpoint(config, run, gpu=False)
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
        snap.corrupt_checkpoint_image(config, source_node, content["metadata"]["uid"])

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
        # Normal CI proves the damaged checkpoint reports a hard failure. The
        # opt-in negative control instead demands success from that same real
        # failure, so pytest and the workflow must turn red. Limit the probe's
        # wait; it must not consume the normal ten-minute restore timeout.
        snap.wait_for_restored_condition(
            config.namespace,
            run.restore_pod,
            "True" if probe else "False",
            "RestoreSucceeded" if probe else "RestoreFailed",
            timeout=60 if probe else 600,
        )
        if probe:
            # If corruption did not cause a restore failure, leave this probe
            # green so the qualification reviewer can reject that evidence.
            # Do not force red independently of the real restore result.
            return

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
            {"RestoreFailed"},
        )
        # A refusal is reported without the agent having touched CRIU, so the
        # two verdicts would be indistinguishable here if the gate had also
        # had something to complain about. It did not: the artifact is the
        # only thing wrong, and the gate does not read artifacts.
        assert "RestoreIncompatible" not in restore_event_reasons(
            config.namespace, run.restore_pod
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
        _, source_node, _ = create_valid_checkpoint(
            config, run, gpu=True, memory_limit=CAPTURE_MEMORY_LIMIT
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

        refusal = snap.pod_condition(pod, snap.RESTORED_CONDITION)
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


@pytest.mark.cpu
@pytest.mark.snapshot_failure
def test_refused_cpu_restore_says_why_and_does_no_criu_work(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The mismatch is the memory limit, so none of this needs a GPU: the gate
    # compares what the capture recorded against what the target offers, and
    # that comparison is the same code on either kind of cluster.
    try:
        _, source_node, _ = create_valid_checkpoint(
            config, run, gpu=False, memory_limit=CAPTURE_MEMORY_LIMIT
        )
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        k8s.create_pod(
            snap.restore_pod(
                config=config,
                run=run,
                gpu=False,
                source_node=source_node,
                memory_limit=SMALLER_MEMORY_LIMIT,
            )
        )
        pod = snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "False", "RestoreIncompatible"
        )

        refusal = snap.pod_condition(pod, snap.RESTORED_CONDITION)
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


# Small enough to be refused, large enough to restore into once the checks are
# off: with the annotation on, the restore this test starts actually runs.
SKIPPABLE_MEMORY_LIMIT = "3Gi"


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_skip_annotation_lets_a_refused_restore_through(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node, _ = create_valid_checkpoint(
            config, run, gpu=True, memory_limit=CAPTURE_MEMORY_LIMIT
        )
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        body = snap.restore_pod(
            config=config,
            run=run,
            gpu=True,
            source_node=source_node,
            memory_limit=SKIPPABLE_MEMORY_LIMIT,
        )
        body["metadata"]["annotations"]["nvidia.com/snapshot-skip-compat-check"] = "true"
        k8s.create_pod(body)

        pod = snap.wait_for_restore_past_the_gate(config.namespace, run.restore_pod)
        assert snap.pod_condition(pod, snap.RESTORED_CONDITION).reason != "RestoreIncompatible"
        assert "RestoreIncompatible" not in restore_event_reasons(
            config.namespace, run.restore_pod
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_skip_annotation_lets_a_refused_cpu_restore_through(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node, _ = create_valid_checkpoint(
            config, run, gpu=False, memory_limit=CAPTURE_MEMORY_LIMIT
        )
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        body = snap.restore_pod(
            config=config,
            run=run,
            gpu=False,
            source_node=source_node,
            memory_limit=SKIPPABLE_MEMORY_LIMIT,
        )
        body["metadata"]["annotations"]["nvidia.com/snapshot-skip-compat-check"] = "true"
        k8s.create_pod(body)

        pod = snap.wait_for_restore_past_the_gate(config.namespace, run.restore_pod)
        assert snap.pod_condition(pod, snap.RESTORED_CONDITION).reason != "RestoreIncompatible"
        assert "RestoreIncompatible" not in restore_event_reasons(
            config.namespace, run.restore_pod
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_direct_content_deletion_removes_complete_artifact_root(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        source, source_node = create_ready_source(config, run, gpu=False)
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace, run.snapshot_name
        )
        content_name = content["metadata"]["name"]
        content_uid = content["metadata"]["uid"]
        assert "nvidia.com/podsnapshotcontent-artifact-cleanup" in content[
            "metadata"
        ].get("finalizers", [])
        parent_status = pod_snapshot.get("status", {})
        assert snap.artifact_root_exists(config, source_node, content_uid)
        snap.create_artifact_staging_file(config, source_node, content_uid)

        snap.delete_podsnapshotcontent(content_name)
        snap.wait_for_custom_object_deleted(
            None, content_name, snap.PODSNAPSHOTCONTENTS
        )
        snap.wait_for_artifact_root_absent(config, source_node, content_uid)

        surviving_parent = snap.get_custom_object(
            client.CustomObjectsApi(),
            config.namespace,
            run.snapshot_name,
            snap.PODSNAPSHOTS,
        )
        assert surviving_parent.get("status", {}) == parent_status
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_snapshot_deletion_cascades_content_and_artifact_cleanup(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        source, source_node = create_ready_source(config, run, gpu=False)
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
        content_name = content["metadata"]["name"]
        content_uid = content["metadata"]["uid"]

        snap.delete_podsnapshot(config.namespace, run.snapshot_name)
        snap.wait_for_custom_object_deleted(
            config.namespace, run.snapshot_name, snap.PODSNAPSHOTS
        )
        snap.wait_for_custom_object_deleted(
            None, content_name, snap.PODSNAPSHOTCONTENTS
        )
        snap.wait_for_artifact_root_absent(config, source_node, content_uid)
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.cpu
@pytest.mark.snapshot_success
def test_orphan_sweep_reclaims_uid_root(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        _, source_node = create_ready_source(config, run, gpu=False)
        orphan_uid = f"e2e-orphan-{run.suffix}"
        snap.create_artifact_staging_file(config, source_node, orphan_uid)
        assert snap.artifact_root_exists(config, source_node, orphan_uid)
        snap.wait_for_artifact_root_absent(
            config, source_node, orphan_uid, timeout=60
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


def create_valid_checkpoint(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    *,
    gpu: bool,
    memory_limit: str | None = None,
) -> tuple[object, str, int]:
    source, source_node = create_ready_source(config, run, gpu=gpu, memory_limit=memory_limit)
    checkpoint_observations = snap.wait_for_state_observations(
        config.namespace,
        run.source_pod,
        run.source_token,
        gpu=gpu,
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


def assert_recorded_environment(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    source_node: str,
    source_image_id: str,
    manifest: dict,
    content: dict,
) -> None:
    """The device-free environment facts, in both the manifest and the content.

    Shared by the CPU and GPU environment tests rather than copied into each:
    these are assertions about a projection, so a change to it has to be able
    to fail in one place. The GPU test adds its device assertions on top.

    Every expectation is read from the node and the pod rather than from the
    manifest, so a projection that renamed or dropped a field is caught instead
    of being compared against itself.
    """
    node_info = k8s.read_node(source_node).status.node_info
    host = manifest["host"]
    assert host["kernelVersion"] == node_info.kernel_version
    assert host["cpuArch"] == node_info.architecture

    pod = k8s.read_pod(config.namespace, run.source_pod)
    container = next(c for c in pod.spec.containers if c.name == snap.CONTAINER)
    limits = (container.resources.limits or {}) if container.resources else {}
    recorded_pod = manifest["k8s"]
    assert recorded_pod["image"] == container.image
    if source_image_id:
        assert recorded_pod["imageId"] == source_image_id
    else:
        # Missing CRI image_id is a supported unknown, not a failed capture.
        # Verify omission rather than skipping this environment test.
        assert "imageId" not in recorded_pod
    assert recorded_pod["memoryLimit"] == limits["memory"]
    # This pod sets no CPU limit, and an absent value is recorded as absent
    # rather than invented, which is what makes it refuse nothing later.
    assert "cpu" not in limits
    assert "cpuLimit" not in recorded_pod

    published = content["status"]["source"]
    assert published["node"] == {
        "name": source_node,
        "architecture": node_info.architecture,
        "kernelVersion": node_info.kernel_version,
    }
    # Exact equality, because the CPU limit this pod never set must stay
    # absent here as well as in the manifest.
    expected_pod = {
        "image": container.image,
        "memory": limits["memory"],
    }
    if source_image_id:
        expected_pod["imageDigest"] = source_image_id.split("://")[-1].rsplit("@", 1)[-1]
    assert published["pod"] == expected_pod


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
