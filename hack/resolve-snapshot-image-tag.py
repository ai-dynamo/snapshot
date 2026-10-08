# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Resolve the Snapshot image tag for the exact commit under test.

The e2e run must test the commit it checked out, so the tag is derived from
HEAD (v0.0.0-g<sha8>) and merely verified to be published for the operator,
agent, and pagebroker packages. If push-artifacts has not published HEAD yet,
the run fails instead of silently testing something else.

Pull requests are the exception: push-artifacts publishes only main and
release branches, so a PR head usually has no images. With --fallback-main the
resolver first tries the head's own tags (plain, then the branch-slugged form a
manual push-artifacts dispatch produces), and otherwise falls back to the
newest plain v0.0.0-g<sha8> tag published for all packages. Plain tags are
only ever produced from main/release, so the fallback cannot pick up another
feature branch's build; it is announced loudly because the Snapshot under test
is then main's, not the PR's.

Upgrade tests also need the version a snapshot is taken on before upgrading.
--from takes release tags or commit hashes, and --from-minor-versions N takes
the newest patch release of each of the last N minor versions. Both print a
JSON list of {tag, chart_version}, verified to be published for the operator
and agent images and the chart, and drop the version under test itself. The
pagebroker image is not required: it is only deployed when enabled, and
releases before it existed never published one.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import urllib.parse
import urllib.request


GITHUB_API_VERSION = "2022-11-28"
PAGE_SIZE = 100
ORG = "ai-dynamo"
REPOSITORY = f"{ORG}/snapshot"
PACKAGES = ("snapshot/operator", "snapshot/agent", "snapshot/pagebroker")
CHART_PACKAGE = "snapshot/snapshot"
UPGRADE_FROM_PACKAGES = ("snapshot/operator", "snapshot/agent")
PLAIN_DEV_TAG = re.compile(r"^v0\.0\.0-g[0-9a-f]{8}$")
VERSION_TAG = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?$")
RELEASE_TAG = re.compile(r"^v([0-9]+)\.([0-9]+)\.([0-9]+)$")
COMMIT_HASH = re.compile(r"^[0-9a-f]{7,40}$")


class ResolveError(RuntimeError):
    pass


def head_sha() -> str:
    # On a push (including copy-pr-bot mirror pushes) GITHUB_SHA is the branch
    # head, which is the commit a branch build would have tagged.
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


def head_ref() -> str:
    return os.environ.get("GITHUB_HEAD_REF") or os.environ.get("GITHUB_REF_NAME") or ""


def dev_tag(sha: str) -> str:
    return f"v0.0.0-g{sha[:8]}"


def branch_dev_tag(ref: str, sha: str) -> str:
    # Mirrors push-artifacts.yaml: lowercase alphanumerics and single hyphens,
    # capped at 30 characters, no leading/trailing hyphen.
    slug = re.sub(r"[^a-z0-9]+", "-", ref.lower()).lstrip("-")[:30].rstrip("-") or "branch"
    return f"v0.0.0-{slug}-g{sha[:8]}"


_VERSIONS_CACHE: dict[str, list[dict]] = {}


def package_versions(package: str, headers: dict[str, str]) -> list[dict]:
    # One listing per package per run: every tag check below reads from it, so
    # the fallback path costs the same handful of requests as the strict one.
    if package in _VERSIONS_CACHE:
        return _VERSIONS_CACHE[package]
    encoded = urllib.parse.quote(package, safe="")
    versions: list[dict] = []
    # Page until exhausted: dev tags accumulate one per main commit and are not
    # pruned, so a fixed page budget would eventually hide a published tag.
    page = 0
    while True:
        page += 1
        url = (
            f"https://api.github.com/orgs/{ORG}/packages/container/"
            f"{encoded}/versions?per_page={PAGE_SIZE}&page={page}"
        )
        request = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(request, timeout=30) as response:
            page_versions = json.load(response)
        if not page_versions:
            break
        versions.extend(page_versions)
        if len(page_versions) < PAGE_SIZE:
            break
    _VERSIONS_CACHE[package] = versions
    return versions


def version_tags(version: dict) -> list[str]:
    return version.get("metadata", {}).get("container", {}).get("tags", [])


def package_has_tag(package: str, tag: str, headers: dict[str, str]) -> bool:
    return any(tag in version_tags(v) for v in package_versions(package, headers))


