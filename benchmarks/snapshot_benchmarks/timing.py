# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Read the original agent and application clocks, without substituting inference."""

import datetime
import json
import math
import re
from pathlib import Path


def seconds(value):
    units = {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}
    pieces = re.findall(r"([0-9]+(?:\.[0-9]+)?)(ms|us|µs|ns|h|m|s)", value)
    if not pieces or "".join(a + b for a, b in pieces) != value:
        raise ValueError(f"Invalid agent duration: {value!r}")
    return sum(float(a) * units[b] for a, b in pieces)


def throughput(payload_bytes, duration):
    """Effective decimal GB/s, not wire bytes or network-only transfer speed."""
    if payload_bytes is None or duration is None or duration <= 0 or not math.isfinite(duration):
        return None
    return payload_bytes / duration / 1e9


def restore_start_ns(log, pod, namespace):
    starts = []
    for line in log.splitlines():
        if "=== Starting external restore ===" not in line:
            continue
        record = json.loads(line[line.index("{") :])
        recorded_pod = record.get("pod", "")
        if "/" not in recorded_pod:
            recorded_pod = record.get("namespace", "") + "/" + recorded_pod
        if recorded_pod != namespace + "/" + pod:
            continue
        stamp = line.split("\t", 1)[0]
        match = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)\.(\d{1,9})Z", stamp)
        if not match:
            raise ValueError(f"Invalid restore timestamp: {stamp}")
        whole = int(datetime.datetime.fromisoformat(match[1] + "+00:00").timestamp())
        starts.append(whole * 10**9 + int(match[2].ljust(9, "0")))
    if len(starts) != 1:
        raise ValueError(f"Expected one restore start for {pod}, got {len(starts)}")
    return starts[0]


def restore_metrics(directory):
    directory = Path(directory)
    pod = json.loads((directory / "pod-final.json").read_text())["metadata"]
    start = restore_start_ns(
        (directory / "agent.log").read_text(), pod["name"], pod["namespace"]
    )
    events = [
        json.loads(line)
        for line in (directory / "phases.jsonl").read_text().splitlines()
    ]
    events = [event for event in events if event["wall_ns"] >= start]
    wakes = [e for e in events if e["phase"] == "wake" and e["event"] == "end"]
    if len(wakes) != 1 or not wakes[0]["success"]:
        raise ValueError("Missing successful application wake completion")
    for phase in ("wake", "resume_generation"):
        phase_events = [e for e in events if e["phase"] == phase]
        if not phase_events and phase == "resume_generation":
            continue  # TRT-LLM's wake call includes the resume operation.
        begins = [e for e in phase_events if e["event"] == "begin"]
        ends = [e for e in phase_events if e["event"] == "end"]
        if (
            len(begins) != 1
            or len(ends) != 1
            or begins[0]["wall_ns"] > ends[0]["wall_ns"]
        ):
            raise ValueError("Incomplete or ambiguous application phase: " + phase)
    completions = [
        e
        for e in events
        if e["phase"] in ("wake", "resume_generation") and e["event"] == "end"
    ]
    if not all(e["success"] for e in completions):
        raise ValueError("Application resume failed")
    end = max(e["wall_ns"] for e in completions)
    generation_starts = [
        e["wall_ns"]
        for e in events
        if e["phase"] == "generation" and e["event"] == "begin"
    ]
    if not generation_starts or not start <= end < min(generation_starts):
        raise ValueError("Wake endpoint must precede inference")
    timing = json.loads((directory / "timing.json").read_text())
    agent = seconds(timing["duration"])
    criu = seconds(timing["phases"]["criu_restore"])
    cuda = seconds(timing["phases"]["cuda_restore"])
    total = (end - start) / 1e9
    if total < agent or agent < criu + cuda:
        raise ValueError("Inconsistent clocks or agent phase accounting")
    staging = timing["phases"].get("pagebroker_stage")
    staging = seconds(staging) if staging is not None else None
    setup = agent - criu - cuda
    if staging is not None and staging > setup:
        raise ValueError("Staging exceeds agent setup time")
    return dict(
        staging_s=staging,
        other_setup_s=setup - staging if staging is not None else None,
        total_s=total,
        agent_s=agent,
        setup_s=agent - criu - cuda,
        criu_s=criu,
        cuda_s=cuda,
        wake_handoff_s=total - agent,
        wake_call_s=wakes[0]["elapsed_ns"] / 1e9,
        start_ns=start,
        wake_complete_ns=end,
    )
