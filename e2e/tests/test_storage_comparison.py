# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from snapshot_e2e import storage_comparison as comparison


def result(case: str, values: dict[str, float], *, outcome: str = "passed") -> dict[str, Any]:
    return {
        "schemaVersion": 1,
        "benchmarkVersion": 1,
        "identity": {
            "suite": comparison.SUITE,
            "case": case,
            "test": "test_framework_checkpoint_restore_serves_inference",
            "runId": "1",
            "runAttempt": 1,
        },
        "outcome": outcome,
        "startedAt": "2026-10-28T10:00:00Z",
        "finishedAt": "2026-10-28T10:10:00Z",
        "source": {},
        "environment": {"sourceGpus": [{"model": "NVIDIA H100"}]},
        "measurements": [
            {
                "name": name,
                "displayName": name.replace(".", " "),
                "unit": "seconds",
                "status": "complete",
                "value": value,
            }
            for name, value in values.items()
        ],
        "events": [],
        "error": None,
    }


def write(directory: Path, data: dict[str, Any]) -> None:
    directory.mkdir(exist_ok=True)
    name = f"{data['identity']['case']}.json"
    (directory / name).write_text(json.dumps(data))


def test_cases_are_paired_with_the_difference_shown(tmp_path: Path) -> None:
    write(tmp_path / "pvc", result("vllm", {"restore.to_traffic.duration": 50.0}))
    write(tmp_path / "s3", result("vllm", {"restore.to_traffic.duration": 35.0}))
    text = comparison.render(
        ["vllm"],
        comparison.load_results(tmp_path / "pvc"),
        comparison.load_results(tmp_path / "s3"),
    )
    assert "| restore to_traffic duration | 50.0 s | 35.0 s | -15.0 s (-30%) |" in text


def test_a_framework_missing_on_one_side_is_reported_not_dropped(tmp_path: Path) -> None:
    write(tmp_path / "pvc", result("sglang", {"checkpoint.duration": 60.0}))
    (tmp_path / "s3").mkdir()
    text = comparison.render(
        ["sglang", "vllm"],
        comparison.load_results(tmp_path / "pvc"),
        comparison.load_results(tmp_path / "s3"),
    )
    assert "| checkpoint duration | 60.0 s | n/a | n/a |" in text
    assert "### vllm" in text
    assert "no completed measurements" in text


def test_headline_measurements_come_first() -> None:
    names = ["restore.criu_restore.duration", "checkpoint.duration", "restore.to_traffic.duration"]
    assert comparison.ordered(names) == [
        "checkpoint.duration",
        "restore.to_traffic.duration",
        "restore.criu_restore.duration",
    ]


def test_the_report_names_the_gpu_so_a_mismatch_is_visible(tmp_path: Path) -> None:
    write(tmp_path / "pvc", result("vllm", {"checkpoint.duration": 1.0}))
    write(tmp_path / "s3", result("vllm", {"checkpoint.duration": 1.0}, outcome="failed"))
    text = comparison.render(
        ["vllm"],
        comparison.load_results(tmp_path / "pvc"),
        comparison.load_results(tmp_path / "s3"),
    )
    assert "PVC: passed, NVIDIA H100. S3: failed, NVIDIA H100." in text
