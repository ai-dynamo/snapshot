# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import importlib.util
import json
import urllib.error
from pathlib import Path
from types import ModuleType
from urllib.parse import parse_qs, urlsplit

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack" / "release-branch-e2e-gate.py"
WORKFLOW = ROOT / ".github" / "workflows" / "e2e-release-branches.yaml"
HEAD = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
OLDER = "9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e"


@pytest.fixture
def gate(monkeypatch: pytest.MonkeyPatch) -> ModuleType:
    spec = importlib.util.spec_from_file_location("release_branch_e2e_gate", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in ("GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY", "GITHUB_REPOSITORY", "GH_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    return module


def decide(gate: ModuleType, **overrides) -> tuple[bool, str]:
    options = {"force": False, "workflow_present": True, "head_sha": HEAD, "last_run_sha": OLDER}
    options.update(overrides)
    return gate.decide(**options)


def test_branch_that_moved_is_dispatched(gate: ModuleType) -> None:
    run, reason = decide(gate)

    assert run and "new commits since" in reason


def test_branch_already_covered_is_skipped(gate: ModuleType) -> None:
    run, reason = decide(gate, last_run_sha=HEAD)

    assert run is False
    assert "no new commits" in reason


def test_suite_that_never_ran_is_dispatched(gate: ModuleType) -> None:
    run, reason = decide(gate, last_run_sha=None)

    assert run and "never ran" in reason


def test_absent_suite_is_skipped(gate: ModuleType) -> None:
    """A release branch cut before a suite existed does not carry its file."""
    run, reason = decide(gate, workflow_present=False, last_run_sha=None)

    assert run is False
    assert "not present" in reason


def test_force_overrides_an_unchanged_branch(gate: ModuleType) -> None:
    run, reason = decide(gate, force=True, last_run_sha=HEAD)

    assert run and reason == "forced"


def test_force_does_not_resurrect_an_absent_suite(gate: ModuleType) -> None:
    assert decide(gate, force=True, workflow_present=False)[0] is False


def test_release_branches_are_sorted_by_version(gate: ModuleType) -> None:
    """release/1.10 is newer than release/1.9, which a string sort reverses."""
    assert gate.version_key("release/1.10") > gate.version_key("release/1.9")


def test_release_branches_filters_and_orders(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    pages = [
        [
            {"name": "main", "commit": {"sha": HEAD}},
            {"name": "release/0.2", "commit": {"sha": HEAD}},
            {"name": "release/0.10", "commit": {"sha": OLDER}},
            {"name": "release/0.9", "commit": {"sha": OLDER}},
        ]
    ]
    monkeypatch.setattr(gate, "github_json", lambda url, headers: pages.pop(0) if pages else [])

    found = gate.release_branches(repository="o/r", headers={})

    assert [item["branch"] for item in found] == ["release/0.10", "release/0.9", "release/0.2"]


def test_last_run_sha_reads_the_newest_run(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    seen: dict[str, list[str]] = {}

    def fake(url: str, headers: dict) -> dict:
        seen.update(parse_qs(urlsplit(url).query))
        return {"workflow_runs": [{"head_sha": OLDER, "conclusion": "success"}]}

    monkeypatch.setattr(gate, "github_json", fake)

    assert gate.last_run_sha(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={}) == OLDER
    assert seen["branch"] == ["release/0.2"]


def test_last_run_sha_is_none_without_history(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(gate, "github_json", lambda url, headers: {"workflow_runs": []})

    assert gate.last_run_sha(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={}) is None


def test_last_run_sha_excludes_pull_request_runs(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    """A pull request run tests a merge commit, not the branch head."""
    seen: dict[str, list[str]] = {}

    def fake(url: str, headers: dict) -> dict:
        seen.update(parse_qs(urlsplit(url).query))
        return {"workflow_runs": [{"head_sha": OLDER, "conclusion": "success"}]}

    monkeypatch.setattr(gate, "github_json", fake)
    gate.last_run_sha(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={})

    assert seen["exclude_pull_requests"] == ["true"]


def test_cancelled_run_is_not_coverage(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    """A cancelled run reached no verdict, so it must not suppress the sweep."""
    runs = {
        "workflow_runs": [
            {"head_sha": HEAD, "conclusion": "cancelled"},
            {"head_sha": OLDER, "conclusion": "failure"},
        ]
    }
    monkeypatch.setattr(gate, "github_json", lambda url, headers: runs)

    assert gate.last_run_sha(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={}) == OLDER


def test_run_in_flight_is_coverage(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    """An unfinished run has no conclusion; counting it avoids a second copy."""
    runs = {"workflow_runs": [{"head_sha": HEAD, "conclusion": None}]}
    monkeypatch.setattr(gate, "github_json", lambda url, headers: runs)

    assert gate.last_run_sha(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={}) == HEAD


def test_a_failed_lookup_does_not_dispatch_the_other_branches(
    gate: ModuleType, monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture
) -> None:
    """One flaky read must not start a GPU suite on every release branch."""
    monkeypatch.setenv("GITHUB_OUTPUT", str(tmp_path / "output"))
    monkeypatch.setattr(
        gate,
        "release_branches",
        lambda **_: [
            {"branch": "release/0.2", "head_sha": HEAD},
            {"branch": "release/0.1", "head_sha": HEAD},
        ],
    )
    monkeypatch.setattr(gate, "workflow_present", lambda **_: True)

    def flaky(*, branch: str, **_) -> str | None:
        if branch == "release/0.2":
            raise OSError("boom")
        return HEAD

    monkeypatch.setattr(gate, "last_run_sha", flaky)
    monkeypatch.setattr("sys.argv", ["gate", "--workflows", "e2e.yaml"])

    assert gate.main() == 0

    pairs = json.loads((tmp_path / "output").read_text(encoding="utf-8").split("pairs=")[1].splitlines()[0])
    assert pairs == [{"branch": "release/0.2", "workflow": "e2e.yaml"}]


def test_unknown_workflow_reports_no_history(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    def missing(url: str, headers: dict) -> dict:
        raise urllib.error.HTTPError(url, 404, "nope", {}, None)

    monkeypatch.setattr(gate, "github_json", missing)

    assert gate.last_run_sha(repository="o/r", workflow="gone.yaml", branch="release/0.2", headers={}) is None


def test_workflow_present_follows_the_contents_api(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    def present(url: str, headers: dict) -> dict:
        assert ".github/workflows/e2e.yaml" in url
        assert parse_qs(urlsplit(url).query)["ref"] == ["release/0.2"]
        return {"type": "file"}

    monkeypatch.setattr(gate, "github_json", present)

    assert gate.workflow_present(repository="o/r", workflow="e2e.yaml", branch="release/0.2", headers={}) is True


def test_workflow_absent_on_branch(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    def missing(url: str, headers: dict) -> dict:
        raise urllib.error.HTTPError(url, 404, "nope", {}, None)

    monkeypatch.setattr(gate, "github_json", missing)

    assert gate.workflow_present(repository="o/r", workflow="e2e.yaml", branch="release/0.1", headers={}) is False


def test_outputs_carry_pairs_and_any(gate: ModuleType, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    output = tmp_path / "output"
    summary = tmp_path / "summary"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    pair = {"branch": "release/0.2", "workflow": "e2e.yaml"}

    gate.write_outputs([pair], [(pair, True, "new commits since 9f8e7d6c")])

    written = output.read_text(encoding="utf-8")
    assert f"pairs={json.dumps([pair], separators=(',', ':'))}" in written
    assert "any=true" in written
    assert "release/0.2" in summary.read_text(encoding="utf-8")


def test_empty_selection_reports_any_false(gate: ModuleType, monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    output = tmp_path / "output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    pair = {"branch": "release/0.2", "workflow": "e2e.yaml"}

    gate.write_outputs([], [(pair, False, "no new commits since 1a2b3c4d")])

    assert "any=false" in output.read_text(encoding="utf-8")
    assert "pairs=[]" in output.read_text(encoding="utf-8")


def test_default_suites_cover_main_s_scheduled_ones(gate: ModuleType) -> None:
    """The sweep exists to give release branches what main gets on a schedule."""
    scheduled = set()
    for path in (ROOT / ".github" / "workflows").glob("e2e*.y*ml"):
        if path.name == WORKFLOW.name:
            continue
        triggers = yaml.safe_load(path.read_text(encoding="utf-8")).get(True, {})
        if isinstance(triggers, dict) and "schedule" in triggers:
            scheduled.add(path.name)

    assert scheduled, "no scheduled e2e suite found on main"
    assert scheduled <= set(gate.DEFAULT_WORKFLOWS)


def test_every_default_suite_accepts_a_dispatch(gate: ModuleType) -> None:
    """The sweep can only reach a suite that declares workflow_dispatch."""
    for name in gate.DEFAULT_WORKFLOWS:
        path = ROOT / ".github" / "workflows" / name
        assert path.exists(), f"{name} is in the default set but absent from main"
        triggers = yaml.safe_load(path.read_text(encoding="utf-8")).get(True, {})
        assert "workflow_dispatch" in triggers, f"{name} cannot be dispatched"


def test_sweep_workflow_is_scheduled_and_may_dispatch() -> None:
    parsed = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    triggers = parsed.get(True, {})

    assert "schedule" in triggers and "workflow_dispatch" in triggers
    assert parsed["jobs"]["sweep"]["permissions"]["actions"] == "write"
