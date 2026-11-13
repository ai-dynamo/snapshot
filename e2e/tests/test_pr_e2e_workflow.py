# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys

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


def test_ci_helper_changes_trigger_e2e(jobs):
    filters = yaml.safe_load(step(jobs["changes"], "Filter paths")["with"]["filters"])
    assert {"hack/ci-build-image.sh", "hack/ci-prepare-cpu-e2e.sh"} <= set(filters["e2e"])


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


def test_frameworks_reuse_validation_for_only_the_callers_commit(jobs):
    caller = jobs["frameworks"]
    assert "check" in caller["needs"]
    assert caller["with"]["validated_sha"] == "${{ github.sha }}"
    called = yaml.safe_load((ROOT / caller["uses"]).read_text())
    inputs = called[True]["workflow_call"]["inputs"]
    assert inputs["validated_sha"]["required"] is False
    assert inputs["validated_sha"]["type"] == "string"
    assert inputs["validated_sha"].get("default", "") == ""
    # An unset input on a schedule/dispatch or a different SHA runs make check.
    assert called["jobs"]["check"]["if"] == "${{ inputs.validated_sha != github.sha }}"
    assert called["jobs"]["e2e"]["if"] == (
        "${{ !cancelled() && (needs.check.result == 'success' || "
        "(needs.check.result == 'skipped' && inputs.validated_sha == github.sha)) }}"
    )


@pytest.mark.parametrize("failed_phase", ["", "not serial", "serial"])
def test_native_parallel_and_serial_phases_preserve_failures(tmp_path, failed_phase):
    workflow = yaml.safe_load((ROOT / ".github/workflows/e2e.yaml").read_text())
    native = workflow["jobs"]["e2e"]
    script = step(native, "Run snapshot and SnapshotJob lifecycle tests")["run"]
    # Run the actual workflow script with pytest replaced by a recorder. A
    # failing parallel phase must still run serial cases and fail the step.
    uv = tmp_path / "uv"
    uv.write_text(
        f"#!{sys.executable}\n"
        "import json, os, sys\n"
        "with open(os.environ['CALL_LOG'], 'a') as f:\n"
        "    f.write(json.dumps(sys.argv[1:]) + '\\n')\n"
        "phase = sys.argv[sys.argv.index('-m') + 1]\n"
        "sys.exit(7 if phase == os.environ['FAILED_PHASE'] else 0)\n"
    )
    uv.chmod(0o755)
    call_log = tmp_path / "calls.jsonl"
    result = subprocess.run(
        ["bash", "-c", script],
        env={
            **os.environ,
            "PATH": f"{tmp_path}:{os.environ['PATH']}",
            "RUNNER_TEMP": str(tmp_path),
            "CALL_LOG": str(call_log),
            "FAILED_PHASE": failed_phase,
        },
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == (7 if failed_phase else 0), result.stderr
    parallel, serial = [json.loads(line) for line in call_log.read_text().splitlines()]
    assert parallel[parallel.index("-m") + 1] == "not serial"
    assert parallel[parallel.index("-n") + 1] == "2"
    assert serial[serial.index("-m") + 1] == "serial"
    assert "-n" not in serial
    for args in (parallel, serial):
        assert {arg for arg in args if arg.endswith(".py")} == {
            "e2e/tests/test_snapshot_lifecycle.py",
            "e2e/tests/test_snapshotjob.py",
        }
        assert "--durations=20" in args
        assert any(arg.startswith("--junitxml=") for arg in args)
