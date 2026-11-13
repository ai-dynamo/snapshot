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


def test_gpu_suites_wait_for_the_verified_candidate_and_make_check(jobs):
    for suite in GPU_SUITES:
        assert {"candidate", "check"} <= set(jobs[suite]["needs"])
    assert step(jobs["check"], "Run make check")["run"] == "make check"


def test_gpu_suites_check_the_running_images_against_the_candidate(jobs):
    verify = step(jobs["candidate"], "Check each tag resolves to the digest built here")
    assert jobs["candidate"]["outputs"]["digests"] == f"${{{{ steps.{verify['id']}.outputs.digests }}}}"
    assert 'echo "digests=${record}" >> "${GITHUB_OUTPUT}"' in verify["run"]
    for suite in GPU_SUITES:
        assert jobs[suite]["with"]["image_digests"] == "${{ needs.candidate.outputs.digests }}"
        called = yaml.safe_load((ROOT / jobs[suite]["uses"]).read_text())
        assert "image_digests" in called[True]["workflow_call"]["inputs"]
        env = called["jobs"]["e2e"]["env"]
        assert env["SNAPSHOT_E2E_IMAGE_DIGESTS"] == "${{ inputs.image_digests || '' }}"


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
