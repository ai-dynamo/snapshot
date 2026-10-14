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
class GpuIdentity:
    """The GPU/driver identity of a single pod+node, queried live (never
    hardcoded). Collected separately for the capture pod/node and the restore
    pod/node -- on a heterogeneous cluster these can genuinely differ, and
    conflating them would silently attribute restore timing to the wrong
    hardware."""

    gpu_product: str | None = None
    gpu_driver_version: str | None = None
    cuda_driver_major_label: str | None = None


@dataclass
class BenchmarkEnvironment:
    """Everything needed to judge whether two runs are comparable.

    Collected on every run regardless of hardware, so a run on, e.g., a single
    A10G is a clearly-labeled, self-describing result rather than a silent,
    invalid comparison against the B200/VAST numbers published in
    docs/development/benchmarks.md.

    `capture` is always populated once the source pod is Ready; `restore` is
    populated only for `mode="both"`, once the restore pod's node is known --
    see `run.py`. They are deliberately not collapsed into one set of fields:
    for a `different_node` restore (or any heterogeneous cluster), the GPU
    product/driver on the restore node can differ from the capture node, and a
    report reading a single flat `gpu_product` would silently attribute
    restore timing to the wrong hardware.
    """

    storage_class: str | None = None
    storage_provisioner: str | None = None
    k8s_version: str | None = None
    capture_node: str | None = None
    restore_node: str | None = None
    placement: str | None = None  # "same_node" | "different_node" | None
    capture: GpuIdentity = field(default_factory=GpuIdentity)
    restore: GpuIdentity | None = None


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
class ImageGrowthSample:
    """One stat of the CRIU image directory while the dump RPC was running."""

    offset_seconds: float
    bytes: int


@dataclass
class CheckpointAgentLog:
    """Capture breakdown from the agent's ``Checkpoint timing summary``.

    ``phases`` are the additive pieces of ``duration``. ``cuda_*`` splits
    ``phases["cuda_checkpoint"]`` into lock, device-to-host copy, and job-file
    refresh. ``criu_memdump`` and ``criu_memwrite`` are CRIU's own timers from
    ``stats-dump``. They overlap ``phases["criu_dump"]`` and are not added to
    it: memdump is time spent reading the frozen process, memwrite is time
    spent writing pages. CRIU can count the last write interval twice, so use
    the pair to see which side dominates rather than as a partition.

    ``image_growth`` is the agent's own sampling of image-directory size during
    the dump. A long stall with a flat size means the time is not a steady
    disk write.
    """

    log_line_found: bool = False
    log_source_pod: str | None = None
    parse_warnings: list[str] = field(default_factory=list)

    duration: float | None = None
    started_to_complete: float | None = None
    phases: dict[str, float] = field(default_factory=dict)

    cuda_lock: float | None = None
    cuda_copy: float | None = None
    cuda_job_file: float | None = None

    criu_freezing: float | None = None
    criu_frozen: float | None = None
    criu_memdump: float | None = None
    criu_memwrite: float | None = None
    criu_pages_written: int | None = None
    criu_pages_written_bytes: int | None = None
    criu_pages_scanned: int | None = None
    criu_image_bytes: int | None = None
    image_growth: list[ImageGrowthSample] = field(default_factory=list)
    overlay_diff_bytes: int | None = None

    @property
    def memwrite_gigabytes_per_second(self) -> float | None:
        """Page payload divided by CRIU's memwrite timer."""
        if not self.criu_pages_written_bytes or not self.criu_memwrite:
            return None
        return self.criu_pages_written_bytes / self.criu_memwrite / 1e9

    @property
    def criu_image_gigabytes_per_second(self) -> float | None:
        """Whole CRIU image divided by the agent's criu_dump wall time."""
        criu_dump = self.phases.get("criu_dump")
        if not self.criu_image_bytes or not criu_dump:
            return None
        return self.criu_image_bytes / criu_dump / 1e9

    @property
    def image_growth_max_stall_seconds(self) -> float | None:
        return _growth_max_stall(self.image_growth)

    @property
    def image_growth_midpoint_fraction(self) -> float | None:
        """Share of the final image present at the midpoint of the dump.

        Near 0.5 is a steady write. Near 0 means the bytes arrived late.
        """
        return _growth_midpoint_fraction(self.image_growth)


def _growth_max_stall(samples: list[ImageGrowthSample]) -> float | None:
    if len(samples) < 2:
        return None
    ordered = sorted(samples, key=lambda sample: sample.offset_seconds)
    best = 0.0
    running = 0.0
    for previous, current in zip(ordered, ordered[1:]):
        gap = current.offset_seconds - previous.offset_seconds
        if gap < 0:
            continue
        if current.bytes <= previous.bytes:
            running += gap
            if running > best:
                best = running
        else:
            running = 0.0
    return best


