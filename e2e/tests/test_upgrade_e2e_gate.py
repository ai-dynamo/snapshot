# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import importlib.util
import io
import json
import sys
import urllib.error
from pathlib import Path
from types import ModuleType
from urllib.parse import parse_qs, urlsplit

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack" / "upgrade-e2e-gate.py"
WORKFLOW = ROOT / ".github" / "workflows" / "e2e-upgrade.yaml"
PAIR = {"from": "v0.1.0", "chart_version": "0.1.0", "config": "full", "profile": "basic"}
SHA = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"


@pytest.fixture
def gate(monkeypatch: pytest.MonkeyPatch) -> ModuleType:
    spec = importlib.util.spec_from_file_location("upgrade_e2e_gate", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in ("GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY", "GITHUB_REPOSITORY", "GH_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    return module


def decide(gate: ModuleType, **overrides) -> tuple[bool, str]:
    options = {
        "always": False,
        "last_success": SHA,
        "is_ancestor": lambda sha: True,
        "changed_since": lambda sha: False,
    }
    options.update(overrides)
    return gate.decide(PAIR, **options)


def test_always_runs_every_pair(gate: ModuleType) -> None:
    assert decide(gate, always=True, last_success=None)[0] is True


def test_pair_that_never_passed_runs(gate: ModuleType) -> None:
    run, reason = decide(gate, last_success=None)

    assert run and "no passing run" in reason


def test_unchanged_pair_is_skipped(gate: ModuleType) -> None:
    run, reason = decide(gate)

    assert not run and SHA[:8] in reason


def test_changes_since_the_last_pass_run_the_pair(gate: ModuleType) -> None:
    run, reason = decide(gate, changed_since=lambda sha: True)

    assert run and "changes since" in reason


def test_last_pass_outside_the_current_history_runs_the_pair(gate: ModuleType) -> None:
    run, reason = decide(gate, is_ancestor=lambda sha: False, changed_since=lambda sha: False)

    assert run and "not in the current history" in reason


def test_pairs_cover_every_version_and_config(gate: ModuleType) -> None:
    versions = [{"tag": "v0.3.0", "chart_version": "0.3.0"}, {"tag": "v0.2.1", "chart_version": "0.2.1"}]

    pairs = gate.build_pairs(versions, ["full", "agent-first"], "all")

    assert [(pair["from"], pair["config"], pair["profile"]) for pair in pairs] == [
        ("v0.3.0", "full", "all"),
        ("v0.3.0", "agent-first", "all"),
        ("v0.2.1", "full", "all"),
        ("v0.2.1", "agent-first", "all"),
    ]
    assert pairs[0]["chart_version"] == "0.3.0"


def test_job_name_matches_the_matrix_job_name(gate: ModuleType) -> None:
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    template = workflow["jobs"]["upgrade"]["name"]

    rendered = (
        template.replace("${{ matrix.profile }}", PAIR["profile"])
        .replace("${{ matrix.from }}", PAIR["from"])
        .replace("${{ matrix.config }}", PAIR["config"])
    )

    assert rendered == gate.job_name(PAIR) == "upgrade (basic, v0.1.0, full)"


def test_last_successes_reads_the_newest_passing_job_per_pair(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    other = dict(PAIR, config="agent-first")
    responses = {
        "runs": {"workflow_runs": [{"id": 2, "head_sha": "new" * 13 + "a"}, {"id": 1, "head_sha": SHA}]},
        "2": {"jobs": [{"name": gate.job_name(PAIR), "conclusion": "failure"}]},
        "1": {"jobs": [{"name": gate.job_name(PAIR), "conclusion": "success"}, {"name": gate.job_name(other), "conclusion": "success"}]},
    }
    urls = []

    def github_json(url: str, headers: dict) -> dict:
        urls.append(url)
        if "/workflows/" in url:
            return responses["runs"]
        return responses[url.split("/runs/")[1].split("/")[0]]

    monkeypatch.setattr(gate, "github_json", github_json)

    found = gate.last_successes(
        {gate.job_name(PAIR), gate.job_name(other)},
        repository="ai-dynamo/snapshot",
        workflow="e2e-upgrade.yaml",
        branch="main",
        max_runs=50,
        headers={},
    )

    assert found == {gate.job_name(PAIR): SHA, gate.job_name(other): SHA}
    assert "branch=main" in urls[0] and "status=completed" in urls[0]
    assert "event=schedule" in urls[0]


@pytest.mark.parametrize("has_scheduled_pass", [False, True])
def test_manual_pass_cannot_suppress_nightly_coverage(
    gate: ModuleType, monkeypatch: pytest.MonkeyPatch, has_scheduled_pass: bool
) -> None:
    previous_sha = "b" * 40
    runs = [{"id": 2, "head_sha": SHA, "event": "workflow_dispatch"}]
    if has_scheduled_pass:
        runs.append({"id": 1, "head_sha": previous_sha, "event": "schedule"})
    name = gate.job_name(PAIR)

    def github_json(url: str, headers: dict) -> dict:
        if "/workflows/" in url:
            # Model the Actions API's event filter. A manual run can use a
            # different image or fewer scenarios with exactly the same job name.
            event = parse_qs(urlsplit(url).query).get("event", [None])[0]
            return {"workflow_runs": [run for run in runs if event is None or run["event"] == event]}
        return {"jobs": [{"name": name, "conclusion": "success"}]}

    monkeypatch.setattr(gate, "github_json", github_json)
    successes = gate.last_successes(
        {name}, repository="ai-dynamo/snapshot", workflow="e2e-upgrade.yaml", branch="main", max_runs=50, headers={}
    )
    run, _ = decide(
        gate,
        last_success=successes.get(name),
        changed_since=lambda sha: sha != SHA,
    )

    assert run, "a matching manual job cannot establish coverage for the current commit"
    assert successes == ({name: previous_sha} if has_scheduled_pass else {})


def test_unknown_workflow_means_no_passing_runs(gate: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    def github_json(url: str, headers: dict) -> dict:
        raise gate.urllib.error.HTTPError(url, 404, "Not Found", {}, io.BytesIO(b""))

    monkeypatch.setattr(gate, "github_json", github_json)

    assert gate.last_successes({"x"}, repository="r/r", workflow="w", branch="main", max_runs=1, headers={}) == {}


def test_main_writes_the_selected_pairs(gate: ModuleType, monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys) -> None:
    output = tmp_path / "output"
    summary = tmp_path / "summary"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    monkeypatch.setattr(gate, "last_successes", lambda names, **kwargs: {gate.job_name(PAIR): SHA})
    monkeypatch.setattr(gate, "is_ancestor", lambda sha: True)
    monkeypatch.setattr(gate, "changed_since", lambda sha: False)
    versions = json.dumps([{"tag": "v0.1.0", "chart_version": "0.1.0"}])
    monkeypatch.setattr(
        sys, "argv", ["upgrade-e2e-gate.py", "--versions", versions, "--configs", "full,agent-first", "--profile", "basic"]
    )

    assert gate.main() == 0

    selected = json.loads(capsys.readouterr().out)
    assert [pair["config"] for pair in selected] == ["agent-first"]
    assert output.read_text(encoding="utf-8") == f"pairs={json.dumps(selected, separators=(',', ':'))}\n"
    assert "| `v0.1.0` | `full` | `basic` | skip |" in summary.read_text(encoding="utf-8")


@pytest.mark.parametrize(
    "error",
    [
        pytest.param(lambda url: urllib.error.HTTPError(url, 502, "Bad Gateway", {}, io.BytesIO(b"")), id="http-5xx"),
        pytest.param(lambda url: urllib.error.HTTPError(url, 403, "rate limited", {}, io.BytesIO(b"")), id="rate-limit"),
        pytest.param(lambda url: urllib.error.URLError("connection reset"), id="network"),
        pytest.param(lambda url: TimeoutError("timed out"), id="timeout"),
        pytest.param(lambda url: json.JSONDecodeError("bad", "", 0), id="bad-json"),
    ],
)
def test_unreadable_history_runs_every_pair(
    gate: ModuleType, monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys, error
) -> None:
    summary = tmp_path / "summary"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))

    def github_json(url: str, headers: dict) -> dict:
        raise error(url)

    monkeypatch.setattr(gate, "github_json", github_json)
    versions = json.dumps([{"tag": "v0.1.0", "chart_version": "0.1.0"}])
    monkeypatch.setattr(
        sys, "argv", ["upgrade-e2e-gate.py", "--versions", versions, "--configs", "full,agent-first", "--profile", "basic"]
    )

    assert gate.main() == 0

    captured = capsys.readouterr()
    assert [pair["config"] for pair in json.loads(captured.out)] == ["full", "agent-first"]
    assert "running every pair" in captured.err
    assert summary.read_text(encoding="utf-8").count("| run | run history unavailable") == 2


def test_unreadable_jobs_of_a_run_runs_every_pair(gate: ModuleType, monkeypatch: pytest.MonkeyPatch, capsys) -> None:
    def github_json(url: str, headers: dict) -> dict:
        if "/jobs" in url:
            raise urllib.error.HTTPError(url, 500, "Server Error", {}, io.BytesIO(b""))
        return {"workflow_runs": [{"id": 1, "head_sha": SHA}]}

    monkeypatch.setattr(gate, "github_json", github_json)
    versions = json.dumps([{"tag": "v0.1.0", "chart_version": "0.1.0"}])
    monkeypatch.setattr(sys, "argv", ["upgrade-e2e-gate.py", "--versions", versions, "--configs", "full", "--profile", "basic"])

    assert gate.main() == 0

    assert [pair["config"] for pair in json.loads(capsys.readouterr().out)] == ["full"]


def test_nothing_selected_writes_an_empty_list(gate: ModuleType, monkeypatch: pytest.MonkeyPatch, capsys) -> None:
    monkeypatch.setattr(sys, "argv", ["upgrade-e2e-gate.py", "--versions", "[]", "--configs", "full", "--profile", "basic"])

    assert gate.main() == 0

    assert json.loads(capsys.readouterr().out) == []


def test_schedule_and_settings_match_the_gate_job(gate: ModuleType) -> None:
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    crons = [entry["cron"] for entry in workflow[True]["schedule"]]
    settings = workflow["env"]

    assert settings["UPGRADE_WEEKLY_CRON"] in crons
    assert len(crons) == 2
    assert settings["UPGRADE_FROM_MINOR_VERSIONS"] == "3"
    assert settings["UPGRADE_NIGHTLY_PROFILE"] == "basic"
    assert settings["UPGRADE_WEEKLY_PROFILE"] == "all"
