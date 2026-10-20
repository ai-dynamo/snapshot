# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Validate paired spans and retain inclusive/exclusive durations per process/thread."""

import argparse, json, re
from collections import defaultdict
from pathlib import Path
from span_log import decode_spans

parser = argparse.ArgumentParser()
parser.add_argument("folder", type=Path)
parser.add_argument("--span-log", type=Path)
args = parser.parse_args()
process_ranks = {}
process_file = args.folder / "processes.txt"
if process_file.exists():
    for line in process_file.read_text().splitlines():
        match = re.search(r"^\s*(\d+)\s+sglang::scheduler_TP(\d+)", line)
        if match:
            process_ranks[int(match[1])] = int(match[2])
lines = (args.folder / "startup.log").read_text().splitlines()
begins = {}
ends = {}
marks = []
pid_ranks = dict(process_ranks)
for line in lines:
    worker = re.search(r"Worker_TP(\d+)[^)]*pid=(\d+)", line)
    if worker:
        pid_ranks[int(worker[2])] = int(worker[1])
    if "COLD_BENCH " in line:
        marks.append(json.loads(line.split("COLD_BENCH ", 1)[1]))
records, repairs = decode_spans(
    args.span_log.read_text().splitlines() if args.span_log else lines
)
for q in records:
    (begins if q["event"] == "begin" else ends)[q["span"]] = q


def union(intervals):
    end = 0
    total = 0
    for a, b in sorted(intervals):
        total += max(0, b - max(end, a))
        end = max(end, b)
    return total


spans = []
children = defaultdict(list)
for key, b in begins.items():
    if key not in ends:
        continue
    e = ends[key]
    if b["parent"]:
        children[b["parent"]].append((b["monotonic_ns"], e["monotonic_ns"]))
for key, b in begins.items():
    if key not in ends:
        continue
    e = ends[key]
    duration = e["monotonic_ns"] - b["monotonic_ns"]
    exclusive = duration - union(children[key])
    assert exclusive >= 0, (key, exclusive)
    spans.append(
        dict(
            name=b["name"],
            pid=b["pid"],
            tid=b["tid"],
            rank=b["rank"] if b["rank"] is not None else pid_ranks.get(b["pid"]),
            rank_source="environment"
            if b["rank"] is not None
            else "process-title"
            if b["pid"] in process_ranks
            else "worker-log"
            if b["pid"] in pid_ranks
            else None,
            span=key,
            parent=b["parent"],
            start_wall_ns=b["wall_ns"],
            end_wall_ns=e["wall_ns"],
            inclusive_s=duration / 1e9,
            exclusive_s=exclusive / 1e9,
            success=e["success"],
        )
    )
report = dict(
    span_source=str(args.span_log or args.folder / "startup.log"),
    log_fragment_repairs=repairs,
    pid_rank_map=pid_ranks,
    completed_spans=len(spans),
    unclosed_spans=[b for key, b in begins.items() if key not in ends],
    unmatched_ends=[e for key, e in ends.items() if key not in begins],
    markers=marks,
    spans=spans,
)
(args.folder / "spans.json").write_text(json.dumps(report, indent=2) + "\n")
if report["unclosed_spans"] or report["unmatched_ends"]:
    raise ValueError("Incomplete timing spans; raw log and span inventory retained")
print(
    "Completed spans",
    len(spans),
    "unclosed",
    len(report["unclosed_spans"]),
    "unmatched ends",
    len(report["unmatched_ends"]),
)
for s in sorted(spans, key=lambda s: s["exclusive_s"], reverse=True)[:20]:
    print(
        f"{s['exclusive_s']:.3f}s exclusive / {s['inclusive_s']:.3f}s inclusive",
        s["name"],
    )
