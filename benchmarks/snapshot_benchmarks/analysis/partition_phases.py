# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Make non-overlapping wall-time accounting from measured boundaries, keeping audit gaps."""

import json
from collections import Counter, defaultdict
from pathlib import Path
import argparse

parser = argparse.ArgumentParser()
parser.add_argument("directory", type=Path)
args = parser.parse_args()
P = args.directory
columns = [
    "Process/worker startup",
    "Model construction and weights",
    "Memory profiling",
    "Attention/sampler/kernel initialization",
    "Compilation and JIT module loading",
    "Autotuning",
    "CUDA graph capture",
    "First generation",
    "Runtime imports",
    "MHC kernel prewarm",
]
# Narrow work supersedes encompassing function scopes, including profiling and warmup.
priority = [
    "First generation",
    "Compilation and JIT module loading",
    "Autotuning",
    "MHC kernel prewarm",
    "CUDA graph capture",
    "Model construction and weights",
    "Attention/sampler/kernel initialization",
    "Memory profiling",
    "Process/worker startup",
    "Runtime imports",
]
rename = {
    "Model loading": columns[1],
    "Torch compilation": columns[4],
    "JIT module build/load": columns[4],
    "Attention initialization": columns[3],
    "Sampler initialization": columns[3],
    "Imports": columns[8],
}
results = []
directories = {
    case["case"]: Path(case["directory"])
    for case in json.loads((P / "cold-runs.json").read_text())["cases"]
}
for old in json.loads((P / "phase-inventory.json").read_text()):
    d = directories[old["case"]]
    spans = json.loads((d / "spans.json").read_text())
    m = spans["markers"][0]
    origin = m["wall_time_ns"] / 1e9 - m["elapsed_s"]
    intervals = []
    for x in old["intervals"]:
        if x["category"] == "Device and distributed initialization":
            continue
        intervals.append(dict(x, category=rename.get(x["category"], x["category"])))
    for span in spans["spans"]:
        if span["name"].endswith(".PyTorchModelEngine.__init__"):
            intervals.append(
                dict(
                    category=columns[1],
                    start=span["start_wall_ns"] / 1e9 - origin,
                    end=span["end_wall_ns"] / 1e9 - origin,
                    source=span["name"],
                )
            )
        if span["name"].endswith(
            (".Worker.compile_or_warm_up_model", ".PyTorchModelEngine.warmup")
        ):
            intervals.append(
                dict(
                    category=columns[3],
                    start=span["start_wall_ns"] / 1e9 - origin,
                    end=span["end_wall_ns"] / 1e9 - origin,
                    source=span["name"]
                    + " minus separately accounted compilation, tuning, capture and profiling",
                )
            )
    first = min(x["start"] for x in intervals if x["category"] == columns[1])
    intervals.append(
        dict(
            category=columns[0],
            start=0,
            end=first,
            source="process_start to first model-load/construction span; includes imports, process creation, worker/device initialization",
        )
    )
    total = next(
        m["elapsed_s"] for m in spans["markers"] if m["stage"] == "engine_init_end"
    )
    events = defaultdict(Counter)
    events[0]
    events[total]
    for x in intervals:
        a = max(0, min(total, x["start"]))
        b = max(0, min(total, x["end"]))
        if b > a:
            events[a][x["category"]] += 1
            events[b][x["category"]] -= 1
    boundaries = sorted(events)
    active = Counter()
    times = {c: 0 for c in columns}
    unattributed = []
    for a, b in zip(boundaries, boundaries[1:]):
        active.update(events[a])
        label = next((c for c in priority if active[c] > 0), None)
        if label:
            times[label] += b - a
        elif unattributed and abs(unattributed[-1]["end"] - a) < 1e-7:
            unattributed[-1]["end"] = b
        else:
            unattributed.append(dict(start=a, end=b))
    covered = sum(times.values())
    gap = sum(x["end"] - x["start"] for x in unattributed)
    assert abs(covered + gap - total) < 1e-5
    results.append(
        dict(
            case=old["case"],
            cold_start_s=total,
            phases_s=times,
            covered_s=covered,
            unattributed_s=gap,
            gaps=unattributed,
            intervals=intervals,
        )
    )
(P / "phase-accounting.json").write_text(json.dumps(results, indent=2) + "\n")
