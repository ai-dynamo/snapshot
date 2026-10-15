# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Recompute resource evidence and idle trends from bounded saved streams."""

from collections import Counter, deque
import math
from itertools import islice
from pathlib import Path
import statistics

from stress_config import RESOURCE_METRICS
from stress_storage import read_json, strict_json


IDLE_PHASES = {"idle", "warmup-idle", "final-idle"}


class IncompleteEvidence(ValueError):
    pass


def records(path, limit=64 * 1024):
    with path.open("rb") as source:
        while line := source.readline(limit + 1):
            if len(line) > limit or not line.endswith(b"\n"):
                raise ValueError("oversized or incomplete evidence record")
            yield strict_json(line)


def metrics(value):
    if set(value) != RESOURCE_METRICS or any(type(number) is not int or number < 0 for number in value.values()):
        raise ValueError("resource metrics are missing, negative or not integers")


def medians(values):
    return {name: statistics.median(item[name] for item in values) for name in RESOURCE_METRICS}


def distribution(points):
    result = {}
    for name in sorted(RESOURCE_METRICS):
        values = [value[name] for _, value in points]
        count, mean_t, mean_y, variance, covariance = 0, 0.0, 0.0, 0.0, 0.0
        for when, value in points:
            count += 1
            dt, dy = when - mean_t, value[name] - mean_y
            mean_t += dt / count
            mean_y += dy / count
            variance += dt * (when - mean_t)
            covariance += dt * (value[name] - mean_y)
        result[name] = {"min": min(values), "median": statistics.median(values), "max": max(values),
                        "slopePerHour": covariance / variance * 3600 if variance else None}
    return result


