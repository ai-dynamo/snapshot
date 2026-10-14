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

SUPPORTED_SCHEMA_VERSIONS = (1,)


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
    if type(version) is int and version == 1:
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


def _image_section(results: list[dict]) -> list[str]:
    """Names the files in the checkpoint and the CRIU log lines around the dump."""
    lines = [
        "## What is in the checkpoint image",
        "",
        "Categories sum every file. Apparent size counts sparse holes; "
        "allocated size is `st_blocks * 512`. Files lists only those of at "
        "least 1 MiB. `pages` is CRIU's private page images. Dump log times "
        "are CRIU's own clock. `Pagemap generated` is the page list, before "
        "the write. `Dumping core` is the first metadata after that write. "
        "Thread cores are one line per thread, summarized as first–last. "
        "`Unfreezing tasks` is the start of killing the frozen tree, and "
        "`Writing stats` is after that wait. `Dumping finished` is the end.",
        "",
    ]
    for result in results:
        image = result.get("checkpoint_image") or {}
        if not image:
            continue
        label = (result.get("model") or {}).get("label", "-")
        lines.append(f"### {label}")
        lines.append("")
        lines.append(
            "| Category | Apparent | Allocated | Files | | Dump log | Offset (s) |"
        )
        lines.append("|---|---:|---:|---:|---|---|---:|")
        categories = image.get("categories") or []
        marks = _dump_marks(image)
        rows = max(len(categories), len(marks), 1)
        for index in range(rows):
            if index < len(categories):
                category = categories[index]
                category_name = category.get("name", "-")
                category_bytes = _fmt_bytes(category.get("bytes"))
                category_allocated = _fmt_bytes(category.get("allocated_bytes"))
                category_count = str(category.get("file_count", "-"))
            else:
                category_name, category_bytes, category_allocated, category_count = "", "", "", ""
            if index < len(marks):
                mark_name, mark_offset_text = marks[index]
            else:
                mark_name, mark_offset_text = "", ""
            lines.append(
                f"| {category_name} | {category_bytes} | {category_allocated} | {category_count} | "
                f"| {mark_name} | {mark_offset_text} |"
            )
        lines.append("")
        lines.append(
            f"Pages apparent {_fmt_bytes(image.get('pages_bytes'))}, "
            f"allocated {_fmt_bytes(image.get('pages_allocated_bytes'))}, "
            f"sparse {_fmt_bytes(image.get('pages_sparse_bytes'))}. "
            f"Pagemap holes {image.get('pagemap_holes', 0)} "
            f"({_fmt_bytes(image.get('pagemap_hole_bytes'))})."
        )
        lines.append("")
        large = image.get("files") or []
        if large:
            lines.append("| File | Apparent | Allocated |")
            lines.append("|---|---:|---:|")
            for item in large:
                lines.append(
                    f"| `{item.get('path', '-')}` | {_fmt_bytes(item.get('bytes'))} | "
                    f"{_fmt_bytes(item.get('allocated_bytes'))} |"
                )
            lines.append("")
    return lines


def _dump_marks(image: dict) -> list[tuple[str, str]]:
    first = image.get("thread_core_first_seconds")
    last = image.get("thread_core_last_seconds")
    count = image.get("thread_core_count") or 0
    if first is None and last is None:
        thread_span = "-"
    else:
        thread_span = f"{_fmt(first)}–{_fmt(last)} ({count})"
    return [
        ("First Dumping pages", _fmt(image.get("first_dumping_pages_seconds"))),
        ("Last Pagemap generated", _fmt(image.get("last_pagemap_seconds"))),
        ("First Dumping core", _fmt(image.get("first_dumping_core_seconds"))),
        ("Thread cores", thread_span),
        ("Dumping mm", _fmt(image.get("dumping_mm_seconds"))),
        ("Dumping pstree", _fmt(image.get("dumping_pstree_seconds"))),
        ("Namespaces complete", _fmt(image.get("namespaces_complete_seconds"))),
        ("Unfreezing tasks", _fmt(image.get("unfreezing_seconds"))),
        ("Writing stats", _fmt(image.get("writing_stats_seconds"))),
        ("Dumping finished", _fmt(image.get("dumping_finished_seconds"))),
    ]


