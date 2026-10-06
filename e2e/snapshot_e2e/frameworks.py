# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inference framework workloads under e2e test.

Each framework's program and manifests live under docs/guides/<name>/ in the
repository root. This module pins what the tests need to know about each one:
which control-dir files it writes, how long each phase may take, and which
model it serves.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, replace
from pathlib import Path

import yaml

FRAMEWORKS_DIR = Path(__file__).resolve().parent.parent.parent / "docs" / "guides"

CONTAINER = "main"
API_PORT = 8000

REQUEST_TIMEOUT_SECONDS = 120

# Phase budgets: source covers image pull, model load, and warm-up generation
# (300s was exceeded mid-init on a cold 10-20 GB image pull); checkpoint
# covers the dump and upload; restore covers the agent restore plus resume
# and the first post-restore generation.
#
# test_frameworks.py waits on restore_timeout_seconds twice (the agent's
# RestoreRequested event, then restore success plus the traffic sentinel), so
# it counts double. The inner `timeout` around pytest in e2e-frameworks.yaml
# is the binding limit: it must exceed SOURCE_READY_TIMEOUT_SECONDS +
# CHECKPOINT_TIMEOUT_SECONDS + POD_DELETE_TIMEOUT_SECONDS +
# 2 * restore_timeout_seconds + 3 * REQUEST_TIMEOUT_SECONDS + 60s for source
# prompt rendering, or pytest is
# interrupted before the failure dump runs -- e.g. sglang's 600s override
# makes that 900+300+180+2*600+3*120+60 = 3000s (50 min), the largest of the three.
SOURCE_READY_TIMEOUT_SECONDS = 900
CHECKPOINT_TIMEOUT_SECONDS = 300
RESTORE_TIMEOUT_SECONDS = 300
POD_DELETE_TIMEOUT_SECONDS = 180


@dataclass(frozen=True)
class FrameworkSpec:
    name: str
    model: str
    # Written by the restored process once its API is listening; holds the
    # first post-restore generation.
    restore_ready_file: str
    # Written by the restored process if resuming or serving raised; holds the
    # traceback. The restored process keeps the dead source container's stdout,
    # so this file is the only place such a failure is visible.
    restore_error_file: str
    # Guide PVC manifest for a persistent model cache, if the guide uses one.
    model_cache_manifest: str | None = None
    # Overrides RESTORE_TIMEOUT_SECONDS for this framework's restore-condition
    # and restore-outcome waits.
    restore_timeout_seconds: int = RESTORE_TIMEOUT_SECONDS
    source_ready_timeout_seconds: int = SOURCE_READY_TIMEOUT_SECONDS
    checkpoint_timeout_seconds: int = CHECKPOINT_TIMEOUT_SECONDS
    recipe: str | None = None

    @property
    def case_name(self) -> str:
        return f"{self.name}-{self.recipe}" if self.recipe else self.name

    @property
    def manifest_dir(self) -> Path:
        return FRAMEWORKS_DIR / self.name

    @property
    def deployment_manifest(self) -> Path:
        return self.manifest_dir / "capture" / f"{self.recipe or 'qwen3-0.6b'}.yaml"

    @property
    def restore_deployment_manifest(self) -> Path:
        filename = "8-gpu.yaml" if self.recipe else "single-gpu.yaml"
        return self.manifest_dir / "restore" / filename

    @property
    def app_py(self) -> Path:
        return self.manifest_dir / "app.py"

    @property
    def app_configmap_name(self) -> str:
        # Matches the configMap.name this framework's capture manifest
        # references, including the multi-GPU variants.
        return f"{self.name}-app"

    @property
    def model_cache_manifest_path(self) -> Path | None:
        if self.model_cache_manifest is None:
            return None
        return self.manifest_dir / self.model_cache_manifest


FRAMEWORKS: dict[str, FrameworkSpec] = {
    "vllm": FrameworkSpec(
        name="vllm",
        model="Qwen/Qwen3-0.6B",
        restore_ready_file="/snapshot-control/vllm-restore-ready",
        restore_error_file="/snapshot-control/vllm-restore-error",
    ),
    "sglang": FrameworkSpec(
        name="sglang",
        model="Qwen/Qwen3-0.6B",
        restore_ready_file="/snapshot-control/sglang-restore-ready",
        restore_error_file="/snapshot-control/sglang-restore-error",
        model_cache_manifest="model-cache-pvc.yaml",
        # engine.resume_memory_occupation() has come within seconds of the
        # default 300s budget in CI; give it more room than the other
        # frameworks need.
        restore_timeout_seconds=600,
    ),
    "tensorrt-llm": FrameworkSpec(
        name="tensorrt-llm",
        model="Qwen/Qwen3-0.6B",
        restore_ready_file="/snapshot-control/trtllm-restore-ready",
        restore_error_file="/snapshot-control/trtllm-restore-error",
    ),
}


