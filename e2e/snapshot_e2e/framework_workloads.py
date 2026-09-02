# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Pods for the framework e2e tests, derived from the guide manifests.

The guides ship Deployments. The tests need plain Pods they can name, label,
pin to a node, and delete individually, so the Deployment's Pod template is
lifted into a Pod with the minimum of edits: test identity, the image under
test, e2e scheduling, and — for the restore pod — the PodSnapshot to restore
from. Everything that makes the Pod checkpointable or restorable (control
volume, probes, seccomp, device mounts, standby env) stays exactly as the
guide wrote it, so a guide that would not work for a user does not pass here.
"""

from __future__ import annotations

import copy
import os
from pathlib import Path
from typing import Any

import yaml

from snapshot_e2e import k8s
from snapshot_e2e.frameworks import FrameworkSpec
from snapshot_e2e.frameworks import framework_image
from snapshot_e2e.frameworks import framework_image_overridden
from snapshot_e2e.workloads import TestRun
from snapshot_e2e.workloads import same_node_affinity
from snapshot_e2e.workloads import workload_scheduling

RESTORE_FROM_ANNOTATION = "nvidia.com/restore-from"


def load_manifest(path: Path) -> dict[str, Any]:
    with path.open(encoding="utf-8") as handle:
        return yaml.safe_load(handle)


def source_pod(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    spec: FrameworkSpec,
    image: str | None = None,
) -> dict[str, Any]:
    deployment = load_manifest(spec.deployment_manifest)
    pod = pod_from_deployment(
        deployment,
        name=run.source_pod,
        namespace=config.namespace,
        labels=run.labels,
        image=image or framework_image(spec),
    )
    # The direct PodSnapshot flow: the test creates the PodSnapshot itself, so
    # the source must carry no restore/snapshot annotations of its own.
    pod["metadata"]["annotations"] = {}
    return pod


def restore_pod(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    spec: FrameworkSpec,
    source_node: str,
    image: str | None = None,
) -> dict[str, Any]:
    deployment = load_manifest(spec.restore_deployment_manifest)
    pod = pod_from_deployment(
        deployment,
        name=run.restore_pod,
        namespace=config.namespace,
        labels=run.labels,
        image=image or framework_image(spec),
    )
    # The guide's placeholder annotation names its own PodSnapshot; this run's
    # PodSnapshot is what must be restored. Restore is node-pinned: the agent
    # that holds the artifact is the one on the source node.
    pod["metadata"]["annotations"] = {RESTORE_FROM_ANNOTATION: run.snapshot_name}
    pod["spec"]["affinity"] = same_node_affinity(source_node)
    return pod


def model_cache_pvc(
    *,
    config: k8s.E2EConfig,
    spec: FrameworkSpec,
) -> dict[str, Any] | None:
    path = spec.model_cache_manifest_path
    if path is None:
        return None
    pvc = load_manifest(path)
    pvc["metadata"]["namespace"] = config.namespace
    storage_class = os.environ.get("SNAPSHOT_E2E_STORAGE_CLASS")
    if storage_class:
        pvc["spec"]["storageClassName"] = storage_class
    return pvc


def pod_from_deployment(
    deployment: dict[str, Any],
    *,
    name: str,
    namespace: str,
    labels: dict[str, str],
    image: str,
) -> dict[str, Any]:
    template = copy.deepcopy(deployment["spec"]["template"])
    metadata = template.get("metadata", {})
    pod_spec = template["spec"]

    # These are throwaway test pods: never restart (a restarted source would
    # re-run the whole load and hide a crash), and do not wait out the default
    # 30s grace period between tests.
    pod_spec["restartPolicy"] = "Never"
    pod_spec["terminationGracePeriodSeconds"] = 1

    # Content-addressed tags are immutable, so a cached pull is correct and
    # saves minutes on multi-GB images. An override (SNAPSHOT_E2E_FRAMEWORK_IMAGE)
    # is typically a mutable dev tag, where a cached image would test stale bits.
    pull_policy = "Always" if framework_image_overridden() else "IfNotPresent"
    for container in pod_spec.get("initContainers", []) + pod_spec["containers"]:
        container["image"] = image
        container["imagePullPolicy"] = pull_policy

    scheduling = workload_scheduling()
    pod_spec["nodeSelector"] = {**pod_spec.get("nodeSelector", {}), **scheduling["nodeSelector"]}
    pod_spec["tolerations"] = pod_spec.get("tolerations", []) + scheduling["tolerations"]

    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {**metadata.get("labels", {}), **labels},
            "annotations": dict(metadata.get("annotations", {})),
        },
        "spec": pod_spec,
    }


def main_container(pod: dict[str, Any]) -> dict[str, Any]:
    for container in pod["spec"]["containers"]:
        if container["name"] == "main":
            return container
    raise AssertionError("pod has no container named 'main'")


def env_value(container: dict[str, Any], name: str) -> str | None:
    for item in container.get("env", []):
        if item.get("name") == name:
            return item.get("value")
    return None
