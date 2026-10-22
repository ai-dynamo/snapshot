# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Renders a standalone text summary from a results directory.

Deliberately does not write to docs/overview/performance.md or any other
doc: these are raw, locally-generated numbers from whatever hardware the user
ran on, and the published doc's tables stay a manually-reviewed, hand-edited
artifact. See docs/development/benchmarks-guide.md for why.
"""

from __future__ import annotations

from statistics import mean, stdev
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
    if sum(raw) <= 0:
        raise ValueError("Cold start has no measured phases")
    return raw


def _spread(values: list[float | None], digits: int = 3) -> str:
    """Mean ± sample standard deviation, or the raw value when there is one."""
    present = [value for value in values if value is not None]
    if not present:
        return "-"
    if len(present) == 1:
        return _fmt(present[0], digits)
    return f"{_fmt(mean(present), digits)} ± {_fmt(stdev(present), digits)}"


def _spread_bytes(values: list[int | None]) -> str:
    present = [value for value in values if value is not None]
    if not present:
        return "-"
    if len(present) == 1:
        return _fmt_bytes(present[0])
    return f"{mean(present) / 1e9:.2f} ± {stdev(present) / 1e9:.2f} GB"


def _capture_groups(results: list[dict]) -> list[tuple[str, list[dict]]]:
    """Repeats of one model, in the order the models first appear."""
    groups: dict[str, list[dict]] = {}
    order: list[str] = []
    for result in results:
        if not result.get("checkpoint_agent_log"):
            continue
        label = (result.get("model") or {}).get("label", "-")
        if label not in groups:
            order.append(label)
            groups[label] = []
        groups[label].append(result)
    return [(label, groups[label]) for label in order]


def _criu_read(capture: dict) -> float | None:
    """memdump minus memwrite, computed here so results saved before the
    ``criu_read`` property existed still render it."""
    memdump, memwrite = capture.get("criu_memdump"), capture.get("criu_memwrite")
    if memdump is None or memwrite is None or memwrite > memdump:
        return None
    return memdump - memwrite


def _capture_field(result: dict, name: str) -> float | None:
    return (result.get("checkpoint_agent_log") or {}).get(name)


def _capture_phase(result: dict, name: str) -> float | None:
    return ((result.get("checkpoint_agent_log") or {}).get("phases") or {}).get(name)


def _staging_bytes(result: dict) -> int | None:
    capture = result.get("checkpoint_agent_log") or {}
    return capture.get("checkpoint_dir_bytes_before_overlay", capture.get("criu_image_bytes"))


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
            f"sparse {_fmt_bytes(image.get('pages_sparse_bytes'))}."
        )
        lines.append("")
        lines.append(
            f"Pagemap holes {image.get('pagemap_holes', 0)} "
            f"({_fmt_bytes(image.get('pagemap_hole_bytes'))}) are pages CRIU "
            "skipped because a parent image already holds them. That count is "
            "`pages_skipped_parent` and is zero without `--prev-images-dir`. "
            "It is not file sparseness."
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


def _capture_sections(results: list[dict]) -> list[str]:
    """Capture breakdown. Skips results that have no checkpoint agent log."""
    lines = [
        "## Where the capture time goes",
        "",
        "Wall, CUDA, CRIU, and overlay come from the capture-node agent's "
        "Checkpoint timing summary. CUDA lock is the device freeze; CUDA copy "
        "moves device state into host memory before CRIU runs. Those CUDA "
        "columns are blank when the capture used PageBroker CustomStorage, "
        "which records the CUDA total and does not measure the split. CRIU "
        "memdump is CRIU's page-dump timer and it includes memwrite, the time "
        "spent writing pages. CRIU read is memdump minus memwrite. memdump "
        "and memwrite overlap the CRIU wall; they are not added to it or to "
        "each other. Checkpoint (API) is the one-second Kubernetes Ready "
        "timestamp and is coarser than Wall. Repeated runs of the same model "
        "are one row, the mean ± the sample standard deviation. A single run "
        "is the raw value.",
        "",
        "| Model | Wall (s) | CUDA lock | CUDA copy | CRIU wall | CRIU freeze | "
        "CRIU memdump | of which memwrite | CRIU read | Overlay | Checkpoint (API) |",
        "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for label, group in _capture_groups(results):
        lines.append(
            "| {label} | {wall} | {lock} | {copy} | {criu} | {freeze} | {memdump} | "
            "{memwrite} | {read} | {overlay} | {api} |".format(
                label=label,
                wall=_spread([_capture_field(result, "duration") for result in group]),
                lock=_spread([_capture_field(result, "cuda_lock") for result in group]),
                copy=_spread([_capture_field(result, "cuda_copy") for result in group]),
                criu=_spread([_capture_phase(result, "criu_dump") for result in group]),
                freeze=_spread([_capture_field(result, "criu_freezing") for result in group]),
                memdump=_spread([_capture_field(result, "criu_memdump") for result in group]),
                memwrite=_spread([_capture_field(result, "criu_memwrite") for result in group]),
                read=_spread([_criu_read(result.get("checkpoint_agent_log") or {}) for result in group]),
                overlay=_spread([_capture_phase(result, "overlay_capture") for result in group]),
                api=_spread(
                    [
                        (result.get("checkpoint") or {}).get("checkpoint_seconds")
                        for result in group
                    ]
                ),
            )
        )
    lines.append("")
    lines.extend(
        [
            "## How the pages were written",
            "",
            "Memwrite GB/s divides CRIU's written page bytes (`pages_written`) "
            "by CRIU's memwrite timer. The staging directory column is the "
            "apparent size of the checkpoint directory after CRIU returns and "
            "before the rootfs diff is added. It includes the manifest, "
            "dump.log, CUDA job file, and criu.conf, so it is neither the CRIU "
            "image nor the final checkpoint size, and it is not turned into a "
            "rate against the dump wall. Repeated runs of the same model are "
            "one row, the mean ± the sample standard deviation.",
            "",
            "| Model | Pages written | Staging dir before overlay | Overlay diff | Memwrite GB/s |",
            "|---|---:|---:|---:|---:|",
        ]
    )
    for label, group in _capture_groups(results):
        lines.append(
            "| {label} | {pages} | {staging} | {overlay} | {memwrite_rate} |".format(
                label=label,
                pages=_spread_bytes(
                    [_capture_field(result, "criu_pages_written_bytes") for result in group]
                ),
                staging=_spread_bytes([_staging_bytes(result) for result in group]),
                overlay=_spread_bytes(
                    [_capture_field(result, "overlay_diff_bytes") for result in group]
                ),
                memwrite_rate=_spread(
                    [_capture_field(result, "memwrite_gigabytes_per_second") for result in group]
                ),
            )
        )
    lines.append("")
    return lines


def _agent_log_warnings(results: list[dict]) -> list[tuple[str, str]]:
    return [
        (r.get("model", {}).get("label", "?"), w)
        for r in results
        for w in (
            ((r.get("agent_log_phases") or {}).get("parse_warnings") or [])
            + ((r.get("checkpoint_agent_log") or {}).get("parse_warnings") or [])
            + ((r.get("checkpoint_image") or {}).get("parse_warnings") or [])
        )
    ]


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
        topology = f"TP{model['tp']}"
        if model["ep"] > 1:
            topology = "TEP8" if model["tp"] == model["ep"] == 8 else f"{topology}/EP{model['ep']}"
        prefix = [model["label"], engine["name"], topology]
        elapsed = r["cold_start"]["startup_to_ready_seconds"]
        timings = [
            a["measurement"]
            for a in r.get("restore_attempts", [])
            if a.get("measurement")
        ]
        if not timings and (r.get("restore") or {}).get("measurement"):
            timings = [r["restore"]["measurement"]]
        restored = mean(t["total_s"] for t in timings) if timings else None

        def spread(key):
            values = [t[key] for t in timings]
            return (
                f(mean(values)) + " ± " + (f(stdev(values)) if len(values) > 1 else "—")
            )

        headline.append(
            prefix
            + [
                _fmt_bytes(model.get("reported_weights_bytes")),
                _fmt_bytes(model.get("checkpoint_artifact_bytes")),
                f((r.get("checkpoint") or {}).get("checkpoint_seconds")),
                f(elapsed),
                spread("total_s") if timings else "-",
                f(elapsed / restored) + "×" if restored and elapsed is not None else "-",
            ]
        )
        measurement = r["cold_start"]["measurement"] or {}
        if measurement.get("phases_s"):
            cold.append(
                prefix
                + [f(v) for v in _cold_phases(measurement)]
                + [f(elapsed)]
            )
        if timings:
            restore.append(
                prefix
                + [
                    spread(k)
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
            "Restore (s, mean ± SD)",
            "Speedup",
        ],
        headline,
    )
    text += "\n\nThe report averages the recorded restore times and shows their sample standard deviation. Cold-start and checkpoint values are reported without a standard deviation. Speedup uses the mean restore time. Older single-attempt results show — for standard deviation.\n"
    if cold:
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
    text += "\n\nEach run saves the raw timings.\n\n## Where the restore time goes (s)\n\n"
    text += _table(
        [
            "Model",
            "Engine",
            "Parallelism",
            "Agent setup / rootfs",
            "CRIU restore",
            "CUDA restore",
            "Wake",
            "Total",
        ],
        restore,
    )
    text += "\n\nRestore ends at wake/resume completion, before inference. CUDA restore includes multi-GPU reconstruction.\n\n## Observed environment\n\n"
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
    if any(r.get("checkpoint_agent_log") for r in current):
        text += "\n" + "\n".join(_capture_sections(current))
    warnings = [
        f"- {r['model']['label']}: {warning}"
        for r in current
        for warning in r.get("warnings", [])
    ] + [f"- {label}: {warning}" for label, warning in _agent_log_warnings(current)]
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
        "docs/overview/performance.md on equivalent hardware (same GPU model, "
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

    if any(r.get("checkpoint_agent_log") for r in results):
        lines.extend(_capture_sections(results))

    if any(r.get("checkpoint_image") for r in results):
        lines.extend(_image_section(results))

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
    agent_log_warnings = _agent_log_warnings(results)
    if warnings or agent_log_warnings:
        lines.append("## Warnings")
        lines.append("")
        for label, warning in warnings + agent_log_warnings:
            lines.append(f"- **{label}**: {warning}")
        lines.append("")

    return "\n".join(lines) + "\n"