def _growth_midpoint_fraction(samples: list[ImageGrowthSample]) -> float | None:
    if len(samples) < 2:
        return None
    ordered = sorted(samples, key=lambda sample: sample.offset_seconds)
    final = ordered[-1]
    if final.bytes <= 0 or final.offset_seconds <= 0:
        return None
    half = final.offset_seconds / 2
    closest = min(ordered, key=lambda sample: abs(sample.offset_seconds - half))
    return closest.bytes / final.bytes


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

    `snapshot_restore_seconds` and `wake_and_copy_seconds` are the split the
    benchmark exists to report: the node agent's `nvidia.com/Restored` pod
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
    def wake_and_copy_seconds(self) -> float | None:
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
    `RestoreTiming.wake_and_copy_seconds` for that instead — it is
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
        if any(p is None for p in parts):
            return None
        return sum(parts)

    @property
    def criu_restore_approx(self) -> float | None:
        parts = [self.criu_prepare, self.criu_restore, self.overlay_capture]
        if any(p is None for p in parts):
            return None
        return sum(parts)

    @property
    def cuda_restore_approx(self) -> float | None:
        return self.cuda_restore

    @property
    def wake_remap_approx(self) -> None:
        return None


@dataclass
class ArtifactFile:
    """One file in the checkpoint artifact, relative to the content directory.

    `bytes` is the apparent size (`st_size`), which counts sparse holes.
    `allocated_bytes` is `st_blocks * 512`, the space the filesystem has
    actually reserved. A page image whose apparent size is far above its
    allocated size is mostly holes.
    """

    path: str
    bytes: int
    allocated_bytes: int | None = None


@dataclass
class ImageCategory:
    """Bytes in the artifact grouped by CRIU image kind (pages, core, mm, ...).

    `bytes` sums apparent sizes. `allocated_bytes` sums allocated sizes, or
    is None when the inventory did not report blocks.
    """

    name: str
    bytes: int
    file_count: int
    allocated_bytes: int | None = None


@dataclass
class DumpLogEvent:
    """One CRIU dump.log line. offset_seconds is CRIU's own clock."""

    offset_seconds: float
    message: str
    path: str


@dataclass
class CheckpointImage:
    """File inventory and dump.log markers for one captured artifact.

    Collected by listing the artifact before the benchmark deletes it. The
    category totals name bytes that CRIU's pages_written counter does not.
    dump.log offsets share CRIU's clock with the image-growth samples.
    """

    files: list[ArtifactFile] = field(default_factory=list)
    categories: list[ImageCategory] = field(default_factory=list)
    file_count: int = 0
    dump_log: list[DumpLogEvent] = field(default_factory=list)
    parse_warnings: list[str] = field(default_factory=list)
    # Summaries of the high-volume dump.log lines. Kept off the event list so
    # "Dumping finished" is not dropped behind hundreds of pagemap lines.
    pagemap_count: int = 0
    pagemap_pages: int = 0
    pagemap_holes: int = 0
    pagemap_first_seconds: float | None = None
    pagemap_last_seconds: float | None = None
    thread_core_count: int = 0
    thread_core_first_seconds: float | None = None
    thread_core_last_seconds: float | None = None

    @property
    def pages_bytes(self) -> int:
        return _category_bytes(self.categories, "pages")

    @property
    def pages_allocated_bytes(self) -> int | None:
        return _category_allocated(self.categories, "pages")

    @property
    def pages_sparse_bytes(self) -> int | None:
        """Apparent page-image size minus allocated size. Holes live here."""
        allocated = self.pages_allocated_bytes
        if allocated is None:
            return None
        return self.pages_bytes - allocated

    @property
    def pagemap_hole_bytes(self) -> int | None:
        """CRIU-reported holes, in bytes. None when no pagemap line was read."""
        if self.pagemap_count == 0:
            return None
        return self.pagemap_holes * 4096

    @property
    def non_page_bytes(self) -> int:
        return sum(category.bytes for category in self.categories if category.name != "pages")

    @property
    def first_dumping_pages_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Dumping pages (")

    @property
    def last_pagemap_seconds(self) -> float | None:
        if self.pagemap_last_seconds is not None:
            return self.pagemap_last_seconds
        return _last_event(self.dump_log, "Pagemap generated:")

    @property
    def first_dumping_core_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Dumping core (pid:")

    @property
    def dumping_mm_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Dumping mm (")

    @property
    def dumping_pstree_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Dumping pstree")

    @property
    def namespaces_complete_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Namespaces dump complete")

    @property
    def unfreezing_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Unfreezing tasks")

    @property
    def writing_stats_seconds(self) -> float | None:
        return _first_event(self.dump_log, "Writing stats")

    @property
    def dumping_finished_seconds(self) -> float | None:
        return _last_event(self.dump_log, "Dumping finished")


def _category_bytes(categories: list[ImageCategory], name: str) -> int:
    for category in categories:
        if category.name == name:
            return category.bytes
    return 0


def _category_allocated(categories: list[ImageCategory], name: str) -> int | None:
    for category in categories:
        if category.name == name:
            return category.allocated_bytes
    return None


def _first_event(events: list[DumpLogEvent], prefix: str) -> float | None:
    for event in events:
        if event.message.startswith(prefix):
            return event.offset_seconds
    return None


def _last_event(events: list[DumpLogEvent], prefix: str) -> float | None:
    found: float | None = None
    for event in events:
        if event.message.startswith(prefix):
            found = event.offset_seconds
    return found


@dataclass
class RunResult:
    schema_version: int
    run_id: str
    timestamp_utc: str
    mode: str  # "cold_start" | "both"
    environment: BenchmarkEnvironment
    engine: BenchmarkEngine
    model: ModelInfo
    git_sha: str | None = None
    cold_start: ColdStartTiming | None = None
    checkpoint: CheckpointTiming | None = None
    checkpoint_agent_log: CheckpointAgentLog | None = None
    checkpoint_image: CheckpointImage | None = None
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
