# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Renders a standalone text summary from a results directory.

Deliberately does not write to docs/development/benchmarks.md or any other
doc: these are raw, locally-generated numbers from whatever hardware the user
ran on, and the published doc's tables stay a manually-reviewed, hand-edited
artifact. See docs/development/benchmarks-guide.md for why.
"""

from __future__ import annotations

from typing import Any


def _fmt(value: Any, digits: int = 3) -> str:
    if value is None:
        return "-"
    if isinstance(value, float):
        return f"{value:.{digits}f}"
    return str(value)


def _fmt_bytes(value: int | None) -> str:
    if value is None:
        return "-"
    gb = value / 1e9
    return f"{gb:.2f} GB"


def render(results: list[dict]) -> str:
    if not results:
        return "no results found\n"

    lines: list[str] = []
    lines.append("# Snapshot benchmark results (standalone report, not published doc content)")
    lines.append("")
    lines.append(
        "Absolute numbers below are only comparable to "
        "docs/development/benchmarks.md on equivalent hardware (same GPU model, "
        "driver major, and storage backend/class) -- see each run's "
        '"environment" block. See docs/development/benchmarks-guide.md for how '
        "to interpret a mismatch."
    )
    lines.append("")

    lines.append("## Cold start vs. Snapshot restore")
    lines.append("")
    lines.append(
        "| Model | Weights (reported) | Checkpoint size (measured) | Checkpoint (s) | "
        "Cold start (s) | Restore total (s) | GPU | Driver | Storage | Placement |"
    )
    lines.append("|---|---:|---:|---:|---:|---:|---|---|---|---|")
    for r in results:
        model = r.get("model", {})
        env = r.get("environment", {})
        cold_start = r.get("cold_start") or {}
        checkpoint = r.get("checkpoint") or {}
        restore = r.get("restore") or {}
        agent_log = r.get("agent_log_phases") or {}
        restore_total = agent_log.get("duration")
        if restore_total is None:
            restore_total = restore.get("restore_total_seconds")
        lines.append(
            "| {label} | {weights} | {ckpt} | {ckpt_s} | {cold} | {restore} | {gpu} | "
            "{driver} | {storage} | {placement} |".format(
                label=model.get("label", "-"),
                weights=_fmt_bytes(model.get("reported_weights_bytes")),
                ckpt=_fmt_bytes(model.get("checkpoint_artifact_bytes")),
                ckpt_s=_fmt(checkpoint.get("checkpoint_seconds")),
                cold=_fmt(cold_start.get("cold_start_excl_container_seconds")),
                restore=_fmt(restore_total),
                gpu=env.get("gpu_product") or "-",
                driver=env.get("gpu_driver_version") or "-",
                storage=env.get("storage_class") or "-",
                placement=env.get("placement") or "-",
            )
        )
    lines.append("")

    lines.append("## Where the restore time goes")
    lines.append("")
    lines.append(
        "Columns ending in `(approx)` are a best-effort remap of the node "
        'agent\'s own phase names to the published doc\'s 4-stage vocabulary '
        '-- see AgentLogPhases in schema.py for the exact mapping. '
        '"wake/remap" is not represented in the agent log at all; use the '
        '"Snapshot restore" / "vLLM wake+copy" columns instead, which are '
        "derived from the nvidia.com/Restored pod condition and are exact, "
        "not approximated (at one-second resolution)."
    )
    lines.append("")
    lines.append(
        "| Model | agent setup (approx) | CRIU restore (approx) | CUDA restore | "
        "Total (agent log) | Snapshot restore | vLLM wake+copy |"
    )
    lines.append("|---|---:|---:|---:|---:|---:|---:|")
    for r in results:
        model = r.get("model", {})
        restore = r.get("restore") or {}
        agent_log = r.get("agent_log_phases") or {}
        lines.append(
            "| {label} | {setup} | {criu} | {cuda} | {total} | {snap} | {wake} |".format(
                label=model.get("label", "-"),
                setup=_fmt(agent_log.get("agent_setup_approx")),
                criu=_fmt(agent_log.get("criu_restore_approx")),
                cuda=_fmt(agent_log.get("cuda_restore_approx")),
                total=_fmt(agent_log.get("duration")),
                snap=_fmt(restore.get("snapshot_restore_seconds")),
                wake=_fmt(restore.get("vllm_wake_and_copy_seconds")),
            )
        )
    lines.append("")

    warnings = [
        (r.get("model", {}).get("label", "?"), w)
        for r in results
        for w in (r.get("warnings") or [])
    ]
    agent_log_warnings = [
        (r.get("model", {}).get("label", "?"), w)
        for r in results
        for w in ((r.get("agent_log_phases") or {}).get("parse_warnings") or [])
    ]
    if warnings or agent_log_warnings:
        lines.append("## Warnings")
        lines.append("")
        for label, warning in warnings + agent_log_warnings:
            lines.append(f"- **{label}**: {warning}")
        lines.append("")

    return "\n".join(lines) + "\n"
