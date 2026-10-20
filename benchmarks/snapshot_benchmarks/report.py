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

SUPPORTED_SCHEMA_VERSIONS = (1, 2)


def _normalize_result(result: dict) -> dict:
    """Returns `result` unchanged, or raises for a `schema_version` this report
    renderer doesn't know how to read -- silently rendering an unsupported/
    unrecognized shape would display missing GPU metadata as "no data" rather
    than "can't read this file", masking a real schema mismatch in a directory
    of mixed-version results."""
    version = result.get("schema_version")
    # `type(...) is int` rather than `isinstance`/`==` on purpose: `bool` is an
    # `int` subclass (`isinstance(True, int)` is `True`) and `1.0 == 1`, so
    # either check would silently accept `True`/`1.0` as schema_version 1.
    if type(version) is int and version in SUPPORTED_SCHEMA_VERSIONS:
        return result
    run_id = result.get("run_id", "<unknown>")
    label = (result.get("model") or {}).get("label", "<unknown>")
    raise ValueError(
        f"unsupported schema_version {version!r} in result run_id={run_id!r} "
        f"model={label!r} -- report.py only renders schema_version "
        f"{SUPPORTED_SCHEMA_VERSIONS}"
    )


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


def _fmt_driver(identity: dict) -> str:
    """Renders a GPU identity's driver field, falling back to the node's
    `cuda_driver_major_label` (major version only, e.g. "595") when the live
    `nvidia-smi` query that fills `gpu_driver_version` failed -- clearly
    marked as major-only so it's never mistaken for the full driver string
    `_heterogeneous_restore_warning` compares against."""
    version = identity.get("gpu_driver_version")
    if version:
        return version
    major = identity.get("cuda_driver_major_label")
    if major:
        return f"{major} (major only)"
    return "-"


def _table(headers, rows):
    return "\n".join(
        [
            "| " + " | ".join(headers) + " |",
            "| " + " | ".join(["---"] * len(headers)) + " |",
        ]
        + ["| " + " | ".join(map(str, row)) + " |" for row in rows]
    )


def _cold_phases(measured):
    phases = measured["phases_s"]
    raw = [
        phases["Process/worker startup"] + phases["Runtime imports"],
        phases["Model construction and weights"],
        sum(
            phases[k]
            for k in (
                "Memory profiling",
                "Attention/sampler/kernel initialization",
                "MHC kernel prewarm",
            )
        ),
        sum(
            phases[k]
            for k in (
                "Compilation and JIT module loading",
                "Autotuning",
                "CUDA graph capture",
            )
        ),
    ]
    total = measured["cold_start_s"]
    if sum(raw) <= 0:
        raise ValueError("Cold start has no measured phases")
    scaled = [v * total / sum(raw) for v in raw]
    cents = [int(v * 100) for v in scaled]
    for i in sorted(range(4), key=lambda i: scaled[i] * 100 - cents[i], reverse=True)[
        : round(total * 100) - sum(cents)
    ]:
        cents[i] += 1
    return [v / 100 for v in cents]


def render(results: list[dict]) -> str:
    if not results:
        return "no results found\n"
    results = [_normalize_result(r) for r in results]
    legacy = [r for r in results if r["schema_version"] == 1]
    current = [r for r in results if r["schema_version"] == 2]
    if not current:
        return _render_legacy(legacy)
    headline, cold, restore, environment = [], [], [], []
    f = lambda value: "-" if value is None else f"{value:.2f}"
    for r in current:
        model, engine, env = r["model"], r["engine"], r["environment"]
        topology = f"TP{model['tp']}" + (f"/EP{model['ep']}" if model["ep"] > 1 else "")
        prefix = [model["label"], engine["name"], topology]
        elapsed = r["cold_start"]["startup_to_ready_seconds"]
        timing = (r.get("restore") or {}).get("measurement")
        restored = timing["total_s"] if timing else None
        headline.append(
            prefix
            + [
                _fmt_bytes(model.get("reported_weights_bytes")),
                _fmt_bytes(model.get("checkpoint_artifact_bytes")),
                f((r.get("checkpoint") or {}).get("checkpoint_seconds")),
                f(elapsed),
                f(restored),
                f(elapsed / restored) + "×" if restored else "-",
            ]
        )
        cold.append(
            prefix
            + [f(v) for v in _cold_phases(r["cold_start"]["measurement"])]
            + [f(elapsed)]
        )
        if timing:
            restore.append(
                prefix
                + [
                    f(timing[k])
                    for k in (
                        "setup_s",
                        "criu_s",
                        "cuda_s",
                        "wake_handoff_s",
                        "total_s",
                    )
                ]
            )
        capture, destination = env.get("capture") or {}, env.get("restore") or {}
        environment.append(
            prefix
            + [
                engine.get("version") or "-",
                capture.get("gpu_product") or "-",
                _fmt_driver(capture),
                destination.get("gpu_product") or "-",
                _fmt_driver(destination),
                env.get("storage_class") or "-",
                env.get("placement") or "-",
            ]
        )
    text = "# Snapshot benchmark results\n\n## Cold start compared with Snapshot\n\n"
    text += _table(
        [
            "Model",
            "Engine",
            "Parallelism",
            "Weights",
            "Checkpoint size",
            "Checkpoint (s)",
            "Cold start (s)",
            "Snapshot (s)",
            "Speedup",
        ],
        headline,
    )
    text += "\n\n## Where the cold start time goes (s)\n\n"
    text += _table(
        [
            "Model",
            "Engine",
            "Parallelism",
            "Startup",
            "Model load",
            "Initialization",
            "Compilation, autotuning & graph capture",
            "Total",
        ],
        cold,
    )
    text += "\n\nOverlapping worker intervals count once. Untimed intervals are distributed proportionally; these additions are estimates. Raw intervals and gaps remain in each run's evidence. Some kernel JIT occurs during Startup.\n\n## Where the restore time goes (s)\n\n"
    text += _table(
        [
            "Model",
            "Engine",
            "Parallelism",
            "Agent setup / rootfs",
            "CRIU restore",
            "CUDA restore",
            "Wake / handoff",
            "Total",
        ],
        restore,
    )
    text += "\n\nRestore uses attempt 1, measured through wake/resume completion, before inference. Attempts 2 and 3 remain in the local results. CUDA restore includes multi-GPU reconstruction.\n\n## Observed environment\n\n"
    text += (
        _table(
            [
                "Model",
                "Engine",
                "Parallelism",
                "Engine version",
                "Capture GPU",
                "Capture driver",
                "Restore GPU",
                "Restore driver",
                "Storage class",
                "Placement",
            ],
            environment,
        )
        + "\n"
    )
    warnings = [
        f"- {r['model']['label']}: {warning}"
        for r in current
        for warning in r.get("warnings", [])
    ]
    if warnings:
        text += "\n## Warnings\n\n" + "\n".join(warnings) + "\n"
    if legacy:
        text += (
            "\n## Legacy measurements (different timing boundaries)\n\n"
            + _render_legacy(legacy)
        )
    return text


