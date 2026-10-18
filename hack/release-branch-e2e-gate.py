# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Choose which end-to-end suites to dispatch on which release branches.

GitHub runs a scheduled workflow only on the default branch, so the nightly
suites that cover main never fire on release/*. This gate runs from main and
picks the (branch, workflow) pairs a sweep should dispatch.

A pair runs when the branch has moved since that suite last ran on it: the
newest run of the suite on the branch is compared with the branch head, so a
branch nobody touched is skipped and a run already covering the head is not
duplicated. The newest run counts whatever its conclusion, because re-running
the same suite on the same commit would only repeat the result; use --force,
or dispatch the suite directly, to retry a failure in place. A suite absent
from a branch is skipped: release branches cut before a suite existed do not
carry its workflow file. If the run history cannot be read, every pair runs.
Prints the JSON list of pairs to dispatch.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


GITHUB_API_VERSION = "2022-11-28"
DEFAULT_REPOSITORY = "ai-dynamo/snapshot"
RELEASE_PREFIX = "release/"
# The three suites main runs on a schedule, plus the CPU suite: it is reached
# on main by pull_request, and release branches receive no pull requests, so a
# sweep is the only thing that can run it there.
DEFAULT_WORKFLOWS = (
    "e2e.yaml",
    "e2e-upgrade.yaml",
    "e2e-frameworks.yaml",
    "e2e-cpu.yaml",
)


def decide(
    *,
    force: bool,
    workflow_present: bool,
    head_sha: str,
    last_run_sha: str | None,
) -> tuple[bool, str]:
    if not workflow_present:
        return False, "suite not present on this branch"
    if force:
        return True, "forced"
    if last_run_sha is None:
        return True, "suite never ran on this branch"
    if last_run_sha == head_sha:
        return False, f"no new commits since {last_run_sha[:8]}"
    return True, f"new commits since {last_run_sha[:8]}"


def github_json(url: str, headers: dict[str, str]) -> dict | list:
    request = urllib.request.Request(url, headers=headers)
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def release_branches(*, repository: str, headers: dict[str, str]) -> list[dict[str, str]]:
    """Every release/* branch with its head SHA, newest version first."""
    base = f"https://api.github.com/repos/{repository}/branches"
    branches: list[dict[str, str]] = []
    page = 1
    while True:
        query = urllib.parse.urlencode({"per_page": 100, "page": page})
        batch = github_json(f"{base}?{query}", headers)
        if not isinstance(batch, list) or not batch:
            break
        for branch in batch:
            name = branch.get("name", "")
            if name.startswith(RELEASE_PREFIX):
                branches.append({"branch": name, "head_sha": branch["commit"]["sha"]})
        if len(batch) < 100:
            break
        page += 1
    return sorted(branches, key=lambda item: version_key(item["branch"]), reverse=True)


def version_key(branch: str) -> tuple[int, ...]:
    """Sort release/1.10 after release/1.9 rather than before it."""
    parts = branch[len(RELEASE_PREFIX) :].split(".")
    key = []
    for part in parts:
        try:
            key.append(int(part))
        except ValueError:
            return (0,)
    return tuple(key)


def last_run_sha(*, repository: str, workflow: str, branch: str, headers: dict[str, str]) -> str | None:
    """Head SHA of the newest run of this suite on this branch, if any."""
    base = f"https://api.github.com/repos/{repository}/actions"
    query = urllib.parse.urlencode({"branch": branch, "per_page": 1})
    try:
        payload = github_json(f"{base}/workflows/{workflow}/runs?{query}", headers)
    except urllib.error.HTTPError as exc:
        if exc.code != 404:
            raise
        return None
    runs = payload.get("workflow_runs", []) if isinstance(payload, dict) else []
    return runs[0]["head_sha"] if runs else None


def workflow_present(*, repository: str, workflow: str, branch: str, headers: dict[str, str]) -> bool:
    path = f".github/workflows/{workflow}"
    query = urllib.parse.urlencode({"ref": branch})
    url = f"https://api.github.com/repos/{repository}/contents/{path}?{query}"
    try:
        github_json(url, headers)
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return False
        raise
    return True


def write_outputs(selected: list[dict[str, str]], decisions: list[tuple[dict[str, str], bool, str]]) -> None:
    encoded = json.dumps(selected, separators=(",", ":"))
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write(f"pairs={encoded}\n")
            handle.write(f"any={'true' if selected else 'false'}\n")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        lines = ["### Release branch e2e sweep", "", "| Branch | Suite | Decision | Reason |", "| --- | --- | --- | --- |"]
        for pair, run, reason in decisions:
            lines.append(f"| `{pair['branch']}` | `{pair['workflow']}` | {'dispatch' if run else 'skip'} | {reason} |")
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n\n")
    print(encoded)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--workflows", default=",".join(DEFAULT_WORKFLOWS), help="comma-separated suites to sweep")
    parser.add_argument("--branches", default="", help="comma-separated release branches instead of every one")
    parser.add_argument("--force", action="store_true", help="dispatch every pair without checking history")
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY") or DEFAULT_REPOSITORY)
    args = parser.parse_args()

    workflows = [item.strip() for item in args.workflows.split(",") if item.strip()]
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": GITHUB_API_VERSION}
    token = os.environ.get("GH_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"

    history_error: str | None = None
    try:
        branches = release_branches(repository=args.repository, headers=headers)
        if args.branches:
            wanted = {item.strip() for item in args.branches.split(",") if item.strip()}
            branches = [branch for branch in branches if branch["branch"] in wanted]
    except (OSError, ValueError) as exc:
        print(f"error: cannot list release branches ({exc})", file=sys.stderr)
        return 1

    decisions: list[tuple[dict[str, str], bool, str]] = []
    for branch in branches:
        for workflow in workflows:
            pair = {"branch": branch["branch"], "workflow": workflow}
            if history_error:
                decisions.append((pair, True, history_error))
                continue
            try:
                present = workflow_present(
                    repository=args.repository, workflow=workflow, branch=pair["branch"], headers=headers
                )
                previous = (
                    last_run_sha(
                        repository=args.repository, workflow=workflow, branch=pair["branch"], headers=headers
                    )
                    if present
                    else None
                )
            except (OSError, ValueError) as exc:
                history_error = f"run history unavailable ({exc})"
                print(f"warning: {history_error}; dispatching every pair", file=sys.stderr)
                decisions.append((pair, True, history_error))
                continue
            run, reason = decide(
                force=args.force,
                workflow_present=present,
                head_sha=branch["head_sha"],
                last_run_sha=previous,
            )
            decisions.append((pair, run, reason))
            print(f"{'dispatch' if run else 'skip'} {pair['branch']} {workflow}: {reason}", file=sys.stderr)

    write_outputs([pair for pair, run, _ in decisions if run], decisions)
    return 0


if __name__ == "__main__":
    sys.exit(main())