def analyze_resources(root, spec, summary, pulse=lambda: None):
    options = spec.evidence
    metadata = read_json(root, "resource-metadata.json")
    if metadata.get("runId") != spec.run_id or metadata.get("mode") != spec.mode:
        raise ValueError("resource metadata belongs to another run")
    if metadata.get("status") == "ERROR":
        raise ValueError("resource sampler reported an error")
    if metadata.get("status") != "SUCCESS":
        raise IncompleteEvidence("resource sampling did not reach a terminal report")
    if not metadata.get("stagingIsMountPoint"):
        raise ValueError("staging was not sampled on a dedicated mount")
    identity = metadata["broker"]
    if metadata.get("schemaVersion") != 1 or metadata.get("intervalSeconds") != options.interval:
        raise ValueError("resource metadata schema or sampling interval differs from the run specification")
    idle_records = iter(records(Path(root) / "idle-windows.jsonl"))
    group, tail, group_first, group_last, group_count = None, deque(maxlen=10), None, None, 0
    windows, aggregates, phases = [], {}, Counter()
    previous_time, first_time, last_time, count, gaps = None, None, None, 0, 0
    baseline_count = final_count = 0
    measured_start = summary.get("measuredStarted")
    if not isinstance(measured_start, (float, int)) or not math.isfinite(measured_start):
        raise IncompleteEvidence("measurement start was not recorded")
    measured_end = measured_start + spec.timing.measured
    measured_first = measured_last = None
    optional_errors = 0
    cgroup_coverage, cgroup_current, cgroup_last = Counter(), {}, None

    def finish_group():
        if group is None or group[0] not in IDLE_PHASES:
            return
        try:
            window = next(idle_records)
        except StopIteration:
            raise IncompleteEvidence("idle-window journal is missing a completed observation") from None
        if len(windows) >= 4096:
            raise ValueError("idle-window analysis capacity exceeded")
        if (window.get("phase"), window.get("batch")) != group or window.get("runId") != spec.run_id:
            raise ValueError("idle-window journal is reordered or belongs to another batch")
        if (window["firstSequence"], window["lastSequence"], window["samples"]) != (
                group_first["sequence"], group_last["sequence"], group_count):
            raise ValueError("idle-window sample bounds disagree with raw evidence")
        started, finished = window["started"], window["finished"]
        if not (math.isfinite(started) and math.isfinite(finished)
                and started <= group_first["monotonic"] <= group_last["monotonic"] <= finished):
            raise ValueError("invalid idle-window time bounds")
        valid = group_count >= options.idle_samples and finished - started >= (options.idle_samples - 1) * options.interval
        expected = medians(tail) if valid else None
        if window.get("medians") != expected or window.get("status") != ("VALID" if valid else "INSUFFICIENT"):
            raise ValueError("idle-window medians disagree with the last ten raw samples")
        windows.append(window)

    chunks = sorted(islice((Path(root) / "resources").glob("*.jsonl"), options.max_files + 1))
    if not chunks or len(chunks) > options.max_files:
        raise IncompleteEvidence("resource chunks are missing")
    for index, path in enumerate(chunks, 1):
        if path.name != f"{index:06d}.jsonl" or path.is_symlink() or path.stat().st_size > options.chunk_bytes:
            raise ValueError("resource chunks are missing, reordered, linked or oversized")
        for record in records(path):
            count += 1
            if count % 128 == 0:
                pulse()
            when = record["monotonic"]
            if (type(record.get("sequence")) is not int or record.get("sequence") != count
                    or isinstance(when, bool) or not isinstance(when, (int, float)) or not math.isfinite(when)
                    or (previous_time is not None and when < previous_time)):
                raise ValueError("resource samples are missing or reordered")
            if record.get("schemaVersion") != 1 or abs(record["elapsedSeconds"] - (when - metadata["monotonicStarted"])) > 1e-6:
                raise ValueError("resource schema or elapsed time disagrees with its fixed origin")
            if (record.get("runId"), record.get("mode"), record.get("pid"), record.get("processStartTicks")) != (
                    spec.run_id, spec.mode, identity["pid"], identity["startTicks"]):
                raise ValueError("resource samples changed broker identity or backend")
            if record.get("status") != "SUCCESS":
                raise ValueError("raw resource evidence contains a sampler failure")
            metrics(record["metrics"])
            if previous_time is not None and when - previous_time > 3 * options.interval:
                gaps += 1
            first_time = when if first_time is None else first_time
            last_time, previous_time = when, when
            if measured_start <= when <= measured_end:
                measured_first = when if measured_first is None else measured_first
                measured_last = when
            phase = record["phase"]
            phases[phase] += 1
            baseline_count += phase == "protocol-ready"
            final_count += phase == "final-sample"
            if phase not in {"cold-start", "startup", "protocol-ready", "warmup-wait", "warmup", "warmup-idle",
                             "initial-reference", "measured-wait", "measured", "drain", "cleanup", "idle", "final-idle", "final-sample"}:
                raise ValueError("unknown resource phase")
            if type(record["batch"]) is not int or record["batch"] < 0:
                raise ValueError("invalid resource batch number")
            if phase == "protocol-ready" and [record["metrics"][name] for name in (
                    "stagingFilesystemUsedBytes", "stagingFilesystemUsedInodes")] != metadata["stagingBaseline"]:
                raise ValueError("protocol-ready staging sample disagrees with the fixed baseline")
            if phase in IDLE_PHASES and [record["metrics"][name] for name in (
                    "stagingFilesystemUsedBytes", "stagingFilesystemUsedInodes")] != metadata["stagingBaseline"]:
                raise ValueError("staging did not return to its fixed baseline between batches")
            cgroup = record.get("cgroup", {})
            cgroup_coverage[cgroup.get("status")] += 1
            if cgroup.get("status") == "VERIFIED":
                if cgroup.get("brokerPid") != identity["pid"] or cgroup.get("runnerExcluded") is not True:
                    raise ValueError("cgroup counters are not attributed to the broker")
                if type(cgroup.get("currentBytes")) is not int or cgroup["currentBytes"] < 0:
                    raise ValueError("invalid cgroup memory counter")
                numbers = [*cgroup["breakdown"].values(), *cgroup["events"].values()]
                if cgroup["limitBytes"] is not None:
                    numbers.append(cgroup["limitBytes"])
                if any(type(number) is not int or number < 0 for number in numbers):
                    raise ValueError("invalid attributed cgroup breakdown/limits")
                value = cgroup["currentBytes"]
                if not cgroup_current:
                    cgroup_current.update(first=value, peak=value)
                cgroup_current.update(last=value, peak=max(cgroup_current["peak"], value))
                cgroup_last = cgroup
            elif cgroup.get("status") not in ("UNSUPPORTED", "ERROR"):
                raise ValueError("optional cgroup coverage was not described")
            optional_errors += cgroup.get("status") == "ERROR"
            for name, value in record["metrics"].items():
                stat = aggregates.setdefault(name, {"first": value, "last": value, "peak": value, "delta": 0})
                stat.update(last=value, peak=max(stat["peak"], value), delta=value - stat["first"])
            key = (phase, record["batch"])
            if key != group:
                finish_group()
                group, tail, group_first, group_count = key, deque(maxlen=10), record, 0
            group_last = record
            group_count += 1
            tail.append(record["metrics"])
    finish_group()
    if next(idle_records, None) is not None:
        raise ValueError("idle-window journal contains extra observations")
    if count != metadata["sampleCount"] or aggregates != metadata["metrics"] or dict(phases) != metadata["phases"]:
        raise ValueError("resource report disagrees with raw sample counts or aggregates")
    if baseline_count != 1 or final_count != 1 or group[0] != "final-sample":
        raise IncompleteEvidence("protocol-ready or final resource evidence is missing")
    initial = [window for window in windows if window["phase"] == "warmup-idle" and window["medians"] is not None][-3:]
    later = [window for window in windows if window["phase"] == "idle" and window["medians"] is not None
             and window["started"] >= measured_start + options.later_reference_seconds
             and window["finished"] <= measured_end]
    reference = medians([window["medians"] for window in initial]) if len(initial) == 3 else None
    later_reference = medians([window["medians"] for window in later[:3]]) if len(later) >= 3 else None
    final = next((window["medians"] for window in reversed(windows) if window["phase"] == "final-idle"), None)
    hours = {}
    for hour in (2, 3, 4):
        points = [(window["started"] - measured_start, window["medians"]) for window in later
                  if (hour - 1) * 3600 <= window["started"] - measured_start < hour * 3600]
        hours[str(hour)] = {"windows": len(points), "metrics": distribution(points) if points else {}}
    coverage = []
    if gaps:
        coverage.append(f"{gaps} sampling gaps exceeded three intervals")
    if measured_first is None or measured_first - measured_start > 3 * options.interval or measured_end - measured_last > 3 * options.interval:
        coverage.append("samples do not cover the declared measurement interval")
    full_profile = spec.timing.measured >= 14400
    steady_reasons = []
    if not full_profile:
        steady_reasons.append("short profile lacks four-hour resource coverage")
    if reference is None:
        steady_reasons.append("three valid warm-up idle windows are unavailable")
    if later_reference is None or any(hour["windows"] < 3 for hour in hours.values()):
        steady_reasons.append("three valid idle windows in each of measured hours 2–4 are required")
    if not options.retention_revision:
        steady_reasons.append("retention policy is not tied to the tested broker revision")
    if spec.timing.warmup_min + options.later_reference_seconds < options.retention_seconds + options.reap_seconds:
        steady_reasons.append("later reference precedes the recorded retention/reap budget")
    if optional_errors:
        steady_reasons.append("verified optional cgroup counters became unavailable")
    breaches = []
    for name, limit in options.absolute_limits.items():
        if aggregates[name]["peak"] > limit:
            breaches.append({"metric": name, "kind": "absolute", "observed": aggregates[name]["peak"], "limit": limit})
    if later_reference is not None:
        for name, limit in options.growth_limits.items():
            peak_delta = max(window["medians"][name] - later_reference[name] for window in later)
            if final is not None:
                peak_delta = max(peak_delta, final[name] - later_reference[name])
            if peak_delta > limit:
                breaches.append({"metric": name, "kind": "idleGrowth", "observed": peak_delta, "limit": limit})
    reasons = coverage + steady_reasons
    assessment = "INCONCLUSIVE"
    if options.growth_limits:
        if any(item["kind"] == "idleGrowth" for item in breaches):
            assessment = "GROWTH_DETECTED"
        elif not reasons and not breaches:
            assessment = "WITHIN_LIMITS"
    def delta(value, base):
        return {name: value[name] - base[name] for name in RESOURCE_METRICS} if value is not None and base is not None else None
    return {"evidenceStatus": "INCOMPLETE" if coverage or (full_profile and (reference is None or final is None or later_reference is None or any(hour["windows"] < 3 for hour in hours.values()))) else "COMPLETE",
            "resourceAssessment": assessment, "limitsExceeded": bool(breaches),
            "reasons": reasons, "sampleCount": count, "samplingGaps": gaps,
            "metrics": aggregates, "initialReference": reference, "laterReference": later_reference,
            "cgroup": {"coverage": dict(cgroup_coverage), "currentBytes": cgroup_current, "lastCounters": cgroup_last},
            "finalIdle": final, "finalMinusInitial": delta(final, reference), "finalMinusLater": delta(final, later_reference),
            "initialGrowthIncludesCacheFill": True, "hours": hours, "breaches": breaches,
            "retentionPolicy": {"seconds": options.retention_seconds, "reapSeconds": options.reap_seconds,
                                "revision": options.retention_revision}}
