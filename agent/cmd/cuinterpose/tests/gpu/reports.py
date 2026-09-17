# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""CUDA-independent coordinator JSONL report reader."""

import json


def parse_phase_lines(stdout: str) -> dict[str, dict[str, str | int | float]]:
    phases: dict[str, dict[str, str | int | float]] = {}
    for line in stdout.splitlines():
        try:
            report = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(report, dict):
            continue
        phase = report.pop("phase", None)
        if not isinstance(phase, str) or not phase or report.get("status") != "ok":
            continue
        phases[phase] = report
    return phases
