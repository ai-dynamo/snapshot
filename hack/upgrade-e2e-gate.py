# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Choose which upgrade e2e pairs to run.

A pair is one version to upgrade from, one upgrade config, and one scenario
profile. Every pair runs when --always is set (manual and weekly runs). For
nightly runs, a pair runs only if it never passed in a scheduled run on main,
if its last passing commit is not in the current history, or if anything that
shapes the upgrade changed since then. If the run history cannot be read, every
pair runs. Prints the JSON list of pairs to run.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Callable


GITHUB_API_VERSION = "2022-11-28"
DEFAULT_REPOSITORY = "ai-dynamo/snapshot"
DEFAULT_WORKFLOW = "e2e-upgrade.yaml"
WATCHED_PATHS = (
    "agent",
    "operator",
    "api",
    "charts",
    "e2e",
    ".github/workflows/e2e-upgrade.yaml",
    ".github/actions",
    "hack/resolve-snapshot-image-tag.py",
    "hack/upgrade-e2e-gate.py",
)


def job_name(pair: dict[str, str]) -> str:
    return f"upgrade ({pair['profile']}, {pair['from']}, {pair['config']})"


def build_pairs(versions: list[dict[str, str]], configs: list[str], profile: str) -> list[dict[str, str]]:
    return [
        {"from": version["tag"], "chart_version": version["chart_version"], "config": config, "profile": profile}
        for version in versions
        for config in configs
    ]


def decide(
    pair: dict[str, str],
    *,
    always: bool,
    last_success: str | None,
    is_ancestor: Callable[[str], bool],
    changed_since: Callable[[str], bool],
) -> tuple[bool, str]:
    if always:
        return True, "manual or weekly run"
    if last_success is None:
        return True, "no passing run on main yet"
    if not is_ancestor(last_success):
        return True, f"last pass {last_success[:8]} is not in the current history"
    if changed_since(last_success):
        return True, f"changes since last pass {last_success[:8]}"
    return False, f"no relevant changes since last pass {last_success[:8]}"


def github_json(url: str, headers: dict[str, str]) -> dict:
    request = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def last_successes(
    names: set[str],
    *,
    repository: str,
    workflow: str,
    branch: str,
    max_runs: int,
    headers: dict[str, str],
) -> dict[str, str]:
    base = f"https://api.github.com/repos/{repository}/actions"
    # Manual runs may override the target image or scenario set while keeping
    # the same job name, so they cannot establish nightly coverage.
    query = urllib.parse.urlencode(
        {"branch": branch, "status": "completed", "event": "schedule", "per_page": max_runs}
    )
    try:
        runs = github_json(f"{base}/workflows/{workflow}/runs?{query}", headers).get("workflow_runs", [])
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise
        runs = []
    found: dict[str, str] = {}
    for run in runs:
        if names <= found.keys():
            break
        jobs = github_json(f"{base}/runs/{run['id']}/jobs?per_page=100", headers).get("jobs", [])
        for job in jobs:
            if job.get("conclusion") == "success" and job.get("name") in names:
                found.setdefault(job["name"], run["head_sha"])
    return found


def is_ancestor(sha: str) -> bool:
    return subprocess.run(["git", "merge-base", "--is-ancestor", sha, "HEAD"], capture_output=True).returncode == 0


def changed_since(sha: str) -> bool:
    result = subprocess.run(["git", "diff", "--quiet", sha, "HEAD", "--", *WATCHED_PATHS], capture_output=True)
    return result.returncode != 0


def write_outputs(selected: list[dict[str, str]], decisions: list[tuple[dict[str, str], bool, str]]) -> None:
    encoded = json.dumps(selected, separators=(",", ":"))
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write(f"pairs={encoded}\n")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        lines = ["### Upgrade pairs", "", "| From | Config | Profile | Decision | Reason |", "| --- | --- | --- | --- | --- |"]
        for pair, run, reason in decisions:
            lines.append(
                f"| `{pair['from']}` | `{pair['config']}` | `{pair['profile']}` | {'run' if run else 'skip'} | {reason} |"
            )
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n\n")
    print(encoded)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--versions", required=True, help="JSON list of {tag, chart_version} to upgrade from")
    parser.add_argument("--configs", required=True, help="comma-separated upgrade configs")
    parser.add_argument("--profile", required=True, help="scenario profile")
    parser.add_argument("--always", action="store_true", help="run every pair without checking history")
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY") or DEFAULT_REPOSITORY)
    parser.add_argument("--workflow", default=DEFAULT_WORKFLOW)
    parser.add_argument("--branch", default="main")
    parser.add_argument("--max-runs", type=int, default=50)
    args = parser.parse_args()

    configs = [config.strip() for config in args.configs.split(",") if config.strip()]
    pairs = build_pairs(json.loads(args.versions), configs, args.profile)

    successes: dict[str, str] = {}
    history_error: str | None = None
    if not args.always and pairs:
        headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": GITHUB_API_VERSION}
        token = os.environ.get("GH_TOKEN")
        if token:
            headers["Authorization"] = f"Bearer {token}"
        try:
            successes = last_successes(
                {job_name(pair) for pair in pairs},
                repository=args.repository,
                workflow=args.workflow,
                branch=args.branch,
                max_runs=args.max_runs,
                headers=headers,
            )
        except (OSError, ValueError) as exc:
            history_error = f"run history unavailable ({exc})"
            print(f"warning: {history_error}; running every pair", file=sys.stderr)

    decisions = []
    for pair in pairs:
        if history_error:
            run, reason = True, history_error
        else:
            run, reason = decide(
                pair,
                always=args.always,
                last_success=successes.get(job_name(pair)),
                is_ancestor=is_ancestor,
                changed_since=changed_since,
            )
        decisions.append((pair, run, reason))
        print(f"{'run' if run else 'skip'} {job_name(pair)}: {reason}", file=sys.stderr)

    write_outputs([pair for pair, run, _ in decisions if run], decisions)
    return 0


if __name__ == "__main__":
    sys.exit(main())