def published_for_all(tag: str, headers: dict[str, str]) -> bool:
    return all(package_has_tag(package, tag, headers) for package in PACKAGES)


def newest_plain_tag(headers: dict[str, str]) -> str | None:
    """Newest plain dev tag published for every package.

    GHCR lists versions newest first, so the first operator tag also present
    on every other package is the newest common one.
    """
    per_package = [
        [tag for v in package_versions(p, headers) for tag in version_tags(v) if PLAIN_DEV_TAG.match(tag)]
        for p in PACKAGES
    ]
    common = set.intersection(*(set(tags) for tags in per_package[1:])) if len(per_package) > 1 else set(per_package[0])
    for tag in per_package[0]:
        if tag in common:
            return tag
    return None


def write_github_env(tag: str) -> None:
    github_env = os.environ.get("GITHUB_ENV")
    if not github_env:
        return

    with open(github_env, "a", encoding="utf-8") as env:
        env.write(f"SNAPSHOT_E2E_SNAPSHOT_TAG={tag}\n")


def release_tags(headers: dict[str, str]) -> list[str]:
    tags: list[str] = []
    page = 0
    while True:
        page += 1
        url = f"https://api.github.com/repos/{REPOSITORY}/releases?per_page={PAGE_SIZE}&page={page}"
        request = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(request, timeout=30) as response:
            releases = json.load(response)
        if not releases:
            break
        tags.extend(r["tag_name"] for r in releases if not r.get("draft") and not r.get("prerelease"))
        if len(releases) < PAGE_SIZE:
            break
    return tags


def latest_minor_releases(tags: list[str], count: int) -> list[str]:
    newest: dict[tuple[int, int], tuple[int, int, int]] = {}
    for tag in tags:
        match = RELEASE_TAG.match(tag)
        if not match:
            continue
        version = (int(match[1]), int(match[2]), int(match[3]))
        if version > newest.get(version[:2], (-1, -1, -1)):
            newest[version[:2]] = version
    return [f"v{major}.{minor}.{patch}" for major, minor, patch in sorted(newest.values(), reverse=True)[:count]]


