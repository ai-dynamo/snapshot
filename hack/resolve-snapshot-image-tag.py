# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Resolve the Snapshot image tag for the exact commit under test.

The e2e run must test the commit it checked out, so the tag is derived from
HEAD (v0.0.0-g<sha8>) and merely verified to be published for both the
operator and agent packages. If push-artifacts has not published HEAD yet,
the run fails instead of silently testing something else.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import urllib.parse
import urllib.request


GITHUB_API_VERSION = "2022-11-28"
MAX_PAGES = 5
ORG = "ai-dynamo"


def head_sha() -> str:
    sha = os.environ.get("GITHUB_SHA")
    if sha:
        return sha
    result = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    )
    return result.stdout.strip()


def dev_tag(sha: str) -> str:
    return f"v0.0.0-g{sha[:8]}"


def package_has_tag(package: str, tag: str, headers: dict[str, str]) -> bool:
    encoded = urllib.parse.quote(package, safe="")

    for page in range(1, MAX_PAGES + 1):
        url = (
            f"https://api.github.com/orgs/{ORG}/packages/container/"
            f"{encoded}/versions?per_page=100&page={page}"
        )
        request = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(request, timeout=30) as response:
            versions = json.load(response)

        if not versions:
            break

        for version in versions:
            version_tags = version.get("metadata", {}).get("container", {}).get("tags", [])
            if tag in version_tags:
                return True

    return False


def write_github_env(tag: str) -> None:
    github_env = os.environ.get("GITHUB_ENV")
    if not github_env:
        return

    with open(github_env, "a", encoding="utf-8") as env:
        env.write(f"SNAPSHOT_E2E_SNAPSHOT_TAG={tag}\n")


def main() -> int:
    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": GITHUB_API_VERSION,
    }
    token = os.environ.get("GH_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"

    sha = head_sha()
    tag = dev_tag(sha)

    missing = [
        package
        for package in ("snapshot/operator", "snapshot/agent")
        if not package_has_tag(package, tag, headers)
    ]
    if missing:
        print(
            f"Tag {tag} for commit {sha} is not published for: "
            f"{', '.join(missing)}. Wait for push-artifacts to publish this "
            "commit, or dispatch the e2e workflow with an explicit "
            "snapshot_tag.",
            file=sys.stderr,
        )
        return 1

    write_github_env(tag)
    print(f"Resolved Snapshot image tag: {tag} (commit {sha})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