def framework_spec(name: str) -> FrameworkSpec:
    spec = FRAMEWORKS[name]
    recipe = os.environ.get("SNAPSHOT_E2E_RECIPE", "")
    if not recipe:
        return spec
    if recipe not in {"glm-5.3", "deepseek-v4.1-flash"}:
        raise ValueError(f"unknown SNAPSHOT_E2E_RECIPE: {recipe}")
    spec = replace(spec, recipe=recipe)
    if not spec.deployment_manifest.is_file():
        raise ValueError(f"{name} has no {recipe} recipe")
    with spec.deployment_manifest.open(encoding="utf-8") as handle:
        deployment = yaml.safe_load(handle)
    main = next(c for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == CONTAINER)
    env = {e["name"]: e["value"] for e in main["env"]}
    return replace(
        spec,
        model=env["SNAPSHOT_MODEL"],
        model_cache_manifest="../model-cache-pvc.yaml",
        source_ready_timeout_seconds=3600,
        checkpoint_timeout_seconds=1800,
        restore_timeout_seconds=1800,
    )


@dataclass(frozen=True)
class SharedModelCache:
    """An NFS export holding a Hugging Face cache in HF_HOME layout.

    CI clusters commonly block or throttle model downloads from the pods, and
    even where they work, pulling weights on every run is wasted minutes. With
    a shared cache the framework pods mount the export at MODEL_CACHE_MOUNT,
    point HF_HOME at it, and run offline; the guide's own per-deployment cache
    (SGLang's download init container and PVC) is replaced, not duplicated.
    """

    server: str
    path: str
    pvc_name: str

    @classmethod
    def from_env(cls) -> "SharedModelCache | None":
        server = os.environ.get("SNAPSHOT_E2E_MODEL_CACHE_SERVER", "").strip()
        path = os.environ.get("SNAPSHOT_E2E_MODEL_CACHE_PATH", "").strip()
        if not server and not path:
            return None
        if not (server and path):
            raise RuntimeError(
                "SNAPSHOT_E2E_MODEL_CACHE_SERVER and SNAPSHOT_E2E_MODEL_CACHE_PATH "
                "must be set together"
            )
        return cls(
            server=server,
            path=path,
            pvc_name=os.environ.get("SNAPSHOT_E2E_MODEL_CACHE_PVC", "model-cache"),
        )


MODEL_CACHE_MOUNT = "/models"
MODEL_CACHE_VOLUME = "model-cache"


def selected_frameworks() -> list[str]:
    """Frameworks to run, from SNAPSHOT_E2E_FRAMEWORK (comma-separated) or all."""
    raw = os.environ.get("SNAPSHOT_E2E_FRAMEWORK", "").strip()
    if not raw:
        return sorted(FRAMEWORKS)
    names = [name.strip() for name in raw.split(",") if name.strip()]
    unknown = sorted(set(names) - set(FRAMEWORKS))
    if unknown:
        raise RuntimeError(
            f"SNAPSHOT_E2E_FRAMEWORK names unknown frameworks {unknown}; "
            f"known: {sorted(FRAMEWORKS)}"
        )
    return names


def framework_image_overridden() -> bool:
    return bool(os.environ.get("SNAPSHOT_E2E_FRAMEWORK_IMAGE"))


def framework_image(spec: FrameworkSpec) -> str:
    """The workload image: an explicit override, else the guide's own image.

    SNAPSHOT_E2E_FRAMEWORK_IMAGE wins so a different image can be tested.
    Otherwise the image is read straight from the guide's own
    capture manifest -- one place to change it.
    """
    override = os.environ.get("SNAPSHOT_E2E_FRAMEWORK_IMAGE")
    if override:
        return override
    with spec.deployment_manifest.open(encoding="utf-8") as handle:
        deployment = yaml.safe_load(handle)
    return deployment["spec"]["template"]["spec"]["containers"][0]["image"]
