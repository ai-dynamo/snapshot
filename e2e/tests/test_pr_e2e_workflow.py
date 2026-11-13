# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "e2e-pr.yaml"
GPU_SUITES = ("gpu", "frameworks")


@pytest.fixture(scope="module")
def jobs() -> dict:
    return yaml.safe_load(WORKFLOW.read_text())["jobs"]


def step(job: dict, name: str) -> dict:
    matches = [s for s in job["steps"] if s.get("name") == name]
    assert len(matches) == 1, f"expected one step named {name!r}"
    return matches[0]


def test_gpu_suites_wait_for_the_verified_candidate(jobs):
    for suite in GPU_SUITES:
        assert "candidate" in jobs[suite]["needs"]


def test_candidate_checks_every_built_image(jobs):
    images = jobs["images"]["strategy"]["matrix"]["image"]
    script = step(jobs["candidate"], "Check each tag resolves to the digest built here")["run"]
    assert f"for image in {' '.join(images)}; do" in script
    record = step(jobs["images"], "Record digest")
    assert record["env"]["DIGEST"] == "${{ steps.build.outputs.digest }}"


def test_a_rerun_replaces_the_recorded_digests(jobs):
    # Artifacts outlive an attempt, and an upload under an existing name fails.
    publish = step(jobs["images"], "Publish digest")
    assert publish["with"]["overwrite"] is True


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
