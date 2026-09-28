# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Upgrade scenarios: state created before the upgrade and checked after it."""

from __future__ import annotations

from dataclasses import dataclass

from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.upgrade.context import ScenarioState, UpgradeContext, UpgradeSettings


BASIC = frozenset({"basic", "all"})
RESTORE_COMPAT_UNCHECKED = "RestoreCompatibilityUnchecked"


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


def create_ready_source(ctx: UpgradeContext, state: ScenarioState, *, gpu: bool) -> client.V1Pod:
    k8s.create_pod(lifecycle.source_pod(config=ctx.config, run=state.run, gpu=gpu))
    pod = lifecycle.wait_for_pod_ready(ctx.config.namespace, state.run.source_pod)
    lifecycle.wait_for_file(ctx.config.namespace, state.run.source_pod, lifecycle.SOURCE_READY)
    state.node = pod.spec.node_name
    return pod


def take_snapshot(ctx: UpgradeContext, state: ScenarioState, source: client.V1Pod) -> None:
    run = state.run
    lifecycle.create_podsnapshot(ctx.config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid)
    lifecycle.wait_for_snapshot_ready(ctx.config.namespace, run.snapshot_name)
    state.snapshot = ctx.record_snapshot(run.snapshot_name, state.node)


def event_reasons(namespace: str, name: str) -> set[str]:
    return {
        event.reason
        for event in k8s.list_events(namespace)
        if event.involved_object and event.involved_object.name == name
    }


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
        namespace = ctx.config.namespace
        run = state.run
        recorded = state.snapshot
        manifest = lifecycle.checkpoint_artifact_manifest(ctx.config, recorded.node, recorded.content_uid)
        assert f"contentUID: {recorded.content_uid}" in manifest
        assert "containerName: main" in manifest

        k8s.delete_pod(namespace, run.source_pod)
        lifecycle.wait_for_pod_deleted(namespace, run.source_pod)
        k8s.create_pod(
            lifecycle.restore_pod(config=ctx.config, run=run, gpu=self.gpu, source_node=recorded.node)
        )
        lifecycle.wait_for_restored_condition(namespace, run.restore_pod, "True", "RestoreSucceeded")
        lifecycle.wait_for_pod_ready(namespace, run.restore_pod, timeout=300)
        lifecycle.assert_restored_state(
            namespace,
            run.restore_pod,
            source_token=run.source_token,
            restore_token=run.restore_token,
            checkpoint_observations=state.observations,
            gpu=self.gpu,
        )
        reasons = event_reasons(namespace, run.restore_pod)
        assert RESTORE_COMPAT_UNCHECKED not in reasons, (
            f"the restore skipped its compatibility checks: events {sorted(reasons)}"
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


SCENARIOS: tuple[UpgradeScenario, ...] = (
    RestorePreUpgradeSnapshot("restore-pre-upgrade-snapshot-gpu", "upg-restore-gpu", BASIC, gpu=True),
    DeletePreUpgradeSnapshot("delete-pre-upgrade-snapshot", "upg-delete", BASIC),
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
