# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the tag selection of push-artifacts.yaml without a release or a registry."""

from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "push-artifacts.yaml"
REPOSITORY_URL = "https://github.com/ai-dynamo/snapshot"

# The release tags of ai-dynamo/snapshot on 2026-11-26. v0.3.0 is the highest
# stable one, and release/0.1 and release/0.2 still get patch releases.
RELEASE_TAGS = (
    "v0.1.0",
    "v0.1.0-alpha.1",
    "v0.1.0-rc.1",
    "v0.1.0-rc2",
    "v0.1.1",
    "v0.2.0",
    "v0.2.0-rc.1",
    "v0.2.0-rc.2",
    "v0.2.0-rc.3",
    "v0.2.1",
    "v0.3.0",
    "v0.3.0-rc1",
)


@pytest.fixture(scope="module")
def step() -> dict:
    steps = yaml.safe_load(WORKFLOW.read_text())["jobs"]["version"]["steps"]
    matches = [s for s in steps if s.get("name") == "Compute version and tags"]
    assert len(matches) == 1
    return matches[0]


def compute(step, tmp_path, *, version="", tags=RELEASE_TAGS, git_status=0):
    """Run the step's own script with git replaced by a recorded tag listing."""
    git = tmp_path / "git"
    git.write_text(
        f"#!{sys.executable}\n"
        "import json, os, sys\n"
        "with open(os.environ['GIT_CALLS'], 'a') as f:\n"
        "    f.write(json.dumps(sys.argv[1:]) + '\\n')\n"
        "for tag in os.environ['RELEASE_TAGS'].split():\n"
        "    print('0' * 40 + '\\trefs/tags/' + tag)\n"
        "sys.exit(int(os.environ['GIT_STATUS']))\n"
    )
    git.chmod(0o755)
    calls = tmp_path / "git-calls.jsonl"
    calls.touch()
    output = tmp_path / "github-output"
    output.touch()
    result = subprocess.run(
        # GitHub runs a step's script with bash -e.
        ["bash", "-e", "-c", step["run"]],
        env={
            **os.environ,
            "PATH": f"{tmp_path}{os.pathsep}{os.environ['PATH']}",
            "GITHUB_OUTPUT": str(output),
            "INPUT": version,
            "HEAD_SHA": "1234567890abcdef",
            "REF_NAME": "main",
            "REPOSITORY_URL": REPOSITORY_URL,
            "GIT_CALLS": str(calls),
            "GIT_STATUS": str(git_status),
            "RELEASE_TAGS": " ".join(tags),
        },
        capture_output=True,
        text=True,
        check=False,
    )
    outputs = dict(line.split("=", 1) for line in output.read_text().splitlines())
    return result, outputs, [json.loads(line) for line in calls.read_text().splitlines()]


@pytest.mark.parametrize(
    ("version", "tags"),
    [
        # A patch release of an older line must not move latest back.
        ("v0.2.2", "v0.2.2"),
        ("v0.1.2", "v0.1.2"),
        # A release publishes its tag first, so a rerun of v0.2.1 finds it.
        ("v0.2.1", "v0.2.1"),
        ("v0.3.0", "v0.3.0 latest"),
        ("v0.3.1", "v0.3.1 latest"),
        ("v0.4.0", "v0.4.0 latest"),
    ],
)
def test_latest_names_the_highest_stable_release(step, tmp_path, version, tags):
    result, outputs, _ = compute(step, tmp_path, version=version)

    assert result.returncode == 0, result.stderr
    assert outputs["version"] == version
    assert outputs["tags"] == tags
    assert outputs["release"] == "true"


def test_a_release_lists_the_tags_of_its_own_repository(step, tmp_path):
    _, _, calls = compute(step, tmp_path, version="v0.3.1")

    assert step["env"]["REPOSITORY_URL"] == "${{ github.server_url }}/${{ github.repository }}"
    # The full ref keeps out the Go module tags, such as api/v0.3.0.
    assert calls == [["ls-remote", "--tags", "--refs", REPOSITORY_URL, "refs/tags/v*"]]


@pytest.mark.parametrize(
    ("version", "tags"),
    [("v0.9.1", "v0.9.1"), ("v0.10.1", "v0.10.1 latest"), ("v0.11.0", "v0.11.0 latest")],
)
def test_release_versions_compare_as_numbers(step, tmp_path, version, tags):
    # As text, v0.9.1 sorts after v0.10.0.
    result, outputs, _ = compute(step, tmp_path, version=version, tags=("v0.9.0", "v0.10.0"))

    assert result.returncode == 0, result.stderr
    assert outputs["tags"] == tags


def test_a_newer_prerelease_does_not_hold_latest(step, tmp_path):
    tags = (*RELEASE_TAGS, "v0.4.0-rc.1")
    result, outputs, _ = compute(step, tmp_path, version="v0.3.1", tags=tags)

    assert result.returncode == 0, result.stderr
    assert outputs["tags"] == "v0.3.1 latest"


@pytest.mark.parametrize(
    ("version", "tags", "release"),
    [("v0.4.0-rc.1", "v0.4.0-rc.1", "true"), ("", "v0.0.0-g12345678", "false")],
)
def test_prereleases_and_dev_builds_never_take_latest(step, tmp_path, version, tags, release):
    result, outputs, calls = compute(step, tmp_path, version=version)

    assert result.returncode == 0, result.stderr
    assert outputs["tags"] == tags
    assert outputs["release"] == release
    assert calls == []


def test_a_release_stops_when_the_tags_cannot_be_listed(step, tmp_path):
    # Guessing either way is wrong: one guess moves latest back, the other
    # leaves it on the previous release.
    result, outputs, _ = compute(step, tmp_path, version="v0.3.1", git_status=128)

    assert result.returncode != 0
    assert "tags" not in outputs
    assert "Cannot list the release tags" in result.stderr
