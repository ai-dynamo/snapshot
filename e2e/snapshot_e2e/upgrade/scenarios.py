# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Upgrade scenarios: state created before the upgrade and checked after it."""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime

import pytest
from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e import workloads
from snapshot_e2e.upgrade import configs
from snapshot_e2e.upgrade.context import ScenarioState, UpgradeContext, UpgradeSettings


BASIC = frozenset({"basic", "all"})
ALL_ONLY = frozenset({"all"})
RESTORE_COMPAT_UNCHECKED = "RestoreCompatibilityUnchecked"
SNAPSHOTJOB_GATE = "/tmp/e2e-upgrade-release"
SNAPSHOTJOB_DEADLINE_SECONDS = 7200
CAPTURE_MEMORY_LIMIT = "4Gi"
SMALLER_MEMORY_LIMIT = "1Gi"


@dataclass(frozen=True)
class UpgradeScenario:
    name: str
    run_prefix: str
    profiles: frozenset[str]

    def applies_to(self, ctx: UpgradeContext) -> bool:
        return True

    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        raise NotImplementedError

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        raise NotImplementedError

    def cleanup(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        lifecycle.cleanup(ctx.config, state.run)

    def debug(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        lifecycle.debug_dump(ctx.config, state.run)


def create_ready_source(
    ctx: UpgradeContext, state: ScenarioState, *, gpu: bool, memory_limit: str | None = None
) -> client.V1Pod:
    k8s.create_pod(lifecycle.source_pod(config=ctx.config, run=state.run, gpu=gpu, memory_limit=memory_limit))
    pod = lifecycle.wait_for_pod_ready(ctx.config.namespace, state.run.source_pod)
    lifecycle.wait_for_file(ctx.config.namespace, state.run.source_pod, lifecycle.SOURCE_READY)
    state.node = pod.spec.node_name
    return pod


def take_snapshot(ctx: UpgradeContext, state: ScenarioState, source: client.V1Pod) -> None:
    run = state.run
    lifecycle.create_podsnapshot(ctx.config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid)
    lifecycle.wait_for_snapshot_ready(ctx.config.namespace, run.snapshot_name)
    state.snapshot = ctx.record_snapshot(run.snapshot_name, state.node)


def event_reasons(namespace: str, name: str, *, since: datetime | None = None) -> set[str]:
    return {
        event.reason
        for event in k8s.list_events(namespace)
        if event.involved_object
        and event.involved_object.name == name
        and (since is None or lifecycle.event_time(event) >= since)
    }


def delete_source(ctx: UpgradeContext, state: ScenarioState) -> None:
    k8s.delete_pod(ctx.config.namespace, state.run.source_pod)
    lifecycle.wait_for_pod_deleted(ctx.config.namespace, state.run.source_pod)


def restore_from_snapshot(
    ctx: UpgradeContext, state: ScenarioState, *, gpu: bool, snapshot_name: str | None = None
) -> client.V1Pod:
    namespace = ctx.config.namespace
    run = state.run
    delete_source(ctx, state)
    k8s.create_pod(
        lifecycle.restore_pod(
            config=ctx.config, run=run, gpu=gpu, source_node=state.node, snapshot_name=snapshot_name
        )
    )
    lifecycle.wait_for_restored_condition(namespace, run.restore_pod, "True", "RestoreSucceeded")
    pod = lifecycle.wait_for_pod_ready(namespace, run.restore_pod, timeout=300)
    lifecycle.assert_restored_state(
        namespace,
        run.restore_pod,
        source_token=run.source_token,
        restore_token=run.restore_token,
        checkpoint_observations=state.observations,
        gpu=gpu,
    )
    return pod


@dataclass(frozen=True)
class RestorePreUpgradeSnapshot(UpgradeScenario):
    gpu: bool = True

    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        source = create_ready_source(ctx, state, gpu=self.gpu)
        state.observations = lifecycle.wait_for_state_observations(
            ctx.config.namespace, state.run.source_pod, state.run.source_token, gpu=self.gpu, minimum=2
        )
        take_snapshot(ctx, state, source)

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        recorded = state.snapshot
        manifest = lifecycle.checkpoint_artifact_manifest(ctx.config, recorded.node, recorded.content_uid)
        assert f"contentUID: {recorded.content_uid}" in manifest
        assert "containerName: main" in manifest

        restore_from_snapshot(ctx, state, gpu=self.gpu)
        reasons = event_reasons(ctx.config.namespace, state.run.restore_pod)
        assert RESTORE_COMPAT_UNCHECKED not in reasons, (
            f"the restore skipped its compatibility checks: events {sorted(reasons)}"
        )


@dataclass(frozen=True)
class RestoredPodSurvivesUpgrade(UpgradeScenario):
    gpu: bool = False

    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        source = create_ready_source(ctx, state, gpu=self.gpu)
        state.observations = lifecycle.wait_for_state_observations(
            ctx.config.namespace, state.run.source_pod, state.run.source_token, gpu=self.gpu, minimum=2
        )
        take_snapshot(ctx, state, source)
        restored = restore_from_snapshot(ctx, state, gpu=self.gpu)
        state.restored_pod_uid = restored.metadata.uid

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        run = state.run
        pod = k8s.read_pod(namespace, run.restore_pod)
        assert pod.metadata.uid == state.restored_pod_uid, "the restored pod was replaced during the upgrade"
        assert pod.status.phase == "Running", f"the restored pod is {pod.status.phase}"
        restarts = {status.name: status.restart_count for status in pod.status.container_statuses or []}
        assert not any(restarts.values()), f"the restored pod restarted during the upgrade: {restarts}"
        # Earlier observations may have accumulated while other scenarios were
        # preparing. Require fresh progress after the rollout has completed.
        baseline = lifecycle.matching_observation_count(namespace, run.restore_pod, run.source_token, gpu=self.gpu)
        lifecycle.wait_for_state_observations(
            namespace, run.restore_pod, run.source_token, gpu=self.gpu, minimum=baseline + 2
        )


@dataclass(frozen=True)
class DeletePreUpgradeSnapshot(UpgradeScenario):
    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        take_snapshot(ctx, state, create_ready_source(ctx, state, gpu=False))

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        recorded = state.snapshot
        lifecycle.delete_podsnapshot(ctx.config.namespace, recorded.name)
        lifecycle.wait_for_custom_object_deleted(ctx.config.namespace, recorded.name, lifecycle.PODSNAPSHOTS)
        lifecycle.wait_for_custom_object_deleted(None, recorded.content_name, lifecycle.PODSNAPSHOTCONTENTS)
        lifecycle.wait_for_artifact_root_absent(ctx.config, recorded.node, recorded.content_uid)


def gated_snapshotjob_template(ctx: UpgradeContext, state: ScenarioState) -> dict:
    template = workloads.snapshotjob_pod_template(config=ctx.config, run=state.run, gpu=False)
    container = template["spec"]["containers"][0]
    *shell, script = container["command"]
    container["command"] = [*shell, f"while [ ! -f {SNAPSHOTJOB_GATE} ]; do sleep 1; done\n{script}"]
    return template


def assert_snapshotjob_completed(snapshotjob: dict) -> None:
    completed = lifecycle.condition(snapshotjob, "Completed")
    assert completed and completed.get("status") == "True" and completed.get("reason") == "JobCompleted", completed
    captured = lifecycle.condition(snapshotjob, "Captured")
    assert captured and captured.get("reason") == "CaptureCompleted", captured


def get_snapshotjob(ctx: UpgradeContext, name: str) -> dict:
    return lifecycle.get_custom_object(client.CustomObjectsApi(), ctx.config.namespace, name, lifecycle.SNAPSHOTJOBS)


@dataclass(frozen=True)
class SnapshotJobScenario(UpgradeScenario):
    def cleanup(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        lifecycle.cleanup_snapshotjob(ctx.config, state.run)

    def debug(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        lifecycle.debug_dump_snapshotjob(ctx.config, state.run)


@dataclass(frozen=True)
class SnapshotJobInFlight(SnapshotJobScenario):
    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        name = state.run.snapshotjob_name
        lifecycle.create_snapshotjob(
            namespace,
            name,
            gated_snapshotjob_template(ctx, state),
            active_deadline_seconds=SNAPSHOTJOB_DEADLINE_SECONDS,
        )
        source = lifecycle.wait_for_job_source_pod(namespace, name)
        lifecycle.wait_for(
            f"SnapshotJob {name} source pod running",
            lambda: True if k8s.read_pod(namespace, source.metadata.name).status.phase == "Running" else None,
            300,
        )
        state.extra["source_pod"] = source.metadata.name
        snapshotjob = get_snapshotjob(ctx, name)
        for condition_type in ("Completed", "Failed"):
            condition = lifecycle.condition(snapshotjob, condition_type)
            assert not condition or condition.get("status") != "True", (
                f"SnapshotJob {name} finished before the upgrade: {condition}"
            )

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        name = state.run.snapshotjob_name
        k8s.exec_command(namespace, state.extra["source_pod"], f"touch {SNAPSHOTJOB_GATE}")
        snapshotjob = lifecycle.wait_for_condition(
            namespace, name, plural=lifecycle.SNAPSHOTJOBS, condition_type="Completed", timeout=600
        )
        assert_snapshotjob_completed(snapshotjob)
        _, content = lifecycle.wait_for_snapshot_ready(namespace, snapshotjob["status"]["podSnapshotName"], timeout=120)
        manifest = lifecycle.checkpoint_artifact_manifest(
            ctx.config, content["spec"]["source"]["nodeName"], content["metadata"]["uid"]
        )
        assert f"podName: {state.extra['source_pod']}" in manifest


@dataclass(frozen=True)
class SnapshotJobCompleted(SnapshotJobScenario):
    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        name = state.run.snapshotjob_name
        lifecycle.create_snapshotjob(
            namespace, name, workloads.snapshotjob_pod_template(config=ctx.config, run=state.run, gpu=False)
        )
        snapshotjob = lifecycle.wait_for_condition(
            namespace, name, plural=lifecycle.SNAPSHOTJOBS, condition_type="Completed", timeout=600
        )
        assert_snapshotjob_completed(snapshotjob)
        lifecycle.wait_for_snapshot_ready(namespace, snapshotjob["status"]["podSnapshotName"], timeout=120)
        state.snapshot = ctx.record_snapshot(snapshotjob["status"]["podSnapshotName"])
        state.node = state.snapshot.node
        state.observations = 1
        state.extra["completed_at"] = snapshotjob["status"]["completedAt"]

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        name = state.run.snapshotjob_name
        snapshotjob = get_snapshotjob(ctx, name)
        assert_snapshotjob_completed(snapshotjob)
        assert snapshotjob["status"]["completedAt"] == state.extra["completed_at"], "the SnapshotJob completed again"
        assert k8s.read_job(namespace, name) is None, "the upgrade re-created the SnapshotJob's source Job"
        assert not k8s.list_job_pods(namespace, name), "the upgrade re-ran the SnapshotJob's source pod"
        restore_from_snapshot(ctx, state, gpu=False, snapshot_name=state.snapshot.name)


@dataclass(frozen=True)
class FailedRestoreStaysFailed(UpgradeScenario):
    gpu: bool = True

    def applies_to(self, ctx: UpgradeContext) -> bool:
        return not configs.get(ctx.settings.config).hold_agent

    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        run = state.run
        take_snapshot(ctx, state, create_ready_source(ctx, state, gpu=self.gpu))
        delete_source(ctx, state)
        k8s.create_pod(lifecycle.restore_pod(config=ctx.config, run=run, gpu=False, source_node=state.node))
        lifecycle.wait_for_restored_condition(namespace, run.restore_pod, "False", "RestoreFailed")

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        namespace = ctx.config.namespace
        run = state.run
        lifecycle.wait_for(
            f"RestoreAlreadyFailed on {run.restore_pod} from the upgraded agent",
            lambda: True
            if "RestoreAlreadyFailed" in event_reasons(namespace, run.restore_pod, since=ctx.upgrade_started)
            else None,
            180,
        )
        restored = lifecycle.pod_condition(k8s.read_pod(namespace, run.restore_pod), lifecycle.RESTORED_CONDITION)
        assert restored and restored.status == "False" and restored.reason == "RestoreFailed", (
            f"the failed restore changed state after the upgrade: {lifecycle.condition_summary(restored)}"
        )
        retried = event_reasons(namespace, run.restore_pod, since=ctx.upgrade_started) & {
            "RestoreRequested",
            "RestoreSucceeded",
        }
        assert not retried, f"the upgraded agent retried a failed restore: {sorted(retried)}"


@dataclass(frozen=True)
class CompatCheckStillEnforced(UpgradeScenario):
    def pre_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        take_snapshot(ctx, state, create_ready_source(ctx, state, gpu=False, memory_limit=CAPTURE_MEMORY_LIMIT))
        manifest = lifecycle.checkpoint_manifest(ctx.config, state.node, state.snapshot.content_uid)
        state.extra["records_memory_limit"] = "memoryLimit" in (manifest.get("k8s") or {})

    def post_upgrade(self, ctx: UpgradeContext, state: ScenarioState) -> None:
        if not state.extra["records_memory_limit"]:
            pytest.skip(f"{ctx.settings.from_tag} does not record memory limits in its snapshots")
        namespace = ctx.config.namespace
        run = state.run
        delete_source(ctx, state)
        k8s.create_pod(
            lifecycle.restore_pod(
                config=ctx.config, run=run, gpu=False, source_node=state.node, memory_limit=SMALLER_MEMORY_LIMIT
            )
        )
        pod = lifecycle.wait_for_restored_condition(namespace, run.restore_pod, "False", "RestoreIncompatible")
        refusal = lifecycle.pod_condition(pod, lifecycle.RESTORED_CONDITION)
        assert "memory-limit" in refusal.message, refusal.message


SCENARIOS: tuple[UpgradeScenario, ...] = (
    RestorePreUpgradeSnapshot("restore-pre-upgrade-snapshot-gpu", "upg-restore-gpu", BASIC, gpu=True),
    DeletePreUpgradeSnapshot("delete-pre-upgrade-snapshot", "upg-delete", BASIC),
    RestorePreUpgradeSnapshot("restore-pre-upgrade-snapshot-cpu", "upg-restore-cpu", ALL_ONLY, gpu=False),
    RestoredPodSurvivesUpgrade("restored-pod-survives-upgrade", "upg-restored-pod", ALL_ONLY),
    SnapshotJobInFlight("snapshotjob-in-flight", "upg-sj-inflight", ALL_ONLY),
    SnapshotJobCompleted("snapshotjob-completed", "upg-sj-done", ALL_ONLY),
    FailedRestoreStaysFailed("failed-restore-stays-failed", "upg-failed-restore", ALL_ONLY),
    CompatCheckStillEnforced("compat-check-still-enforced", "upg-compat", ALL_ONLY),
)


def by_name(name: str) -> UpgradeScenario:
    for scenario in SCENARIOS:
        if scenario.name == name:
            return scenario
    raise ValueError(f"unknown upgrade scenario {name!r}; expected one of {[s.name for s in SCENARIOS]}")


def selected(settings: UpgradeSettings) -> list[UpgradeScenario]:
    if settings.scenarios:
        return [by_name(name) for name in settings.scenarios]
    return [scenario for scenario in SCENARIOS if settings.profile in scenario.profiles]
