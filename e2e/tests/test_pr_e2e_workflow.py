# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "e2e-pr.yaml"
FRAMEWORKS_WORKFLOW = ROOT / ".github" / "workflows" / "e2e-frameworks.yaml"
HOLDER = '--holder "${{ github.run_id }}-${{ github.run_attempt }}"'
GPU_SUITES = ("gpu", "frameworks")


@pytest.fixture(scope="module")
def jobs() -> dict:
    return yaml.safe_load(WORKFLOW.read_text())["jobs"]


def step(job: dict, name: str) -> dict:
    matches = [s for s in job["steps"] if s.get("name") == name]
    assert len(matches) == 1, f"expected one step named {name!r}"
    return matches[0]


def test_gpu_suites_wait_for_the_verified_candidate_and_the_datadog_gate(jobs):
    for suite in GPU_SUITES:
        assert {"candidate", "datadog-gate"} <= set(jobs[suite]["needs"])


def test_candidate_checks_every_built_image(jobs):
    images = jobs["images"]["strategy"]["matrix"]["image"]
    script = step(jobs["candidate"], "Check each tag resolves to the digest built here")["run"]
    assert f"for image in {' '.join(images)}; do" in script
    record = step(jobs["images"], "Record digest")
    assert record["env"]["DIGEST"] == "${{ steps.build.outputs.digest }}"


def test_the_framework_suite_leaves_datadog_to_the_caller(jobs):
    # A called workflow shares the caller's run ID, so a toggle of its own would
    # hold and release the caller's hold.
    assert jobs["frameworks"]["with"]["datadog_gpu_monitoring"] == "leave"
    frameworks = yaml.safe_load(FRAMEWORKS_WORKFLOW.read_text())
    assert "datadog_gpu_monitoring" in frameworks[True]["workflow_call"]["inputs"]


def test_datadog_is_held_across_both_gpu_suites(jobs):
    gate = jobs["datadog-gate"]
    restore = jobs["datadog-restore"]
    assert {"datadog-gate", *GPU_SUITES} <= set(restore["needs"])
    assert restore["if"].startswith("${{ always() &&")
    assert "needs.datadog-gate.result == 'success'" in restore["if"]
    assert restore["concurrency"]["group"] == gate["concurrency"]["group"]
    assert HOLDER in step(gate, "Disable Datadog GPU monitoring for this run")["run"]
    assert HOLDER in step(restore, "Restore Datadog GPU monitoring")["run"]


def test_pr_e2e_requires_every_other_job(jobs):
    # A job left out here could fail without failing the required check.
    aggregator = jobs["pr-e2e"]
    others = set(jobs) - {"pr-e2e"}
    assert set(aggregator["needs"]) == others
    assert aggregator["if"] == "${{ always() }}"
    script = step(aggregator, "Require every needed suite to pass")["run"]
    for job in others - {"changes"}:
        selector = f".{job}" if job.isidentifier() else f'.["{job}"]'
        assert selector in script, f"PR E2E does not check {job}"
