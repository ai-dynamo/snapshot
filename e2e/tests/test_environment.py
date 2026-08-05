# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest
from kubernetes.client import ApiException

from snapshot_e2e import k8s


@pytest.mark.environment
def test_snapshot_e2e_environment_is_ready() -> None:
    config = k8s.E2EConfig.from_env()
    k8s.configure(config)

    try:
        namespace = k8s.read_namespace(config.namespace)
    except ApiException as exc:
        pytest.fail(
            f"Snapshot e2e namespace {config.namespace!r} is not readable: "
            f"{k8s.api_error_detail(exc)}"
        )
    assert namespace.status.phase == "Active"

    for crd_name in ("podsnapshots.nvidia.com", "podsnapshotcontents.nvidia.com"):
        try:
            crd = k8s.read_crd(crd_name)
        except ApiException as exc:
            pytest.fail(f"CRD {crd_name!r} is not readable: {k8s.api_error_detail(exc)}")
        served_versions = [version.name for version in crd.spec.versions if version.served]
        assert "v1alpha1" in served_versions

    try:
        k8s.snapshot_custom_resource_api_is_accessible(config.namespace)
    except ApiException as exc:
        pytest.fail(f"Snapshot custom resource API is not accessible: {k8s.api_error_detail(exc)}")

    try:
        pvc = k8s.read_pvc(config.namespace, config.pvc_name)
    except ApiException as exc:
        pytest.fail(
            f"Snapshot checkpoint PVC {config.namespace}/{config.pvc_name} "
            f"is not readable: {k8s.api_error_detail(exc)}"
        )
    assert pvc.status.phase != "Lost"

    operators = k8s.list_snapshot_pods(
        config.namespace,
        config.release,
        "operator",
    )
    assert operators, "Snapshot operator pod was not found"
    not_ready = [
        k8s.pod_readiness_detail(pod)
        for pod in operators
        if not k8s.pod_containers_ready(pod)
    ]
    assert not not_ready, "Snapshot operator pod is not ready: " + "; ".join(not_ready)

    agents = k8s.list_snapshot_daemonsets(
        config.namespace,
        config.release,
        "snapshot-agent",
    )
    assert agents, "Snapshot agent daemonset was not found"
    not_ready = [
        k8s.daemonset_readiness_detail(daemonset)
        for daemonset in agents
        if not k8s.daemonset_scheduled(daemonset)
    ]
    assert not not_ready, "Snapshot agent daemonset did not schedule pods: " + "; ".join(not_ready)

    agent_pods = k8s.list_snapshot_pods(
        config.namespace,
        config.release,
        "snapshot-agent",
    )
    assert agent_pods, "Snapshot agent pod was not found"
    not_ready = [
        k8s.pod_readiness_detail(pod)
        for pod in agent_pods
        if not k8s.pod_containers_ready(pod)
    ]
    assert not not_ready, "Snapshot agent pod is not ready: " + "; ".join(not_ready)
