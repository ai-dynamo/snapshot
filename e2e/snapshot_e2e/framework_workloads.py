# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Pods for the framework e2e tests, derived from the framework guide manifests.

docs/guides/<name>/ ships Deployments. The tests need plain Pods they can
name, label, pin to a node, and delete individually, so the Deployment's Pod
template is lifted into a Pod with the minimum of edits: test identity, the
image under test, e2e scheduling, and — for the restore pod — the PodSnapshot
to restore from. Everything that makes the Pod checkpointable or restorable
(control volume, probes, seccomp, device mounts, standby env) stays exactly as
the manifest wrote it, so a framework definition that would not work does not
pass here.
"""

from __future__ import annotations

import copy
import json
import os
import shlex
from pathlib import Path
from typing import Any

import yaml
from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.frameworks import CONTAINER
from snapshot_e2e.frameworks import MODEL_CACHE_MOUNT
from snapshot_e2e.frameworks import MODEL_CACHE_VOLUME
from snapshot_e2e.frameworks import FrameworkSpec
from snapshot_e2e.frameworks import SharedModelCache
from snapshot_e2e.frameworks import framework_image
from snapshot_e2e.frameworks import framework_image_overridden
from snapshot_e2e.workloads import TestRun
from snapshot_e2e.workloads import same_node_affinity
from snapshot_e2e.workloads import workload_image
from snapshot_e2e.workloads import workload_scheduling

RESTORE_FROM_ANNOTATION = "nvidia.com/restore-from"
SHARED_MEMORY_ANNOTATION = "nvidia.com/cuda-shared-memory-support"
# The guides' own cache plumbing, replaced when a shared cache is configured:
# the init container downloads into the guide PVC, which the shared export
# makes both unnecessary and impossible offline.
GUIDE_CACHE_INIT_CONTAINER = "model-cache"
MANIFESTS_DIR = Path(__file__).resolve().parent / "manifests"


# Run from the privileged agent: ordinary exec cannot dereference a restored,
# nondumpable process's root and can inhabit a different mount namespace.
CUINTERPOSE_LIBRARY_PROBE = r'''
import hashlib
import json
import sys
from pathlib import Path

libraries = None
for process in Path(sys.argv[1]).glob("[0-9]*"):
    try:
        if sys.argv[2] not in (process / "cgroup").read_text():
            continue
        args = (process / "cmdline").read_bytes().split(b"\0")
    except FileNotFoundError:
        continue
    if not args or args[0].rsplit(b"/", 1)[-1] not in {b"python", b"python3"}:
        continue
    if b"/snapshot-app/app.py" not in args[1:3]:
        continue
    current = {}
    for name in ("libcuinterpose.so", "libcuinterpose_core.so"):
        with (process / "root/tmp/snapshot-cuda" / name).open("rb") as library:
            current[name] = hashlib.sha256(library.read()).hexdigest()
    if libraries is not None and libraries != current:
        raise RuntimeError("guide processes have different libraries")
    libraries = current
if libraries is None:
    raise RuntimeError("no running /snapshot-app/app.py guide process")
print(json.dumps(libraries))
'''


def cuinterpose_library_hashes(config: k8s.E2EConfig, pod: client.V1Pod) -> dict[str, str]:
    """Require both delivered libraries to remain readable by the guide process."""
    container_id = next(
        status.container_id for status in pod.status.container_statuses
        if status.name == CONTAINER
    )
    runtime_id = (container_id or "").split("://", 1)[-1]
    if len(runtime_id) != 64 or any(char not in "0123456789abcdef" for char in runtime_id):
        raise AssertionError(f"missing or invalid guide container ID: {container_id!r}")
    agent = lifecycle.checkpoint_agent_pod(config, pod.spec.node_name)
    # The same host-tools seam used for crictl metadata. Host Python 3 is required.
    output = k8s.exec_payload(
        config.namespace, agent,
        f"timeout 30s nsenter -t 1 -m -r -w -- /usr/bin/python3 -c "
        f"{shlex.quote(CUINTERPOSE_LIBRARY_PROBE)} /proc {shlex.quote(runtime_id)}",
    )
    try:
        return json.loads(output)
    except json.JSONDecodeError as exc:
        raise AssertionError(
            f"cuinterpose library probe failed for {pod.metadata.name}: {output}"
        ) from exc


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
    # The test creates the PodSnapshot itself. Preserve workload activation and
    # other annotations, but do not treat the source as a restore destination.
    for annotation in (RESTORE_FROM_ANNOTATION, "nvidia.com/restore-container-map"):
        pod["metadata"]["annotations"].pop(annotation, None)
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
    # The guide names its own PodSnapshot, so the restore annotation must instead
    # identify this run's snapshot. Tests normally restore on the source node because
    # another destination requires shared checkpoint storage.
    pod["metadata"]["annotations"] = {RESTORE_FROM_ANNOTATION: run.snapshot_name}
    destination = os.environ.get("SNAPSHOT_E2E_RESTORE_NODE", source_node)
    if not destination:
        raise ValueError("SNAPSHOT_E2E_RESTORE_NODE must name a node")
    pod["spec"]["affinity"] = same_node_affinity(destination)
    return pod


def app_configmap(
    *,
    config: k8s.E2EConfig,
    spec: FrameworkSpec,
) -> dict[str, Any]:
    """The ConfigMap the guide's deployment.yaml mounts app.py from.

    Matches spec.app_configmap_name, the name the guide's own manifest
    references, and the same `kubectl create configmap --from-file=app.py`
    a user runs by hand -- read from the identical source file, so there is
    nothing to keep in sync by hand.
    """
    return {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": spec.app_configmap_name, "namespace": config.namespace},
        "data": {"app.py": spec.app_py.read_text(encoding="utf-8")},
    }


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

    Capacity, mount options, and the static binding (empty storageClassName +
    volumeName, Retain policy) live in the checked-in templates under
    manifests/ so they can be tuned without touching this script; only the
    per-run identity and NFS coordinates are filled in here.
    """
    pv = load_manifest(MANIFESTS_DIR / "shared-model-cache-pv.yaml")
    pv["metadata"]["name"] = cache.pvc_name
    pv["spec"]["nfs"] = {"server": cache.server, "path": cache.path}

    pvc = load_manifest(MANIFESTS_DIR / "shared-model-cache-pvc.yaml")
    pvc["metadata"]["name"] = cache.pvc_name
    pvc["metadata"]["namespace"] = config.namespace
    pvc["spec"]["volumeName"] = cache.pvc_name
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

    main = main_container({"spec": pod_spec})
    parallelism = os.environ.get("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE")
    if parallelism is not None:
        try:
            size = int(parallelism)
        except ValueError:
            raise ValueError("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE must be a positive integer") from None
        if size < 1:
            raise ValueError("SNAPSHOT_E2E_TENSOR_PARALLEL_SIZE must be a positive integer")
        set_env(main, "SNAPSHOT_TENSOR_PARALLEL_SIZE", str(size))
        resources = main.setdefault("resources", {})
        resources.setdefault("limits", {})["nvidia.com/gpu"] = str(size)
        if "nvidia.com/gpu" in resources.get("requests", {}):
            resources["requests"]["nvidia.com/gpu"] = str(size)

    # Content-addressed tags are immutable, so a cached pull is correct and
    # saves minutes on multi-GB images. An override (SNAPSHOT_E2E_FRAMEWORK_IMAGE)
    # is typically a mutable dev tag, where a cached image would test stale bits.
    pull_policy = "Always" if framework_image_overridden() else "IfNotPresent"
    original_image = main["image"]
    for container in pod_spec.get("initContainers", []) + pod_spec["containers"]:
        if container["image"] == "${SNAPSHOT_AGENT_IMAGE}":
            # Resolve only the guide's installer placeholder. Its image supplies
            # the same bundle as the installed Snapshot agent, not the engine.
            container["image"] = workload_image()
            container["imagePullPolicy"] = (
                "IfNotPresent" if "@sha256:" in container["image"] else "Always"
            )
        elif container["image"] == original_image:
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
