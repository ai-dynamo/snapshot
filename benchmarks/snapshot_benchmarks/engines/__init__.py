# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The engine interface `run.py` drives.

Each implementation loads the corresponding manual guide templates.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol


@dataclass
class ModelSpec:
    """One entry from models.yaml."""

    label: str
    hf_id_or_path: str
    reported_weights_bytes: int | None = None
    env: dict[str, str] | None = None
    tp: int = 1
    ep: int = 1
    engine_args: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self):
        if self.reported_weights_bytes is not None and self.reported_weights_bytes <= 0:
            raise ValueError("Reported weight size must be positive")
        if (
            type(self.tp) is not int
            or type(self.ep) is not int
            or self.tp < 1
            or self.ep < 1
            or self.tp % self.ep
        ):
            raise ValueError("TP and EP must be positive; EP must divide TP")
        reserved = {
            "model",
            "model_path",
            "tensor_parallel_size",
            "tp_size",
            "ep_size",
            "enable_expert_parallel",
            "moe_expert_parallel_size",
            "dp_size",
            "pp_size",
            "pipeline_parallel_size",
            "data_parallel_size",
        }
        if not isinstance(self.engine_args, dict) or reserved & self.engine_args.keys():
            raise ValueError(
                "Use the model and TP/EP flags, not engine-args, for topology"
            )

    @property
    def parallelism(self):
        if self.ep == 1:
            return f"TP{self.tp}"
        return f"TP{self.tp}/EP{self.ep}"


class Engine(Protocol):
    """A snapshot-ready inference framework, as built and deployed by the
    corresponding docs/guides/<engine>/ guide.

    Implementations load and patch the guide's own deployment YAML rather than
    hand-building pod specs, so the benchmark can't silently drift from the
    documented manual flow -- see engines/vllm.py.
    """

    name: str
    guide: str
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
        """Returns a Pod manifest equivalent to the guide's capture
        pod template, for the given model. `image_pull_policy` overrides the
        guide's own default. Pass `"IfNotPresent"` for an image that was built and imported
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
        restore manifest's pod template, restoring from `snapshot_name`."""
        ...
