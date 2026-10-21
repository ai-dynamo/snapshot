# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Parses the node agent's restore and checkpoint timing summary log lines.

The agent logs with zap's console encoder (agent/internal/logging/logging.go):
a plain-text preamble (RFC3339Nano timestamp, level, logger, caller, message)
followed by a single JSON object holding every structured field passed to that
log call. A real line looks like:

    2026-09-08T12:46:55Z  INFO  controller  executor/restore.go:249  \
    Restore timing summary  {"pod": "default/vllm-restored-...", \
    "snapshot": "vllm-snapshot", "restore": {"duration":"3.506021s", \
    "phases":{"criu_restore":"2.359102s","cuda_restore":"0.894113s", ...}, \
    "started_to_complete":"3.601s"}}

The preamble is not JSON, so the whole line cannot be `json.loads`-ed — but
everything from the first `{` to the end of the line is exactly one JSON
object, which this module parses directly rather than attempting to
reflection-decode the log line as a whole.
"""

from __future__ import annotations

import json
import re
from typing import Any

from snapshot_e2e.benchmark import parse_agent_timing_summary

from snapshot_benchmarks.schema import AgentLogPhases, CheckpointAgentLog

RESTORE_SUMMARY_MARKER = "Restore timing summary"
CHECKPOINT_SUMMARY_MARKER = "Checkpoint timing summary"

_GO_DURATION_TERM = re.compile(r"([0-9]+(?:\.[0-9]+)?)(ns|us|µs|ms|s|m|h)")
_GO_DURATION_UNIT_SECONDS = {
    "ns": 1e-9,
    "us": 1e-6,
    "µs": 1e-6,
    "ms": 1e-3,
    "s": 1.0,
    "m": 60.0,
    "h": 3600.0,
}


def parse_go_duration(value: str | None) -> float | None:
    """Parses a Go `time.Duration.String()` value (e.g. "3.506021s",
    "659.262378ms", "1m2.5s") into float seconds. Returns None for falsy/
    unparseable input rather than raising, since this is a best-effort
    diagnostic parse, never a required value.

    Matched terms must consume the entire string contiguously from the
    start -- `re.findall` alone would silently skip over any unmatched text
    between or around terms (e.g. "3.5s garbage 2ms" or "xx3s"), accumulating
    a bogus partial total instead of reporting the value as unparseable."""
    if not value:
        return None
    total = 0.0
    pos = 0
    for match in _GO_DURATION_TERM.finditer(value):
        if match.start() != pos:
            return None
        amount, unit = match.groups()
        total += float(amount) * _GO_DURATION_UNIT_SECONDS[unit]
        pos = match.end()
    if pos == 0 or pos != len(value):
        return None
    return total


def find_restore_summary_json(
    log_text: str, *, pod: str, snapshot: str
) -> dict[str, Any] | None:
    """Returns the parsed trailing JSON object of the last "Restore timing
    summary" line in `log_text` whose `pod` and `snapshot` fields both match
    the given values, or None if no such line is present.

    A node's agent log can carry restore summaries for other pods/snapshots
    entirely -- concurrent benchmark runs, or another tenant's workload on a
    shared cluster -- so a bare marker-text match would risk silently
    attributing someone else's phase timings to this run."""
    return _find_summary_json(
        log_text,
        marker=RESTORE_SUMMARY_MARKER,
        matches={"pod": pod, "snapshot": snapshot},
    )


def _find_summary_json(
    log_text: str, *, marker: str, matches: dict[str, str]
) -> dict[str, Any] | None:
    """Returns the last log line whose trailing JSON object matches every key."""
    summary_payload = None
    for line in log_text.splitlines():
        if marker not in line:
            continue
        brace_index = line.find("{")
        if brace_index == -1:
            continue
        try:
            candidate = json.loads(line[brace_index:])
        except json.JSONDecodeError:
            continue
        if isinstance(candidate, dict) and all(
            str(candidate.get(key)) == expected for key, expected in matches.items()
        ):
            summary_payload = candidate
    return summary_payload


