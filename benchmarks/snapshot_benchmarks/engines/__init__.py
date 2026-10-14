# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The engine interface `run.py` drives.

Only `vllm` is implemented today ("the agreed day-one vLLM comparison"). This
module exists so SGLang and TensorRT-LLM -- which already have parallel guides
under docs/guides/sglang/ and docs/guides/tensorrt-llm/ -- can be added later
by implementing this same `Engine` protocol, without reworking `run.py`.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol


@dataclass
class ModelSpec:
    """One entry from models.yaml."""

    label: str
    hf_id_or_path: str
    reported_weights_bytes: int | None = None
    env: dict[str, str] | None = None


class Engine(Protocol):
    """A snapshot-ready inference framework, as built and deployed by the
    corresponding docs/guides/<engine>/ guide.

    Implementations load and patch the guide's own deployment YAML rather than
    hand-building pod specs, so the benchmark can't silently drift from the
    documented manual flow -- see engines/vllm.py.
    """

    name: str
    container_name: str
    version_probe_command: str

    def build_source_pod(
        self,
        *,
        name: str,
        namespace: str,
        image: str,
        model: ModelSpec,
        image_pull_policy: str | None = None,
        tolerations: list[dict[str, str]] | None = None,
    ) -> dict[str, Any]:
        """Returns a Pod manifest equivalent to the guide's `deployment.yaml`
        pod template, for the given model. `image_pull_policy` overrides the
        guide's own default (`Always`, which assumes a real registry push) --
        pass `"IfNotPresent"` for an image that was built and imported
        directly into the node's container runtime with no registry.
        `tolerations` are appended to whatever the guide's own pod spec
        already declares (empty by default) -- needed on clusters whose GPU
        nodes carry a scheduling taint the guide doesn't know about."""
        ...

    def build_restore_pod(
        self,
        *,
        name: str,
        namespace: str,
        image: str,
        model: ModelSpec,
        snapshot_name: str,
        image_pull_policy: str | None = None,
        tolerations: list[dict[str, str]] | None = None,
    ) -> dict[str, Any]:
        """Returns a Pod manifest equivalent to the guide's
        `restore-deployment.yaml` pod template, restoring from `snapshot_name`."""
        ...