def _render_legacy(results: list[dict]) -> str:
    if not results:
        return "no results found\n"

    results = [_normalize_result(r) for r in results]

    lines: list[str] = []
    lines.append(
        "# Snapshot benchmark results (standalone report, not published doc content)"
    )
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
        "Cold start (s) | Restore, agent-internal (s) | Restore total, incl. vLLM wake (s) | "
        "GPU (capture) | Driver (capture) | GPU (restore) | Driver (restore) | Storage | Placement |"
    )
    lines.append("|---|---:|---:|---:|---:|---:|---:|---|---|---|---|---|---|")
    for r in results:
        model = r.get("model", {})
        env = r.get("environment", {})
        capture_gpu = env.get("capture") or {}
        restore_gpu = env.get("restore") or {}
        cold_start = r.get("cold_start") or {}
        checkpoint = r.get("checkpoint") or {}
        restore = r.get("restore") or {}
        agent_log = r.get("agent_log_phases") or {}
        # These are two different measurements, not two precisions of the same
        # one: `agent_log.duration` is the node agent's own restore duration
        # (excludes vLLM's wake-and-copy-to-GPU step entirely), while
        # `restore_total_seconds` spans container start to pod Ready (includes
        # it). Rendered as separate columns rather than one falling back to
        # the other, so a row where the agent log didn't parse never silently
        # gets compared against a different metric than the rest of the table.
        #
        # GPU/driver are shown for capture and restore separately -- not
        # collapsed into one column -- because on a heterogeneous cluster (or
        # any `different_node` placement) the restore node's hardware can
        # genuinely differ from the capture node's; see `warnings` below for
        # an explicit flag when they do.
        lines.append(
            "| {label} | {weights} | {ckpt} | {ckpt_s} | {cold} | {restore_agent} | "
            "{restore_total} | {cgpu} | {cdriver} | {rgpu} | {rdriver} | {storage} | "
            "{placement} |".format(
                label=model.get("label", "-"),
                weights=_fmt_bytes(model.get("reported_weights_bytes")),
                ckpt=_fmt_bytes(model.get("checkpoint_artifact_bytes")),
                ckpt_s=_fmt(checkpoint.get("checkpoint_seconds")),
                cold=_fmt(cold_start.get("cold_start_excl_container_seconds")),
                restore_agent=_fmt(agent_log.get("duration")),
                restore_total=_fmt(restore.get("restore_total_seconds")),
                cgpu=capture_gpu.get("gpu_product") or "-",
                cdriver=_fmt_driver(capture_gpu),
                rgpu=restore_gpu.get("gpu_product") or "-",
                rdriver=_fmt_driver(restore_gpu),
                storage=env.get("storage_class") or "-",
                placement=env.get("placement") or "-",
            )
        )
    lines.append("")

    lines.append("## Where the restore time goes")
    lines.append("")
    lines.append(
        "Columns ending in `(approx)` are a best-effort remap of the node "
        "agent's own phase names to the published doc's 4-stage vocabulary "
        "-- see AgentLogPhases in schema.py for the exact mapping. "
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
                wake=_fmt(restore.get("wake_and_copy_seconds")),
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
