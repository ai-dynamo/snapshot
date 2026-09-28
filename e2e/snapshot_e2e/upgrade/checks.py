# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checks that the upgrade finished cleanly and pre-upgrade state survived it."""

from __future__ import annotations

import json
import os
import subprocess
import time
from datetime import datetime
from pathlib import Path
from typing import Any

import yaml
from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.upgrade.configs import UpgradeConfig
from snapshot_e2e.upgrade.context import UpgradeContext


CRD_DIR = Path(__file__).resolve().parents[3] / "api" / "v1alpha1" / "crds"
OPERATOR = "operator"
AGENT = "snapshot-agent"
OPERATOR_CONTAINER = "manager"
AGENT_CONTAINER = "agent"
PAGEBROKER_CONTAINER = "pagebroker"
CRD_INSTALLER = "crd-installer"
CLEANUP_SCANS_WAIT_SECONDS = 20
LOG_TAIL_LINES = 5000


def helm(ctx: UpgradeContext, *args: str) -> str:
    env = os.environ.copy()
    if ctx.config.kubeconfig:
        env["KUBECONFIG"] = ctx.config.kubeconfig
    result = subprocess.run(
        ["helm", *args, "--namespace", ctx.config.namespace],
        env=env,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise AssertionError(f"helm {' '.join(args)} failed: {result.stderr.strip()}")
    return result.stdout


def helm_release(ctx: UpgradeContext) -> tuple[int, str]:
    status = json.loads(helm(ctx, "status", ctx.config.release, "--output", "json"))
    return int(status["version"]), status["info"]["status"]


def image_tag(image: str) -> str:
    return image.rsplit("@", 1)[0].rsplit(":", 1)[-1]


def operator_deployment(ctx: UpgradeContext) -> client.V1Deployment:
    deployments = client.AppsV1Api().list_namespaced_deployment(
        ctx.config.namespace,
        label_selector=k8s.snapshot_selector(ctx.config.release, OPERATOR),
    ).items
    assert len(deployments) == 1, f"expected one operator Deployment, found {len(deployments)}"
    return deployments[0]


def agent_daemonset(ctx: UpgradeContext) -> client.V1DaemonSet:
    daemonsets = k8s.list_snapshot_daemonsets(ctx.config.namespace, ctx.config.release, AGENT)
    assert len(daemonsets) == 1, f"expected one agent DaemonSet, found {len(daemonsets)}"
    return daemonsets[0]


def container_image(containers: list[Any] | None, name: str) -> str:
    for container in containers or []:
        if container.name == name:
            return container.image
    raise AssertionError(f"no container named {name!r}")


def installed_tags(ctx: UpgradeContext) -> dict[str, str]:
    operator = operator_deployment(ctx).spec.template.spec
    agent = agent_daemonset(ctx).spec.template.spec
    return {
        "operator": image_tag(container_image(operator.containers, OPERATOR_CONTAINER)),
        "agent": image_tag(container_image(agent.containers, AGENT_CONTAINER)),
    }


def snapshot_pods(ctx: UpgradeContext, component: str) -> list[client.V1Pod]:
    return k8s.list_snapshot_pods(ctx.config.namespace, ctx.config.release, component)


def rollout_state(ctx: UpgradeContext, upgrade: UpgradeConfig) -> list[str]:
    settings = ctx.settings
    operator_tag = upgrade.operator_tag(settings)
    agent_tag = upgrade.agent_tag(settings)
    pending = []

    deployment = operator_deployment(ctx)
    template_tag = image_tag(container_image(deployment.spec.template.spec.containers, OPERATOR_CONTAINER))
    if template_tag != operator_tag:
        pending.append(f"operator template runs {template_tag}, want {operator_tag}")
    status = deployment.status
    replicas = deployment.spec.replicas or 0
    if (status.observed_generation or 0) < (deployment.metadata.generation or 0):
        pending.append("operator Deployment generation not observed")
    if (status.updated_replicas or 0) < replicas or (status.ready_replicas or 0) < replicas:
        pending.append(
            f"operator updated={status.updated_replicas} ready={status.ready_replicas} want={replicas}"
        )
    if (status.replicas or 0) > replicas:
        pending.append(f"operator still has {status.replicas} pods, want {replicas}")

    daemonset = agent_daemonset(ctx)
    template_tag = image_tag(container_image(daemonset.spec.template.spec.containers, AGENT_CONTAINER))
    if template_tag != agent_tag:
        pending.append(f"agent template runs {template_tag}, want {agent_tag}")
    if not k8s.daemonset_ready(daemonset):
        pending.append(k8s.daemonset_readiness_detail(daemonset))

    for component, container, tag in (
        (OPERATOR, OPERATOR_CONTAINER, operator_tag),
        (AGENT, AGENT_CONTAINER, agent_tag),
    ):
        for pod in snapshot_pods(ctx, component):
            pod_tag = image_tag(container_image(pod.spec.containers, container))
            if pod.metadata.deletion_timestamp or pod_tag != tag or not k8s.pod_containers_ready(pod):
                pending.append(f"{k8s.pod_readiness_detail(pod)} tag={pod_tag}")
    return pending


def wait_for_rollout(ctx: UpgradeContext, upgrade: UpgradeConfig) -> None:
    last: list[str] = []

    def done() -> bool | None:
        nonlocal last
        last = rollout_state(ctx, upgrade)
        return True if not last else None

    lifecycle.wait_for(
        "Snapshot operator and agent rolled out to the upgraded version",
        done,
        ctx.settings.ready_timeout_seconds,
        detail=lambda: "; ".join(last),
    )


def assert_crd_installer_succeeded(ctx: UpgradeContext) -> None:
    for pod in snapshot_pods(ctx, OPERATOR):
        statuses = {status.name: status for status in pod.status.init_container_statuses or []}
        status = statuses.get(CRD_INSTALLER)
        assert status is not None, f"{pod.metadata.name} has no {CRD_INSTALLER} init container"
        terminated = status.state.terminated if status.state else None
        assert terminated and terminated.exit_code == 0, (
            f"{pod.metadata.name} {CRD_INSTALLER} did not exit 0: {status.state}"
        )


def schema_paths(schema: dict[str, Any], prefix: str = "") -> set[str]:
    paths: set[str] = set()
    for name, child in (schema.get("properties") or {}).items():
        path = f"{prefix}.{name}"
        paths.add(path)
        paths |= schema_paths(child, path)
    items = schema.get("items")
    if isinstance(items, dict):
        paths |= schema_paths(items, f"{prefix}[]")
    return paths


def crd_version_schemas(crd: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {
        version["name"]: version.get("schema", {}).get("openAPIV3Schema", {})
        for version in crd["spec"]["versions"]
    }


def missing_crd_fields(expected: dict[str, Any], served: dict[str, Any]) -> list[str]:
    served_schemas = crd_version_schemas(served)
    missing = []
    for version, schema in crd_version_schemas(expected).items():
        if version not in served_schemas:
            missing.append(f"version {version}")
            continue
        missing.extend(
            f"{version}{path}" for path in sorted(schema_paths(schema) - schema_paths(served_schemas[version]))
        )
    return missing


def assert_crds_upgraded() -> None:
    api = client.ApiextensionsV1Api()
    serializer = client.ApiClient()
    problems = []
    for path in sorted(CRD_DIR.glob("*.yaml")):
        expected = yaml.safe_load(path.read_text(encoding="utf-8"))
        name = expected["metadata"]["name"]
        served = serializer.sanitize_for_serialization(api.read_custom_resource_definition(name))
        missing = missing_crd_fields(expected, served)
        if missing:
            problems.append(f"{name} is missing {', '.join(missing[:10])}")
    assert not problems, "served CRDs do not match api/v1alpha1/crds: " + "; ".join(problems)


def assert_no_restarts_or_panics(ctx: UpgradeContext) -> None:
    problems = []
    for component in (OPERATOR, AGENT):
        for pod in snapshot_pods(ctx, component):
            for status in pod.status.container_statuses or []:
                if status.restart_count:
                    problems.append(f"{pod.metadata.name}/{status.name} restarted {status.restart_count}x")
                logs = k8s.pod_logs(
                    ctx.config.namespace, pod.metadata.name, tail_lines=LOG_TAIL_LINES, container=status.name
                )
                if "panic:" in logs:
                    problems.append(f"{pod.metadata.name}/{status.name} logged a panic")
    assert not problems, "; ".join(problems)


def warning_events(ctx: UpgradeContext, since: datetime) -> list[str]:
    names = {s.name for s in ctx.snapshots} | {s.content_name for s in ctx.snapshots}
    events = client.CoreV1Api().list_event_for_all_namespaces(field_selector="type=Warning").items
    return [
        f"{event.involved_object.kind}/{event.involved_object.name} {event.reason}: {event.message}"
        for event in events
        if event.involved_object
        and event.involved_object.name in names
        and lifecycle.event_time(event) >= since
    ]


def pagebroker_problem(pod: client.V1Pod, agent_tag: str, chart_deploys_pagebroker: bool) -> str | None:
    containers = {container.name: container for container in pod.spec.containers}
    if image_tag(containers[AGENT_CONTAINER].image) != agent_tag:
        return None
    pagebroker = containers.get(PAGEBROKER_CONTAINER)
    if not chart_deploys_pagebroker:
        if pagebroker is not None:
            return f"{pod.metadata.name} runs pagebroker, which the upgraded chart does not deploy"
        return None
    if pagebroker is None:
        return f"{pod.metadata.name} runs the upgraded agent without pagebroker, which the chart deploys"
    if image_tag(pagebroker.image) != agent_tag:
        return f"{pod.metadata.name} runs pagebroker {image_tag(pagebroker.image)} next to agent {agent_tag}"
    return None


def assert_pagebroker_matches_chart(ctx: UpgradeContext) -> None:
    template = agent_daemonset(ctx).spec.template.spec.containers
    chart_deploys_pagebroker = any(container.name == PAGEBROKER_CONTAINER for container in template)
    problems = [
        problem
        for pod in snapshot_pods(ctx, AGENT)
        if (problem := pagebroker_problem(pod, ctx.settings.to_tag, chart_deploys_pagebroker))
    ]
    assert not problems, "; ".join(problems)


def assert_upgrade_completed(ctx: UpgradeContext, upgrade: UpgradeConfig) -> None:
    revision, status = helm_release(ctx)
    assert status == "deployed", f"helm release status is {status!r}"
    assert revision > ctx.revision_before, (
        f"helm revision {revision} is not newer than pre-upgrade revision {ctx.revision_before}"
    )
    wait_for_rollout(ctx, upgrade)
    assert_crd_installer_succeeded(ctx)
    assert_crds_upgraded()
    assert_pagebroker_matches_chart(ctx)
    assert_no_restarts_or_panics(ctx)
    warnings = warning_events(ctx, ctx.upgrade_started)
    assert not warnings, "Warning events on pre-upgrade objects: " + "; ".join(warnings)


def assert_global_invariants(ctx: UpgradeContext) -> None:
    time.sleep(CLEANUP_SCANS_WAIT_SECONDS)
    api = client.CustomObjectsApi()
    problems = []
    for recorded in ctx.snapshots:
        try:
            snapshot = lifecycle.get_custom_object(
                api, ctx.config.namespace, recorded.name, lifecycle.PODSNAPSHOTS
            )
            content = lifecycle.get_custom_object(
                api, None, recorded.content_name, lifecycle.PODSNAPSHOTCONTENTS
            )
        except ApiException as exc:
            problems.append(f"{recorded.name}: {k8s.api_error_detail(exc)}")
            continue
        if snapshot["metadata"]["uid"] != recorded.uid:
            problems.append(f"PodSnapshot {recorded.name} was recreated")
        if content["metadata"]["uid"] != recorded.content_uid:
            problems.append(f"PodSnapshotContent {recorded.content_name} was recreated")
        for obj, kind in ((snapshot, "PodSnapshot"), (content, "PodSnapshotContent")):
            ready = lifecycle.condition(obj, "Ready")
            if not ready or ready.get("status") != "True":
                problems.append(f"{kind} {obj['metadata']['name']} is no longer Ready: {ready}")
        lost = set(recorded.content_finalizers) - set(content["metadata"].get("finalizers") or ())
        if lost:
            problems.append(f"PodSnapshotContent {recorded.content_name} lost finalizers {sorted(lost)}")
        if not lifecycle.artifact_root_exists(ctx.config, recorded.node, recorded.content_uid):
            problems.append(f"artifacts of {recorded.content_name} were removed from the PVC")

    warnings = warning_events(ctx, ctx.upgrade_started)
    if warnings:
        problems.append("Warning events on pre-upgrade objects: " + "; ".join(warnings))
    assert not problems, "; ".join(problems)


def dump_diagnostics(ctx: UpgradeContext) -> None:
    print("\n--- snapshot upgrade e2e debug ---")
    try:
        print(helm(ctx, "history", ctx.config.release))
    except AssertionError as exc:
        print(exc)
    core = client.CoreV1Api()
    for component in (OPERATOR, AGENT):
        for pod in snapshot_pods(ctx, component):
            print(f"pod {pod.metadata.name} phase={pod.status.phase} node={pod.spec.node_name}")
            for status in list(pod.status.init_container_statuses or []) + list(pod.status.container_statuses or []):
                print(f"  {status.name} ready={status.ready} restarts={status.restart_count} state={status.state}")
                print(k8s.pod_logs(ctx.config.namespace, pod.metadata.name, tail_lines=80, container=status.name))
                if status.restart_count:
                    try:
                        print(
                            core.read_namespaced_pod_log(
                                pod.metadata.name,
                                ctx.config.namespace,
                                container=status.name,
                                previous=True,
                                tail_lines=80,
                            )
                        )
                    except ApiException as exc:
                        print(f"<previous logs unavailable: {k8s.api_error_detail(exc)}>")
    print("--- end debug ---\n")