def parse_agent_log_phases(
    log_text: str, *, log_source_pod: str | None, restore_pod: str, snapshot: str
) -> AgentLogPhases:
    """Parses the node agent's own restore log to recover the sub-second-
    precision phase breakdown for the restore of `restore_pod` (a
    "namespace/name" key, matching the agent's own log field) from
    `snapshot`. Best-effort: returns an `AgentLogPhases` with
    `log_line_found=False` and a warning if no matching line is present
    (e.g. the agent log rotated past it), never raises."""
    phases = AgentLogPhases(log_source_pod=log_source_pod)

    payload = find_restore_summary_json(log_text, pod=restore_pod, snapshot=snapshot)
    if payload is None:
        phases.parse_warnings.append(
            f'"{RESTORE_SUMMARY_MARKER}" line for pod={restore_pod!r} snapshot={snapshot!r} '
            "not found in the provided agent log"
        )
        return phases

    restore = payload.get("restore")
    if not isinstance(restore, dict):
        phases.parse_warnings.append(
            f'"{RESTORE_SUMMARY_MARKER}" line found but had no "restore" object'
        )
        return phases

    phases.log_line_found = True
    phases.duration = parse_go_duration(restore.get("duration"))
    phases.started_to_complete = parse_go_duration(restore.get("started_to_complete"))

    phase_map = restore.get("phases")
    if isinstance(phase_map, dict):
        for field_name in (
            "pagebroker_stage",
            "pagebroker_mount",
            "pagebroker_commit",
            "gpu_device_map",
            "overlay_capture",
            "criu_prepare",
            "criu_restore",
            "cuda_restore",
            "unaccounted",
        ):
            setattr(phases, field_name, parse_go_duration(phase_map.get(field_name)))
    else:
        phases.parse_warnings.append('"restore" object had no "phases" map')

    return phases


def parse_checkpoint_agent_log(
    log_text: str, *, log_source_pod: str | None, content: str
) -> CheckpointAgentLog:
    """Parses the capture-node agent's checkpoint summary for `content`.

    Best-effort: a missing line or an older agent that only logged phase
    totals becomes warnings on the returned object, never an exception.
    """
    parsed = CheckpointAgentLog(log_source_pod=log_source_pod)
    try:
        summary = parse_agent_timing_summary(
            log_text,
            message=CHECKPOINT_SUMMARY_MARKER,
            field="checkpoint",
            matches={"content": content},
        )
    except ValueError as exc:
        parsed.parse_warnings.append(str(exc))
        return parsed

    parsed.log_line_found = True
    parsed.duration = summary.duration_seconds
    parsed.started_to_complete = summary.started_to_complete_seconds
    parsed.phases = dict(summary.phases_seconds)
    nested = summary.nested_seconds
    parsed.cuda_lock = nested.get("cuda.lock")
    parsed.cuda_copy = nested.get("cuda.copy")
    parsed.cuda_job_file = nested.get("cuda.job_file")
    parsed.criu_freezing = nested.get("criu.freezing")
    parsed.criu_frozen = nested.get("criu.frozen")
    parsed.criu_memdump = nested.get("criu.memdump")
    parsed.criu_memwrite = nested.get("criu.memwrite")

    # The e2e parser keeps Go durations. Page and byte counters are integers
    # on the same payload, so they are read from that object afterwards.
    payload = _find_summary_json(
        log_text,
        marker=CHECKPOINT_SUMMARY_MARKER,
        matches={"content": content},
    )
    checkpoint = payload.get("checkpoint") if isinstance(payload, dict) else None
    if isinstance(checkpoint, dict):
        criu = checkpoint.get("criu")
        if isinstance(criu, dict):
            parsed.criu_pages_written = _int(criu.get("pages_written"))
            parsed.criu_pages_written_bytes = _int(criu.get("pages_written_bytes"))
            parsed.criu_pages_scanned = _int(criu.get("pages_scanned"))
            parsed.criu_image_bytes = _int(criu.get("image_bytes"))
            stats_error = criu.get("stats_error")
            if isinstance(stats_error, str) and stats_error:
                parsed.parse_warnings.append(f"criu stats unavailable: {stats_error}")
        overlay = checkpoint.get("overlay")
        if isinstance(overlay, dict):
            parsed.overlay_diff_bytes = _int(overlay.get("diff_bytes"))

    if parsed.criu_memwrite is None and not any(
        warning.startswith("criu stats unavailable:") for warning in parsed.parse_warnings
    ):
        parsed.parse_warnings.append(
            "checkpoint summary has no criu memwrite split; "
            "the agent build predates capture sub-phase instrumentation"
        )
    return parsed


def _int(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, float) and value.is_integer():
        return int(value)
    if isinstance(value, str) and value.isdigit():
        return int(value)
    return None
