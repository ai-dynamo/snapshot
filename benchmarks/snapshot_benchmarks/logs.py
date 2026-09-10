# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Parses the node agent's "Restore timing summary" log line.

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

from snapshot_benchmarks.schema import AgentLogPhases

RESTORE_SUMMARY_MARKER = "Restore timing summary"

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
    diagnostic parse, never a required value."""
    if not value:
        return None
    matches = _GO_DURATION_TERM.findall(value)
    if not matches:
        return None
    total = 0.0
    for amount, unit in matches:
        total += float(amount) * _GO_DURATION_UNIT_SECONDS[unit]
    return total


def find_restore_summary_json(log_text: str) -> dict[str, Any] | None:
    """Returns the parsed trailing JSON object of the last "Restore timing
    summary" line in `log_text`, or None if no such line is present."""
    summary_line = None
    for line in log_text.splitlines():
        if RESTORE_SUMMARY_MARKER in line:
            summary_line = line
    if summary_line is None:
        return None

    brace_index = summary_line.find("{")
    if brace_index == -1:
        return None
    try:
        return json.loads(summary_line[brace_index:])
    except json.JSONDecodeError:
        return None


def parse_agent_log_phases(log_text: str, *, log_source_pod: str | None) -> AgentLogPhases:
    """Parses the node agent's own restore log to recover the sub-second-
    precision phase breakdown. Best-effort: returns an `AgentLogPhases` with
    `log_line_found=False` and a warning if the line is missing or malformed
    (e.g. the agent log rotated past it), never raises."""
    phases = AgentLogPhases(log_source_pod=log_source_pod)

    payload = find_restore_summary_json(log_text)
    if payload is None:
        phases.parse_warnings.append(
            f'"{RESTORE_SUMMARY_MARKER}" line not found in the provided agent log'
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
