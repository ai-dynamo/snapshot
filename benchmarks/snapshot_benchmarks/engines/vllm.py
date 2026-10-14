# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The vLLM engine: loads and patches docs/guides/vllm/{deployment,
restore-deployment}.yaml directly, so the benchmark deploys exactly the pod
template the manual guide documents -- only `image`, `SNAPSHOT_MODEL`, the run
name/namespace, and (for restore) the `nvidia.com/restore-from` annotation are
patched. This is deliberate: the guide's own YAML stays the single source of
truth for what gets deployed, so the benchmark can't silently drift from the
documented flow.

Deployed as bare Pods, not the Deployments the YAML files declare -- the
`spec.template` is what's actually reused; wrapping it in a Deployment adds
ReplicaSet churn and indirection for no benefit to a single timed run, and a
directly-created Pod has an immediate, deterministic name and UID (needed for
`PodSnapshot.spec.source.podRef.uid`).
"""

from __future__ import annotations

import copy
from pathlib import Path
from typing import Any

import yaml

from snapshot_benchmarks.engines import ModelSpec

_REPO_ROOT = Path(__file__).resolve().parents[3]
SOURCE_DEPLOYMENT_PATH = _REPO_ROOT / "docs/guides/vllm/deployment.yaml"
RESTORE_DEPLOYMENT_PATH = _REPO_ROOT / "docs/guides/vllm/restore-deployment.yaml"

CONTAINER_NAME = "main"
VERSION_PROBE_COMMAND = 'python3 -c "import vllm; print(vllm.__version__)"'

RESTORE_FROM_ANNOTATION = "nvidia.com/restore-from"
SNAPSHOT_MODEL_ENV = "SNAPSHOT_MODEL"


def _load_pod_template(path: Path) -> tuple[dict[str, Any], dict[str, Any]]:
    """Returns (template metadata, template spec) from a Deployment YAML's
    `spec.template`."""
    with path.open() as fh:
        deployment = yaml.safe_load(fh)
    template = deployment["spec"]["template"]
    return template.get("metadata", {}), template["spec"]


def _set_env(container: dict[str, Any], name: str, value: str) -> None:
    env = container.setdefault("env", [])
    for entry in env:
        if entry.get("name") == name:
            entry["value"] = value
            return
    env.append({"name": name, "value": value})


def _patch_container(
    pod_spec: dict[str, Any],
    *,
    image: str,
    model: ModelSpec,
    image_pull_policy: str | None,
    tolerations: list[dict[str, str]] | None = None,
) -> None:
    containers = [c for c in pod_spec["containers"] if c["name"] == CONTAINER_NAME]
    if len(containers) != 1:
        raise AssertionError(
            f"expected exactly one {CONTAINER_NAME!r} container, found {len(containers)}"
        )
    container = containers[0]
    container["image"] = image
    if image_pull_policy is not None:
        container["imagePullPolicy"] = image_pull_policy
    for env_name, env_value in (model.env or {}).items():
        _set_env(container, env_name, env_value)
    # Set last so a conflicting `model.env` entry can never make the pod load
    # a different model than the one `RunResult.model.hf_id_or_path` records.
    _set_env(container, SNAPSHOT_MODEL_ENV, model.hf_id_or_path)
    if tolerations:
        pod_spec.setdefault("tolerations", []).extend(tolerations)


def build_source_pod(
    *,
    name: str,
    namespace: str,
    image: str,
    model: ModelSpec,
    image_pull_policy: str | None = None,
    tolerations: list[dict[str, str]] | None = None,
) -> dict[str, Any]:
    template_metadata, pod_spec = _load_pod_template(SOURCE_DEPLOYMENT_PATH)
    pod_spec = copy.deepcopy(pod_spec)
    _patch_container(
        pod_spec,
        image=image,
        model=model,
        image_pull_policy=image_pull_policy,
        tolerations=tolerations,
    )
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": dict(template_metadata.get("labels", {})),
        },
        "spec": pod_spec,
    }


def build_restore_pod(
    *,
    name: str,
    namespace: str,
    image: str,
    model: ModelSpec,
    snapshot_name: str,
    image_pull_policy: str | None = None,
    tolerations: list[dict[str, str]] | None = None,
) -> dict[str, Any]:
    template_metadata, pod_spec = _load_pod_template(RESTORE_DEPLOYMENT_PATH)
    pod_spec = copy.deepcopy(pod_spec)
    _patch_container(
        pod_spec,
        image=image,
        model=model,
        image_pull_policy=image_pull_policy,
        tolerations=tolerations,
    )
    annotations = dict(template_metadata.get("annotations", {}))
    annotations[RESTORE_FROM_ANNOTATION] = snapshot_name
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": dict(template_metadata.get("labels", {})),
            "annotations": annotations,
        },
        "spec": pod_spec,
    }


class VLLMEngine:
    name = "vllm"
    container_name = CONTAINER_NAME
    version_probe_command = VERSION_PROBE_COMMAND

    build_source_pod = staticmethod(build_source_pod)
    build_restore_pod = staticmethod(build_restore_pod)
