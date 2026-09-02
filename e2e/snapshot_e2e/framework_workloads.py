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
from snapshot_e2e.frameworks import MODEL_CACHE_MOUNT
from snapshot_e2e.frameworks import MODEL_CACHE_VOLUME
from snapshot_e2e.frameworks import FrameworkSpec
from snapshot_e2e.frameworks import SharedModelCache
from snapshot_e2e.frameworks import framework_image
from snapshot_e2e.workloads import TestRun
from snapshot_e2e.workloads import same_node_affinity
from snapshot_e2e.workloads import workload_scheduling

RESTORE_FROM_ANNOTATION = "nvidia.com/restore-from"
# The guides' own cache plumbing, replaced when a shared cache is configured:
# the init container downloads into the guide PVC, which the shared export
# makes both unnecessary and impossible offline.
GUIDE_CACHE_INIT_CONTAINER = "model-cache"
# Sized and mounted like a typical CI model share: read-mostly, many readers,
# large sequential reads of safetensors. Capacity is nominal for an NFS PV.
SHARED_CACHE_CAPACITY = "1024Gi"
SHARED_CACHE_MOUNT_OPTIONS = [
    "vers=4",
    "minorversion=1",
    "sec=sys",
    "nconnect=4",
    "rsize=1048576",
    "wsize=1048576",
    "hard",
]


def load_manifest(path: Path) -> dict[str, Any]:
    with path.open(encoding="utf-8") as handle:
        return yaml.safe_load(handle)


def source_pod(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    spec: FrameworkSpec,
    image: str | None = None,
    model_cache: SharedModelCache | None = None,
) -> dict[str, Any]:
    deployment = load_manifest(spec.deployment_manifest)
    pod = pod_from_deployment(
        deployment,
        name=run.source_pod,
        namespace=config.namespace,
        labels=run.labels,
        image=image or framework_image(spec),
        model_cache=model_cache,
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
    model_cache: SharedModelCache | None = None,
) -> dict[str, Any]:
    deployment = load_manifest(spec.restore_deployment_manifest)
    pod = pod_from_deployment(
        deployment,
        name=run.restore_pod,
        namespace=config.namespace,
        labels=run.labels,
        image=image or framework_image(spec),
        model_cache=model_cache,
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


def shared_model_cache_volume(
    *,
    config: k8s.E2EConfig,
    cache: SharedModelCache,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """PersistentVolume and PersistentVolumeClaim for the shared NFS cache.

    Static binding (empty storageClassName + volumeName) so no provisioner is
    involved; Retain so deleting the claim can never touch the export.
    """
    pv = {
        "apiVersion": "v1",
        "kind": "PersistentVolume",
        "metadata": {"name": cache.pvc_name},
        "spec": {
            "capacity": {"storage": SHARED_CACHE_CAPACITY},
            "accessModes": ["ReadWriteMany"],
            "persistentVolumeReclaimPolicy": "Retain",
            "storageClassName": "",
            "mountOptions": list(SHARED_CACHE_MOUNT_OPTIONS),
            "nfs": {"server": cache.server, "path": cache.path},
        },
    }
    pvc = {
        "apiVersion": "v1",
        "kind": "PersistentVolumeClaim",
        "metadata": {"name": cache.pvc_name, "namespace": config.namespace},
        "spec": {
            "accessModes": ["ReadWriteMany"],
            "storageClassName": "",
            "volumeName": cache.pvc_name,
            "resources": {"requests": {"storage": SHARED_CACHE_CAPACITY}},
        },
    }
    return pv, pvc


def pod_from_deployment(
    deployment: dict[str, Any],
    *,
    name: str,
    namespace: str,
    labels: dict[str, str],
    image: str,
    model_cache: SharedModelCache | None = None,
) -> dict[str, Any]:
    template = copy.deepcopy(deployment["spec"]["template"])
    metadata = template.get("metadata", {})
    pod_spec = template["spec"]

    # These are throwaway test pods: never restart (a restarted source would
    # re-run the whole load and hide a crash), and do not wait out the default
    # 30s grace period between tests.
    pod_spec["restartPolicy"] = "Never"
    pod_spec["terminationGracePeriodSeconds"] = 1

    if model_cache is not None:
        use_shared_model_cache(pod_spec, model_cache)

    for container in pod_spec.get("initContainers", []) + pod_spec["containers"]:
        container["image"] = image
        # Content-addressed tags are immutable, so a cached pull is correct
        # and saves minutes on multi-GB images.
        container["imagePullPolicy"] = "IfNotPresent"

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


def use_shared_model_cache(pod_spec: dict[str, Any], cache: SharedModelCache) -> None:
    """Point the Pod at the shared cache instead of downloading.

    Drops the guide's download init container, rebinds (or adds) the
    model-cache volume to the shared claim, mounts it at MODEL_CACHE_MOUNT on
    every remaining container, and sets HF_HOME there with HF_HUB_OFFLINE=1 so
    a missing model fails immediately instead of hanging on the network.
    """
    pod_spec["initContainers"] = [
        c for c in pod_spec.get("initContainers", []) if c["name"] != GUIDE_CACHE_INIT_CONTAINER
    ]
    if not pod_spec["initContainers"]:
        del pod_spec["initContainers"]

    volume = {
        "name": MODEL_CACHE_VOLUME,
        "persistentVolumeClaim": {"claimName": cache.pvc_name},
    }
    volumes = [v for v in pod_spec.get("volumes", []) if v["name"] != MODEL_CACHE_VOLUME]
    pod_spec["volumes"] = volumes + [volume]

    for container in pod_spec["containers"]:
        mounts = [
            m for m in container.get("volumeMounts", []) if m["name"] != MODEL_CACHE_VOLUME
        ]
        mounts.append({"name": MODEL_CACHE_VOLUME, "mountPath": MODEL_CACHE_MOUNT})
        container["volumeMounts"] = mounts
        set_env(container, "HF_HOME", MODEL_CACHE_MOUNT)
        set_env(container, "HF_HUB_OFFLINE", "1")


def set_env(container: dict[str, Any], name: str, value: str) -> None:
    env = [item for item in container.get("env", []) if item.get("name") != name]
    env.append({"name": name, "value": value})
    container["env"] = env


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