def expand_commit(prefix: str) -> str:
    result = subprocess.run(
        ["git", "rev-parse", "--verify", "--quiet", f"{prefix}^{{commit}}"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise ResolveError(
            f"commit {prefix} is not in the local history; pass at least 8 hex "
            "characters or fetch more history"
        )
    return result.stdout.strip()


def from_ref_tag(ref: str) -> str:
    if VERSION_TAG.match(ref):
        return ref
    sha = ref.lower()
    if not COMMIT_HASH.match(sha):
        raise ResolveError(f"{ref!r} is neither a version tag (vX.Y.Z) nor a commit hash")
    if len(sha) < 8:
        sha = expand_commit(sha)
    return dev_tag(sha)


def chart_version(tag: str) -> str:
    return tag.removeprefix("v")


def resolve_from(tags: list[str], to_tag: str, headers: dict[str, str]) -> list[dict[str, str]]:
    resolved = []
    for tag in dict.fromkeys(tags):
        if tag == to_tag:
            print(f"Skipping {tag}: it is the version under test", file=sys.stderr)
            continue
        missing = [package for package in UPGRADE_FROM_PACKAGES if not package_has_tag(package, tag, headers)]
        if not package_has_tag(CHART_PACKAGE, chart_version(tag), headers):
            missing.append(CHART_PACKAGE)
        if missing:
            hint = (
                " push-artifacts publishes only main and release-branch commits."
                if PLAIN_DEV_TAG.match(tag)
                else ""
            )
            raise ResolveError(f"{tag} is not published for: {', '.join(missing)}.{hint}")
        resolved.append({"tag": tag, "chart_version": chart_version(tag)})
    if not resolved:
        raise ResolveError(f"no version left to upgrade from after dropping the version under test ({to_tag})")
    return resolved


def upgrade_from_tags(args: argparse.Namespace, headers: dict[str, str]) -> list[str]:
    refs = [ref.strip() for ref in (args.upgrade_from or "").split(",") if ref.strip()]
    if refs:
        return [from_ref_tag(ref) for ref in refs]
    count = 1 if args.from_minor_versions is None else args.from_minor_versions
    if count < 1:
        raise ResolveError("--from-minor-versions must be at least 1")
    tags = latest_minor_releases(release_tags(headers), count)
    if not tags:
        raise ResolveError(f"{REPOSITORY} has no published release to upgrade from")
    return tags


def write_github_output(name: str, value: str) -> None:
    github_output = os.environ.get("GITHUB_OUTPUT")
    if not github_output:
        return

    with open(github_output, "a", encoding="utf-8") as output:
        output.write(f"{name}={value}\n")


def resolve_upgrade_from(args: argparse.Namespace, headers: dict[str, str]) -> int:
    to_tag = args.to or os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG") or dev_tag(head_sha())
    try:
        resolved = resolve_from(upgrade_from_tags(args, headers), to_tag, headers)
    except ResolveError as exc:
        print(str(exc), file=sys.stderr)
        return 1

    encoded = json.dumps(resolved, separators=(",", ":"))
    write_github_output("upgrade_from", encoded)
    for entry in resolved:
        print(f"Resolved upgrade-from version: {entry['tag']} (upgrading to {to_tag})", file=sys.stderr)
    print(encoded)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument(
        "--fallback-main",
        action="store_true",
        help=(
            "if the head commit has no published tag (plain or branch-slugged), "
            "use the newest plain v0.0.0-g<sha8> tag instead of failing"
        ),
    )
    mode.add_argument(
        "--from",
        dest="upgrade_from",
        metavar="REFS",
        help=(
            "comma-separated version tags or commit hashes to upgrade from; "
            "empty means the latest release"
        ),
    )
    mode.add_argument(
        "--from-minor-versions",
        type=int,
        metavar="N",
        help="upgrade from the newest patch release of each of the last N minor versions",
    )
    parser.add_argument(
        "--to",
        help=(
            "version under test, dropped from the upgrade-from list; defaults to "
            "SNAPSHOT_E2E_SNAPSHOT_TAG, then the head commit's dev tag"
        ),
    )
    args = parser.parse_args()

    headers = {
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": GITHUB_API_VERSION,
    }
    token = os.environ.get("GH_TOKEN")
    if token:
        headers["Authorization"] = f"Bearer {token}"

    if args.upgrade_from is not None or args.from_minor_versions is not None:
        return resolve_upgrade_from(args, headers)
    if args.to:
        parser.error("--to only applies with --from or --from-minor-versions")

    sha = head_sha()
    tag = dev_tag(sha)

    if published_for_all(tag, headers):
        write_github_env(tag)
        print(f"Resolved Snapshot image tag: {tag} (commit {sha})")
        return 0

    if not args.fallback_main:
        missing = [package for package in PACKAGES if not package_has_tag(package, tag, headers)]
        print(
            f"Tag {tag} for commit {sha} is not published for: "
            f"{', '.join(missing)}. Wait for push-artifacts to publish this "
            "commit, or dispatch the e2e workflow with an explicit "
            "snapshot_tag.",
            file=sys.stderr,
        )
        return 1

    ref = head_ref()
    if ref:
        branch_tag = branch_dev_tag(ref, sha)
        if published_for_all(branch_tag, headers):
            write_github_env(branch_tag)
            print(f"Resolved Snapshot image tag: {branch_tag} (branch build of commit {sha})")
            return 0

    fallback = newest_plain_tag(headers)
    if fallback is None:
        print(
            f"Commit {sha} has no published Snapshot images and no plain dev tag "
            "exists to fall back to.",
            file=sys.stderr,
        )
        return 1

    write_github_env(fallback)
    # ::warning:: surfaces in the job summary and annotations: anyone reading
    # a green run must be able to see it did not test this commit's Snapshot.
    print(
        f"::warning title=Snapshot images not built for this commit::"
        f"Commit {sha} has no published operator/agent/pagebroker images; testing against "
        f"{fallback} (newest main build). Dispatch push-artifacts on this branch "
        "and re-run to test the commit's own Snapshot."
    )
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write(
                f"> **Snapshot images not built for this commit.** Tested against `{fallback}` "
                f"(newest main build); commit `{sha[:8]}` has no published operator/agent/pagebroker images.\n"
            )
    print(f"Resolved Snapshot image tag: {fallback} (fallback; commit {sha} unpublished)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
