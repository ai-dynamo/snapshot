# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Every GPU suite that creates a vCluster holds a suite slot while it does."""

from __future__ import annotations

import importlib.util
import re
import sys
from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
SCRIPT = ROOT / "hack" / "e2e-suite-slot.py"
GPU_RUNNER = "prod-deploy-tester-v1"
TAKE = "python3 hack/e2e-suite-slot.py take"
RELEASE = "python3 hack/e2e-suite-slot.py release"
HOST_KUBECONFIG = "${{ steps.host.outputs.kubeconfig }}"


def creates_vcluster(step: dict) -> bool:
    return step.get("uses") == "./.github/actions/e2e-vcluster-up" or "--phase vcluster" in step.get("run", "")


def gpu_suites() -> list:
    suites = []
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        for name, job in (yaml.safe_load(path.read_text()).get("jobs") or {}).items():
            if job.get("runs-on") == GPU_RUNNER and any(creates_vcluster(step) for step in job.get("steps", [])):
                suites.append(pytest.param(job, id=f"{path.name}:{name}"))
    return suites


def index_of(job: dict, run: str) -> int:
    matches = [index for index, step in enumerate(job["steps"]) if step.get("run") == run]
    assert len(matches) == 1, f"expected one step running {run!r}"
    return matches[0]


@pytest.fixture(scope="module")
def default_wait_minutes() -> int:
    spec = importlib.util.spec_from_file_location("e2e_suite_slot_defaults", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    try:
        spec.loader.exec_module(module)
    finally:
        del sys.modules[spec.name]
    return module.DEFAULT_TIMEOUT_MINUTES


def test_every_gpu_suite_is_covered():
    assert {suite.id for suite in gpu_suites()} == {
        "e2e-frameworks.yaml:e2e",
        "e2e-upgrade.yaml:upgrade",
        "e2e.yaml:e2e",
    }


@pytest.mark.parametrize("job", gpu_suites())
def test_a_suite_takes_a_slot_before_it_creates_its_vcluster(job, default_wait_minutes):
    take = index_of(job, TAKE)
    create = next(index for index, step in enumerate(job["steps"]) if creates_vcluster(step))
    assert take < create
    step = job["steps"][take]
    assert step.get("if") == job["steps"][create].get("if")
    assert step["env"]["KUBECONFIG"] == HOST_KUBECONFIG
    assert "HOST_NAMESPACE" in job["env"]
    # The script reports who holds the slots only if it gives up before the step is killed.
    assert step["timeout-minutes"] > default_wait_minutes


@pytest.mark.parametrize("job", gpu_suites())
def test_a_suite_releases_its_slot_after_its_cleanup_even_when_it_fails(job):
    release = index_of(job, RELEASE)
    cleanups = [index for index, step in enumerate(job["steps"]) if step.get("name") == "Cleanup e2e vCluster"]
    assert cleanups and release > max(cleanups)
    step = job["steps"][release]
    assert step["if"].startswith("${{ always()")
    assert step["env"]["KUBECONFIG"] == HOST_KUBECONFIG


@pytest.mark.parametrize("job", gpu_suites())
def test_the_job_timeout_leaves_room_for_the_slot_wait(job):
    wait = job["steps"][index_of(job, TAKE)]["timeout-minutes"]
    release = job["steps"][index_of(job, RELEASE)]["timeout-minutes"]
    timeouts = [int(value) for value in re.findall(r"\d+", str(job["timeout-minutes"]))]
    assert timeouts and min(timeouts) > wait + release


def test_no_workflow_caps_the_computed_limit():
    for path in WORKFLOWS.glob("*.y*ml"):
        assert "--max-running" not in path.read_text(), path.name


def test_changing_the_slot_script_runs_the_pr_gpu_suites():
    workflow = yaml.safe_load((WORKFLOWS / "e2e-pr.yaml").read_text())
    step = next(step for step in workflow["jobs"]["changes"]["steps"] if step.get("id") == "filter")
    assert "hack/e2e-suite-slot.py" in yaml.safe_load(step["with"]["filters"])["e2e"]
