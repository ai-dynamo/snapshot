#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Prune stale untagged GHCR versions for the images and chart this repo publishes.

Every push to main publishes a v0.0.0-g<sha8> image, so untagged manifests
accumulate indefinitely. Deleting them is not as simple as "delete everything
untagged": with buildx attestations enabled, every image we publish is an OCI
index whose per-platform and attestation manifests appear in the GHCR API as
separate, untagged versions. Deleting those breaks the tagged image that
references them — including published releases. cosign signatures are stored
under a sha256-<digest> tag and so are tagged, but the manifests inside them
are not.

So a digest is deletable only when it is BOTH untagged AND unreachable from any
tagged version, after walking every tagged index one level down. Anything we
cannot resolve is treated as reachable: the cost of keeping a stale blob is
storage, the cost of deleting a referenced one is a broken release.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone

API = "https://api.github.com"
REGISTRY = "https://ghcr.io"

INDEX_TYPES = {
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
}


def _get(url: str, token: str, accept: str) -> tuple[dict | list | None, dict]:
    req = urllib.request.Request(url, headers={
        "Authorization": f"Bearer {token}",
        "Accept": accept,
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return json.load(resp), dict(resp.headers)
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return None, {}
        if exc.code in (401, 403):
            raise PermissionError(
                f"{exc.code} for {url} — the token needs read:packages to list "
                f"versions and packages: write to delete them"
            ) from exc
        raise


def list_versions(org: str, package: str, token: str) -> list[dict]:
    """Every version of one container package, following pagination."""
    out: list[dict] = []
    page = 1
    while True:
        url = (f"{API}/orgs/{org}/packages/container/"
               f"{package.replace('/', '%2F')}/versions?per_page=100&page={page}")
        body, _ = _get(url, token, "application/vnd.github+json")
        if not body:
            break
        out.extend(body)
        if len(body) < 100:
            break
        page += 1
    return out


def registry_token(repo_path: str, token: str) -> str:
    """Exchange the Actions token for a registry pull token."""
    url = f"{REGISTRY}/token?scope=repository:{repo_path}:pull&service=ghcr.io"
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)["token"]


def child_digests(repo_path: str, digest: str, reg_token: str) -> set[str]:
    """Digests referenced by an index. Empty for a plain manifest.

    On any failure the caller treats the parent as opaque and keeps everything,
    so returning an empty set here must never be read as "nothing referenced".
    """
    url = f"{REGISTRY}/v2/{repo_path}/manifests/{digest}"
    accept = ", ".join(sorted(INDEX_TYPES) + [
        "application/vnd.oci.image.manifest.v1+json",
        "application/vnd.docker.distribution.manifest.v2+json",
    ])
    body, headers = _get(url, reg_token, accept)
    if not body:
        raise RuntimeError(f"could not read manifest {digest}")
    if headers.get("Content-Type", "").split(";")[0] in INDEX_TYPES:
        return {m["digest"] for m in body.get("manifests", []) if m.get("digest")}
    return set()


def prune_package(org: str, package: str, token: str, keep_days: int,
                  dry_run: bool) -> tuple[int, int]:
    repo_path = f"{org}/{package}".lower()
    versions = list_versions(org, package, token)
    if not versions:
        print(f"  {package}: no versions visible")
        return (0, 0)

    reg_token = registry_token(repo_path, token)
    cutoff = datetime.now(timezone.utc) - timedelta(days=keep_days)

    reachable: set[str] = set()
    untagged: list[dict] = []
    for v in versions:
        tags = (v.get("metadata") or {}).get("container", {}).get("tags") or []
        digest = v.get("name", "")
        if tags:
            reachable.add(digest)
            try:
                reachable |= child_digests(repo_path, digest, reg_token)
            except Exception as exc:  # noqa: BLE001 - keep, never delete, on doubt
                print(f"  ! {package}@{digest[:19]}: {exc}; treating as reachable")
        else:
            untagged.append(v)

    deleted = 0
    for v in untagged:
        digest = v.get("name", "")
        if digest in reachable:
            continue
        created = datetime.fromisoformat(v["created_at"].replace("Z", "+00:00"))
        if created > cutoff:
            continue
        print(f"  {'would delete' if dry_run else 'deleting'} "
              f"{package}@{digest[:19]} (created {created.date()})")
        if not dry_run:
            req = urllib.request.Request(
                f"{API}/orgs/{org}/packages/container/"
                f"{package.replace('/', '%2F')}/versions/{v['id']}",
                method="DELETE",
                headers={"Authorization": f"Bearer {token}",
                         "Accept": "application/vnd.github+json"},
            )
            urllib.request.urlopen(req, timeout=30).close()
        deleted += 1

    print(f"  {package}: {len(versions)} versions, {len(untagged)} untagged, "
          f"{len(reachable)} reachable, {deleted} "
          f"{'prunable' if dry_run else 'pruned'}")
    return (len(untagged), deleted)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--org", default="ai-dynamo")
    ap.add_argument("--packages", default="snapshot/operator,snapshot/agent,snapshot/snapshot")
    ap.add_argument("--keep-days", type=int, default=90)
    ap.add_argument("--dry-run", default="true")
    args = ap.parse_args()

    token = os.environ.get("GITHUB_TOKEN")
    if not token:
        print("GITHUB_TOKEN is not set", file=sys.stderr)
        return 1

    dry_run = args.dry_run.lower() != "false"
    print(f"org={args.org} keep_days={args.keep_days} dry_run={dry_run}")

    total = 0
    try:
        for package in [p.strip() for p in args.packages.split(",") if p.strip()]:
            _, deleted = prune_package(args.org, package, token, args.keep_days, dry_run)
            total += deleted
    except PermissionError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1
    print(f"{'Prunable' if dry_run else 'Pruned'}: {total}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
