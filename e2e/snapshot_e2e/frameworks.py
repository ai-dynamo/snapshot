# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inference framework workloads under e2e test.

The workloads are the framework guides under docs/guides/<framework>/: their
programs, Dockerfiles, and manifests are used as-is, so the guides stay
regression-tested and there is one place to change a model or engine setting.
This module only pins what the tests need to know about each guide: which
control-dir files it writes, how long each phase may take, and which model it
serves.
"""

from __future__ import annotations

import importlib.util
import os
from dataclasses import dataclass
from pathlib import Path
from types import ModuleType

REPO_ROOT = Path(__file__).resolve().parents[2]
GUIDES_DIR = REPO_ROOT / "docs" / "guides"
IMAGE_TAG_SCRIPT = REPO_ROOT / "hack" / "framework-image-tag.py"

CONTAINER = "main"
API_PORT = 8000

# One small chat-style prompt is enough to prove the restored engine serves;
# the guide APIs cap generation length themselves.
PROMPT = "Reply with one short sentence confirming this restored worker can serve."
REQUEST_TIMEOUT_SECONDS = 120

# Phase budgets. The source budget covers image pull, model download or cache
# load, engine load, compilation/CUDA-graph capture, and the warm-up
# generation; the checkpoint budget covers the dump and artifact upload; the
# restore budget covers the agent restore plus the program's own resume and
# the first post-restore generation.
#
# The first run on a node pulls a 10-20 GB runtime image (observed: ~4 min on
# the CI cluster) before the engine even starts, and vLLM then compiles and
# captures CUDA graphs; 300s was exceeded with the engine still initializing.
# The CI step running the test must exceed the sum of these budgets plus
# REQUEST_TIMEOUT_SECONDS (about 33 min today), or GitHub kills pytest before
# the failure dump runs; see the test step in e2e-frameworks.yaml.
SOURCE_READY_TIMEOUT_SECONDS = 900
CHECKPOINT_TIMEOUT_SECONDS = 300
RESTORE_TIMEOUT_SECONDS = 300
POD_DELETE_TIMEOUT_SECONDS = 180


@dataclass(frozen=True)
class FrameworkSpec:
    name: str
    model: str
    # Written by the guide program after its pre-checkpoint generation; holds
    # the generated text. Proves the engine served before capture.
    precheck_file: str
    # Written by the restored process once its API is listening; holds the
    # first post-restore generation.
    restore_ready_file: str
    # Written by the restored process if resuming or serving raised; holds the
    # traceback. The restored process keeps the dead source container's stdout,
    # so this file is the only place such a failure is visible.
    restore_error_file: str
    # Guide PVC manifest for a persistent model cache, if the guide uses one.
    model_cache_manifest: str | None = None

    @property
    def guide_dir(self) -> Path:
        return GUIDES_DIR / self.name

    @property
    def deployment_manifest(self) -> Path:
        return self.guide_dir / "deployment.yaml"

    @property
    def restore_deployment_manifest(self) -> Path:
        return self.guide_dir / "restore-deployment.yaml"

    @property
    def model_cache_manifest_path(self) -> Path | None:
        if self.model_cache_manifest is None:
            return None
        return self.guide_dir / self.model_cache_manifest


FRAMEWORKS: dict[str, FrameworkSpec] = {
    "vllm": FrameworkSpec(
        name="vllm",
        model="Qwen/Qwen3-0.6B",
        precheck_file="/snapshot-control/vllm-precheck",
        restore_ready_file="/snapshot-control/vllm-restore-ready",
        restore_error_file="/snapshot-control/vllm-restore-error",
    ),
    "sglang": FrameworkSpec(
        name="sglang",
        model="Qwen/Qwen3-0.6B",
        precheck_file="/snapshot-control/sglang-precheck",
        restore_ready_file="/snapshot-control/sglang-restore-ready",
        restore_error_file="/snapshot-control/sglang-restore-error",
        model_cache_manifest="model-cache-pvc.yaml",
    ),
    "tensorrt-llm": FrameworkSpec(
        name="tensorrt-llm",
        model="Qwen/Qwen3-0.6B",
        precheck_file="/snapshot-control/trtllm-precheck",
        restore_ready_file="/snapshot-control/trtllm-restore-ready",
        restore_error_file="/snapshot-control/trtllm-restore-error",
    ),
}


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
    """The workload image: an explicit override, else the published guide image.

    SNAPSHOT_E2E_FRAMEWORK_IMAGE wins so a locally built image can be tested.
    Otherwise the tag is the content digest of the guide's image inputs, which
    is what the E2E Framework Images workflow publishes.
    """
    override = os.environ.get("SNAPSHOT_E2E_FRAMEWORK_IMAGE")
    if override:
        return override
    image, _ = image_tag_module().image_ref(spec.name)
    return image


def image_tag_module() -> ModuleType:
    # hack/ is not a package; load the script so the digest logic has exactly
    # one implementation shared by the workflow and the tests.
    module_spec = importlib.util.spec_from_file_location(
        "framework_image_tag", IMAGE_TAG_SCRIPT
    )
    if module_spec is None or module_spec.loader is None:
        raise RuntimeError(f"cannot load {IMAGE_TAG_SCRIPT}")
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)
    return module
