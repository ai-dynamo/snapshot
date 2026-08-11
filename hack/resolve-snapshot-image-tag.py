# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Resolve the newest shared published Snapshot image tag for e2e runs."""

from __future__ import annotations

from datetime import datetime
import json
import os
import sys
import urllib.parse
import urllib.request


GITHUB_API_VERSION = "2022-11-28"
MAX_PAGES = 5
ORG = "ai-dynamo"
TAG_PREFIX = "v0.0.0-g"


def parse_created_at(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def package_tags(package: str, headers: dict[str, str]) -> dict[str, datetime]:
    tags: dict[str, datetime] = {}
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
            created_at = parse_created_at(version["created_at"])
            version_tags = version.get("metadata", {}).get("container", {}).get("tags", [])
            for tag in version_tags:
                if not tag.startswith(TAG_PREFIX):
                    continue
                if tag not in tags or created_at > tags[tag]:
                    tags[tag] = created_at

    return tags


def newest_shared_tag(
    operator_tags: dict[str, datetime],
    agent_tags: dict[str, datetime],
) -> str | None:
    shared_tags = operator_tags.keys() & agent_tags.keys()
    if not shared_tags:
        return None

    return max(
        shared_tags,
        key=lambda tag: (min(operator_tags[tag], agent_tags[tag]), tag),
    )


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

    operator_tags = package_tags("snapshot/operator", headers)
    agent_tags = package_tags("snapshot/agent", headers)

    tag = newest_shared_tag(operator_tags, agent_tags)
    if tag:
        write_github_env(tag)
        print(f"Resolved Snapshot image tag: {tag}")
        return 0

    print("No shared published Snapshot operator/agent tag found", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
