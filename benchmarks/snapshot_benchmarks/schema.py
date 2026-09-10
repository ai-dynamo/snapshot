# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Data model for one benchmark run.

A `RunResult` is the raw, reviewable output of a single `run` invocation (one
model, one engine, cold start and/or restore). It is written verbatim to disk
as JSON by `results.py` and is the only input `report.py` reads back.

Every timing field that comes from a Kubernetes object (pod creation, a
condition's `lastTransitionTime`, a container's `started_at`) is stored as the
`datetime` the API server reported, not a wall-clock timestamp captured by the
polling loop — this makes every derived duration a difference between two
object-owned timestamps, not an artifact of how often the benchmark polled.
"""

from __future__ import annotations

import dataclasses
import datetime
from dataclasses import dataclass, field
from typing import Any


def _iso(value: datetime.datetime | None) -> str | None:
    return value.isoformat() if value is not None else None


def _seconds(end: datetime.datetime | None, start: datetime.datetime | None) -> float | None:
    """Returns (end - start) in seconds, or None if either side is missing."""
    if end is None or start is None:
        return None
    return (end - start).total_seconds()


@dataclass
class BenchmarkEnvironment:
    """Everything needed to judge whether two runs are comparable.

    Collected on every run regardless of hardware, so a run on, e.g., a single
    A10G is a clearly-labeled, self-describing result rather than a silent,
    invalid comparison against the B200/VAST numbers published in
    docs/development/benchmarks.md.
    """

    gpu_product: str | None = None
    gpu_driver_version: str | None = None
    cuda_driver_major_label: str | None = None
    storage_class: str | None = None
    storage_provisioner: str | None = None
    k8s_version: str | None = None
    capture_node: str | None = None
    restore_node: str | None = None
    placement: str | None = None  # "same_node" | "different_node" | None


@dataclass
class BenchmarkEngine:
    name: str = "vllm"
    version: str | None = None  # queried live from the running pod, never hardcoded


@dataclass
class ModelInfo:
    label: str
    hf_id_or_path: str
    reported_weights_bytes: int | None = None  # informational, from models.yaml
    checkpoint_artifact_bytes: int | None = None  # measured via `du -sb` on the node


@dataclass
class ColdStartTiming:
    pod_created_at: datetime.datetime | None = None
    container_started_at: datetime.datetime | None = None
    ready_at: datetime.datetime | None = None

    @property
    def container_start_seconds(self) -> float | None:
        return _seconds(self.container_started_at, self.pod_created_at)

    @property
    def cold_start_total_seconds(self) -> float | None:
        return _seconds(self.ready_at, self.pod_created_at)

    @property
    def cold_start_excl_container_seconds(self) -> float | None:
        """Matches the published doc's "Cold start" column, which excludes
        container pull/start."""
        return _seconds(self.ready_at, self.container_started_at)


@dataclass
class CheckpointTiming:
    """Checkpoint timing boundaries.

    `t0` is `podsnapshot_created_at` (the `PodSnapshot` CR's own creation
    timestamp), and `ready_at` is that same object's `Ready` condition
    `lastTransitionTime` -- the moment the node agent finishes capturing the
    checkpoint (CRIU dump + rootfs diff). Both come from the Kubernetes API
    server, at one-second resolution, same as `RestoreTiming`.
    """

    podsnapshot_created_at: datetime.datetime | None = None
    ready_at: datetime.datetime | None = None

    @property
    def checkpoint_seconds(self) -> float | None:
        return _seconds(self.ready_at, self.podsnapshot_created_at)


@dataclass
class RestoreTiming:
    """Restore timing boundaries.

    `t0` for every derived duration is `restore_container_started_at`, not pod
    creation: the published doc's restore clock "begins once the container is
    already running, because CRIU injects the restored process into a
    container that has already started" (docs/development/benchmarks.md).

    `snapshot_restore_seconds` and `vllm_wake_and_copy_seconds` are the split
    the benchmark exists to report: the node agent's `nvidia.com/Restored` pod
    condition fires the moment Snapshot itself believes the restore is done —
    before the workload's own wake_up()/resume_generation()/warmup sequence in
    app.py runs. Everything before that condition is Snapshot's own work;
    everything after it, up to pod Ready, is the engine's own wake and
    copy-to-GPU work.

    These are derived from Kubernetes condition timestamps, which have
    one-second resolution — see `restore_total_seconds_precise` for a
    sub-second alternative headline number.
    """

    restore_pod_created_at: datetime.datetime | None = None
    restore_container_started_at: datetime.datetime | None = None
    restored_condition_at: datetime.datetime | None = None
    restore_ready_at: datetime.datetime | None = None

    @property
    def snapshot_restore_seconds(self) -> float | None:
        return _seconds(self.restored_condition_at, self.restore_container_started_at)

    @property
    def vllm_wake_and_copy_seconds(self) -> float | None:
        return _seconds(self.restore_ready_at, self.restored_condition_at)

    @property
    def restore_total_seconds(self) -> float | None:
        return _seconds(self.restore_ready_at, self.restore_container_started_at)


@dataclass
class AgentLogPhases:
    """Best-effort phase breakdown parsed from the node agent's "Restore
    timing summary" log line.

    The agent log is the source of the sub-second-precision totals in the
    published doc — `duration` here is the headline, high-precision restore
    total. The `*_approx` fields remap the agent's own phase names to the
    published doc's 4-stage vocabulary; the mapping is inexact (see module
    docstring in logs.py) and `wake_remap_approx` is intentionally left None:
    the published "wake / remap" stage is not represented in the agent log at
    all (inet-remap happens inside `criu_restore`'s wall time; the workload's
    own wake-up is outside the agent process entirely). Use
    `RestoreTiming.vllm_wake_and_copy_seconds` for that instead — it is
    reported alongside this, never folded into it.
    """

    log_line_found: bool = False
    log_source_pod: str | None = None
    parse_warnings: list[str] = field(default_factory=list)

    duration: float | None = None
    started_to_complete: float | None = None
    pagebroker_stage: float | None = None
    pagebroker_mount: float | None = None
    pagebroker_commit: float | None = None
    gpu_device_map: float | None = None
    overlay_capture: float | None = None
    criu_prepare: float | None = None
    criu_restore: float | None = None
    cuda_restore: float | None = None
    unaccounted: float | None = None

    @property
    def agent_setup_approx(self) -> float | None:
        parts = [self.gpu_device_map, self.pagebroker_stage, self.pagebroker_mount, self.pagebroker_commit]
        present = [p for p in parts if p is not None]
        return sum(present) if present else None

    @property
    def criu_restore_approx(self) -> float | None:
        parts = [self.criu_prepare, self.criu_restore, self.overlay_capture]
        present = [p for p in parts if p is not None]
        return sum(present) if present else None

    @property
    def cuda_restore_approx(self) -> float | None:
        return self.cuda_restore

    @property
    def wake_remap_approx(self) -> None:
        return None


@dataclass
class RunResult:
    schema_version: int
    run_id: str
    timestamp_utc: str
    mode: str  # "cold_start" | "restore" | "both"
    environment: BenchmarkEnvironment
    engine: BenchmarkEngine
    model: ModelInfo
    git_sha: str | None = None
    cold_start: ColdStartTiming | None = None
    checkpoint: CheckpointTiming | None = None
    restore: RestoreTiming | None = None
    agent_log_phases: AgentLogPhases | None = None
    warnings: list[str] = field(default_factory=list)

    def to_json_dict(self) -> dict[str, Any]:
        """Serializes to a plain JSON-able dict, including the derived
        properties (`*_seconds`, `*_approx`) alongside the raw fields, so a
        reader of the raw file never has to recompute them."""
        return _to_json_value(self)


def _to_json_value(value: Any) -> Any:
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        result: dict[str, Any] = {}
        for f in dataclasses.fields(value):
            result[f.name] = _to_json_value(getattr(value, f.name))
        for name, prop in type(value).__dict__.items():
            if isinstance(prop, property):
                result[name] = _to_json_value(prop.fget(value))
        return result
    if isinstance(value, datetime.datetime):
        return _iso(value)
    if isinstance(value, list):
        return [_to_json_value(item) for item in value]
    if isinstance(value, dict):
        return {key: _to_json_value(item) for key, item in value.items()}
    return value


SCHEMA_VERSION = 1
