# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from pathlib import Path

import pytest
import yaml


WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
MIRROR_BRANCH = "pull-request/[0-9]+"
ON_PUSH = "${{ github.event_name == 'push' }}"


def load(name: str) -> dict:
    return yaml.safe_load((WORKFLOWS / name).read_text())


def triggers(workflow: dict) -> dict:
    # YAML reads the bare key `on` as the boolean true.
    on = workflow.get(True)
    return on if isinstance(on, dict) else {}


@pytest.fixture(scope="module")
def workflow() -> dict:
    return load("cuinterpose-gpu.yaml")


def test_no_mirror_push_workflow_filters_paths_in_its_trigger():
    # copy-pr-bot creates the mirror branch at a commit the repository already
    # has. That push carries no commit, so a trigger filter finds no changed
    # file and the workflow never starts for a pull request's first push.
    mirrored = set()
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        push = triggers(load(path.name)).get("push") or {}
        if MIRROR_BRANCH in (push.get("branches") or []):
            mirrored.add(path.name)
            assert not {"paths", "paths-ignore"} & set(push), path.name
    assert {"cuinterpose-gpu.yaml", "e2e-pr.yaml"} <= mirrored


def test_gpu_runner_admits_only_trusted_events(workflow):
    on = triggers(workflow)
    assert set(on) == {"push", "schedule", "workflow_dispatch"}
    # Mirror branches only, and no tag: copy-pr-bot writes them for vetted commits.
    assert on["push"]["branches"] == [MIRROR_BRANCH]
    assert "tags" not in on["push"]


def test_changes_job_decides_from_the_whole_pull_request(workflow):
    changes = workflow["jobs"]["changes"]
    # A skipped job skips every job that needs it, and the nightly run with them.
    assert "if" not in changes
    assert changes["runs-on"] == "ubuntu-24.04"
    checkout, paths_filter = changes["steps"]
    assert checkout["uses"].startswith("actions/checkout@")
    assert checkout["with"] == {"persist-credentials": False}
    assert checkout["if"] == paths_filter["if"] == ON_PUSH
    # The pinned action of PR E2E, with no base: it compares against the
    # merge-base with the default branch, not against the previous push.
    pr_e2e = load("e2e-pr.yaml")["jobs"]["changes"]["steps"]
    assert paths_filter["uses"] in {step.get("uses") for step in pr_e2e}
    assert set(paths_filter["with"]) == {"filters"}
    assert yaml.safe_load(paths_filter["with"]["filters"]) == {
        "cuinterpose": [
            "agent/cmd/cuinterpose/**",
            "agent/Dockerfile",
            ".github/workflows/cuinterpose-gpu.yaml",
        ]
    }
    assert changes["outputs"] == {
        "cuinterpose": f"${{{{ steps.{paths_filter['id']}.outputs.cuinterpose }}}}"
    }


def test_gpu_tests_run_for_a_changed_pull_request_and_for_every_other_event(workflow):
    jobs = workflow["jobs"]
    assert set(jobs) == {"changes", "check", "artifacts", "gpu"}
    assert jobs["check"]["needs"] == "changes"
    assert jobs["check"]["if"] == (
        "${{ github.event_name != 'push' || needs.changes.outputs.cuinterpose == 'true' }}"
    )
    assert jobs["artifacts"]["needs"] == "check"
    assert jobs["gpu"]["needs"] == "artifacts"
    # Default job conditions block failed or skipped prerequisites.
    assert "if" not in jobs["artifacts"]
    assert "if" not in jobs["gpu"]
    # Only the last job leaves the hosted runners, after everything else passed.
    assert [name for name, job in jobs.items() if job["runs-on"] != "ubuntu-24.04"] == ["gpu"]