def render(results: list[dict]) -> str:
    if not results:
        return "no results found\n"

    results = [_normalize_result(r) for r in results]

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

    lines.append("## Where the capture time goes")
    lines.append("")
    lines.append(
        "Wall, CUDA, CRIU, and overlay come from the capture-node agent's "
        "Checkpoint timing summary. CUDA lock is the device freeze; CUDA copy "
        "moves device state into host memory before CRIU runs. CRIU memdump is "
        "CRIU reading the frozen process; CRIU memwrite is CRIU writing pages. "
        "Those two CRIU timers overlap the CRIU wall and are not added to it. "
        "Checkpoint (API) is the one-second Kubernetes Ready timestamp and is "
        "coarser than Wall."
    )
    lines.append("")
    lines.append(
        "| Model | Wall (s) | CUDA lock | CUDA copy | CRIU wall | CRIU freeze | "
        "CRIU memdump | CRIU memwrite | Overlay | Checkpoint (API) |"
    )
    lines.append("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for r in results:
        model = r.get("model", {})
        checkpoint = r.get("checkpoint") or {}
        capture = r.get("checkpoint_agent_log") or {}
        phases = capture.get("phases") or {}
        lines.append(
            "| {label} | {wall} | {lock} | {copy} | {criu} | {freeze} | {memdump} | "
            "{memwrite} | {overlay} | {api} |".format(
                label=model.get("label", "-"),
                wall=_fmt(capture.get("duration")),
                lock=_fmt(capture.get("cuda_lock")),
                copy=_fmt(capture.get("cuda_copy")),
                criu=_fmt(phases.get("criu_dump")),
                freeze=_fmt(capture.get("criu_freezing")),
                memdump=_fmt(capture.get("criu_memdump")),
                memwrite=_fmt(capture.get("criu_memwrite")),
                overlay=_fmt(phases.get("overlay_capture")),
                api=_fmt(checkpoint.get("checkpoint_seconds")),
            )
        )
    lines.append("")

    lines.append("## How the CRIU image was written")
    lines.append("")
    lines.append(
        "Memwrite GB/s divides CRIU's written page bytes by CRIU's memwrite "
        "timer. Image GB/s divides the whole CRIU image by the agent's CRIU "
        "wall, which is the lower number because that wall also includes "
        "freeze and memory collection. Midpoint fraction is how much of the "
        "final image was already on disk halfway through the dump: near 0.5 "
        "is a steady write, near 0 means the bytes arrived late. Max stall is "
        "the longest stretch where the image size did not grow."
    )
    lines.append("")
    lines.append(
        "| Model | Pages written | CRIU image | Overlay diff | Memwrite GB/s | "
        "Image GB/s | Midpoint fraction | Max stall (s) |"
    )
    lines.append("|---|---:|---:|---:|---:|---:|---:|---:|")
    for r in results:
        model = r.get("model", {})
        capture = r.get("checkpoint_agent_log") or {}
        lines.append(
            "| {label} | {pages} | {image} | {overlay} | {memwrite_rate} | "
            "{image_rate} | {mid} | {stall} |".format(
                label=model.get("label", "-"),
                pages=_fmt_bytes(capture.get("criu_pages_written_bytes")),
                image=_fmt_bytes(capture.get("criu_image_bytes")),
                overlay=_fmt_bytes(capture.get("overlay_diff_bytes")),
                memwrite_rate=_fmt(capture.get("memwrite_gigabytes_per_second")),
                image_rate=_fmt(capture.get("criu_image_gigabytes_per_second")),
                mid=_fmt(capture.get("image_growth_midpoint_fraction")),
                stall=_fmt(capture.get("image_growth_max_stall_seconds")),
            )
        )
    lines.append("")

    if any(r.get("checkpoint_image") for r in results):
        lines.extend(_image_section(results))

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
        for w in (
            ((r.get("agent_log_phases") or {}).get("parse_warnings") or [])
            + ((r.get("checkpoint_agent_log") or {}).get("parse_warnings") or [])
            + ((r.get("checkpoint_image") or {}).get("parse_warnings") or [])
        )
    ]
    if warnings or agent_log_warnings:
        lines.append("## Warnings")
        lines.append("")
        for label, warning in warnings + agent_log_warnings:
            lines.append(f"- **{label}**: {warning}")
        lines.append("")

    return "\n".join(lines) + "\n"
