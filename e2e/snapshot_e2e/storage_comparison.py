# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Side by side timings of the checkpoint volume and S3 from one workflow run.

    python -m snapshot_e2e.storage_comparison --pvc-dir DIR --s3-dir DIR \\
        --case vllm [--case sglang ...] [--summary-file FILE]

Each directory holds the benchmark results of one storage, as the framework
test writes them. Results are paired by framework (the benchmark case).
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any, Mapping, Sequence

from snapshot_e2e import benchmark_history

SUITE = "framework-checkpoint-restore"
LEADING_MEASUREMENTS = (
    "checkpoint.duration",
    "restore.to_traffic.duration",
    "restore.pod_create_to_traffic.duration",
    "checkpoint.agent.duration",
    "restore.agent.duration",
)


def load_results(directory: Path) -> dict[str, dict[str, Any]]:
    """Framework results in a directory, keyed by case; a later file wins."""
    results: dict[str, dict[str, Any]] = {}
    for path in sorted(directory.glob("*.json")):
        result = benchmark_history.load_result(path)
        identity = result["identity"]
        if identity["suite"] == SUITE:
            results[identity["case"]] = result
    return results


def seconds(result: Mapping[str, Any] | None) -> dict[str, float]:
    if result is None:
        return {}
    return {
        item["name"]: float(item["value"])
        for item in result["measurements"]
        if item["status"] == "complete" and item["unit"] == "seconds"
    }


def display_names(*results: Mapping[str, Any] | None) -> dict[str, str]:
    names: dict[str, str] = {}
    for result in results:
        for item in (result or {}).get("measurements", []):
            names.setdefault(item["name"], item["displayName"])
    return names


def ordered(names: Sequence[str]) -> list[str]:
    leading = [name for name in LEADING_MEASUREMENTS if name in names]
    return leading + sorted(name for name in names if name not in leading)


def describe(result: Mapping[str, Any] | None) -> str:
    if result is None:
        return "no result"
    dimensions = benchmark_history.comparison_dimensions(result)
    gpus = ", ".join(dimensions["sourceGpuModels"]) or "unknown GPU"
    return f"{result['outcome']}, {gpus}"


def cell(value: float | None) -> str:
    return "n/a" if value is None else f"{value:.1f} s"


def difference(pvc: float | None, s3: float | None) -> str:
    if pvc is None or s3 is None:
        return "n/a"
    delta = s3 - pvc
    if pvc == 0:
        return f"{delta:+.1f} s"
    return f"{delta:+.1f} s ({delta / pvc * 100:+.0f}%)"


def render_case(
    case: str,
    pvc: Mapping[str, Any] | None,
    s3: Mapping[str, Any] | None,
) -> str:
    pvc_seconds, s3_seconds = seconds(pvc), seconds(s3)
    names = display_names(pvc, s3)
    lines = [
        f"### {case}",
        "",
        f"PVC: {describe(pvc)}. S3: {describe(s3)}.",
        "",
        "| Measurement | PVC | S3 | S3 vs PVC |",
        "|---|---:|---:|---:|",
    ]
    for name in ordered(list(pvc_seconds.keys() | s3_seconds.keys())):
        before, after = pvc_seconds.get(name), s3_seconds.get(name)
        lines.append(
            f"| {names[name]} | {cell(before)} | {cell(after)} | {difference(before, after)} |"
        )
    if not pvc_seconds and not s3_seconds:
        lines.append("| no completed measurements | n/a | n/a | n/a |")
    return "\n".join(lines)


def render(
    cases: Sequence[str],
    pvc: Mapping[str, Mapping[str, Any]],
    s3: Mapping[str, Mapping[str, Any]],
) -> str:
    sections = ["## Checkpoint volume and S3, same run", ""]
    sections.extend(render_case(case, pvc.get(case), s3.get(case)) + "\n" for case in cases)
    sections.append(
        "Both stores ran in this workflow run on the same cluster, but not necessarily on "
        "the same node. Compare the GPU models above before reading the difference."
    )
    return "\n".join(sections) + "\n"


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--pvc-dir", type=Path, required=True)
    parser.add_argument("--s3-dir", type=Path, required=True)
    parser.add_argument("--case", action="append", required=True)
    parser.add_argument("--summary-file", type=Path)
    parser.add_argument("--json-file", type=Path)
    args = parser.parse_args(argv)
    pvc, s3 = load_results(args.pvc_dir), load_results(args.s3_dir)
    text = render(args.case, pvc, s3)
    print(text)
    if args.summary_file:
        with args.summary_file.open("a", encoding="utf-8") as handle:
            handle.write(text)
    if args.json_file:
        args.json_file.write_text(
            json.dumps(
                {
                    case: {"pvc": seconds(pvc.get(case)), "s3": seconds(s3.get(case))}
                    for case in args.case
                },
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
