# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free regressions for ``hack/prune-ghcr.py``.

The script deletes published artifacts, so the properties worth pinning are the
ones whose failure is silent in both directions: a package missing from the
sweep accumulates untagged manifests forever, and a digest still referenced by
a tag must never be deleted however old it is.

The package list is the whole configuration, and it has already been wrong
twice -- ``snapshot/pagebroker`` has been published since it was introduced and
was never swept, and the layer cache turns over on every push to main rather
than on the 90-day clock the images use.
"""

from __future__ import annotations

import importlib.util
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "hack" / "prune-ghcr.py"


def _load():
    """Import a module whose filename is not a valid identifier."""
    spec = importlib.util.spec_from_file_location("prune_ghcr", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def prune():
    return _load()


def _version(vid: int, digest: str, *, tags: list[str], age_days: int) -> dict:
    created = datetime.now(timezone.utc) - timedelta(days=age_days)
    return {
        "id": vid,
        "name": digest,
        "created_at": created.strftime("%Y-%m-%dT%H:%M:%SZ"),
        "metadata": {"container": {"tags": tags}},
    }


def _sweep(prune, monkeypatch, versions, *, keep_days, children=None,
           child_digests=None, dry_run=False, pr_keep_days=None,
           pr_dry_run=None, package="snapshot/cache"):
    """Run prune_package with the network stubbed. Returns the digests deleted.

    ``children`` maps a digest to what a tagged index references. Pass
    ``child_digests`` instead to supply the resolver directly, which is how the
    unreadable-manifest path is reached -- patching it outside this helper would
    be overwritten by the patch below.
    """
    deleted: list[str] = []
    by_id = {v["id"]: v["name"] for v in versions}

    class _Response:
        def close(self):
            pass

    def fake_urlopen(req, timeout=None):
        deleted.append(by_id[int(req.full_url.rsplit("/", 1)[-1])])
        return _Response()

    def resolve(_repo, digest, _token):
        return set((children or {}).get(digest, ()))

    monkeypatch.setattr(prune, "list_versions", lambda *_: versions)
    monkeypatch.setattr(prune, "registry_token", lambda *_: "registry-token")
    monkeypatch.setattr(prune, "child_digests", child_digests or resolve)
    monkeypatch.setattr(prune.urllib.request, "urlopen", fake_urlopen)

    untagged, count = prune.prune_package(
        "ai-dynamo", package, "token", keep_days, dry_run,
        pr_keep_days=pr_keep_days, pr_dry_run=pr_dry_run,
    )
    return untagged, count, deleted


def test_superseded_cache_manifest_is_deleted(prune, monkeypatch):
    """The case from review: a tag moved on, leaving a multi-gigabyte orphan."""
    versions = [
        _version(1, "sha256:current", tags=["agent-main"], age_days=1),
        _version(2, "sha256:superseded", tags=[], age_days=30),
    ]
    untagged, count, deleted = _sweep(prune, monkeypatch, versions, keep_days=7)
    assert (untagged, count, deleted) == (1, 1, ["sha256:superseded"])


def test_cache_is_swept_on_a_clock_of_days_not_months(prune, monkeypatch):
    """A month-old orphan survives the image rule and not the cache one.

    Adding the package to the list without this would have reported the blobs
    and kept them for a quarter, which is the accumulation being complained of.
    """
    versions = [
        _version(1, "sha256:current", tags=["agent-main"], age_days=1),
        _version(2, "sha256:superseded", tags=[], age_days=30),
    ]
    assert _sweep(prune, monkeypatch, versions, keep_days=90)[1] == 0
    assert _sweep(prune, monkeypatch, versions, keep_days=7)[1] == 1


def test_a_recent_orphan_is_kept(prune, monkeypatch):
    """The window exists to notice a mistake in before it is irreversible."""
    versions = [
        _version(1, "sha256:current", tags=["agent-main"], age_days=0),
        _version(2, "sha256:yesterday", tags=[], age_days=1),
    ]
    assert _sweep(prune, monkeypatch, versions, keep_days=7)[1] == 0


def test_a_digest_reachable_from_a_tag_is_never_deleted(prune, monkeypatch):
    """Age is irrelevant while a tagged index still references it."""
    versions = [
        _version(1, "sha256:index", tags=["agent-main"], age_days=400),
        _version(2, "sha256:child", tags=[], age_days=400),
    ]
    _, count, deleted = _sweep(
        prune, monkeypatch, versions, keep_days=7,
        children={"sha256:index": ["sha256:child"]},
    )
    assert (count, deleted) == (0, [])


def test_an_unresolvable_tag_blocks_every_deletion(prune, monkeypatch):
    """Fail closed: what could not be read cannot be enumerated."""
    versions = [
        _version(1, "sha256:index", tags=["agent-main"], age_days=400),
        _version(2, "sha256:orphan", tags=[], age_days=400),
    ]

    def boom(*_args):
        raise RuntimeError("502 from ghcr.io")

    untagged, count, deleted = _sweep(
        prune, monkeypatch, versions, keep_days=7, child_digests=boom
    )
    assert (untagged, count, deleted) == (1, 0, [])


def test_dry_run_reports_without_deleting(prune, monkeypatch):
    versions = [
        _version(1, "sha256:current", tags=["agent-main"], age_days=1),
        _version(2, "sha256:superseded", tags=[], age_days=30),
    ]
    _, count, deleted = _sweep(
        prune, monkeypatch, versions, keep_days=7, dry_run=True
    )
    assert (count, deleted) == (1, [])


def _default_sweep(prune, monkeypatch):
    """What the defaults sweep, as {package: keep_days}."""
    calls: dict[str, int] = {}

    def record(_org, package, _token, keep_days, _dry_run, **kwargs):
        calls[package] = keep_days
        calls[f"{package}:pr"] = kwargs.get("pr_keep_days")
        return (0, 0)

    monkeypatch.setattr(prune.sys, "argv", ["prune-ghcr.py"])
    monkeypatch.setenv("GITHUB_TOKEN", "token")
    monkeypatch.setattr(prune, "prune_package", record)

    assert prune.main() == 0
    return calls


@pytest.mark.parametrize("package", [
    "snapshot/operator",
    "snapshot/agent",
    "snapshot/pagebroker",
    "snapshot/snapshot",
    "snapshot/cache",
])
def test_every_published_package_is_swept(prune, monkeypatch, package):
    """push-artifacts.yaml publishes all of these; anything missed grows forever."""
    assert package in _default_sweep(prune, monkeypatch)


def test_the_cache_and_the_images_keep_different_clocks(prune, monkeypatch):
    swept = _default_sweep(prune, monkeypatch)
    assert swept["snapshot/cache"] == 7
    assert swept["snapshot/operator"] == 90


# Pull request images: tagged, so the untagged sweep above never reaches them.

PR_TAG = "v0.0.0-pull-request-42-g1a2b3c4d"


def _pr_sweep(prune, monkeypatch, versions, **kwargs):
    kwargs.setdefault("keep_days", 90)
    kwargs.setdefault("pr_keep_days", 7)
    return _sweep(prune, monkeypatch, versions, package="snapshot/agent", **kwargs)


def test_an_expired_pull_request_image_is_deleted(prune, monkeypatch):
    versions = [
        _version(1, "sha256:main", tags=["v0.0.0-g0badf00d"], age_days=8),
        _version(2, "sha256:pr", tags=[PR_TAG], age_days=8),
    ]
    _, count, deleted = _pr_sweep(prune, monkeypatch, versions)
    assert (count, deleted) == (1, ["sha256:pr"])


def test_a_recent_pull_request_image_is_kept(prune, monkeypatch):
    """A week covers a rerun of the run that built it."""
    versions = [_version(1, "sha256:pr", tags=[PR_TAG], age_days=3)]
    assert _pr_sweep(prune, monkeypatch, versions)[2] == []


@pytest.mark.parametrize("other", [
    "v0.0.0-g0badf00d",
    "v0.1.0",
    "latest",
    "v0.0.0-pull-request-42-g1a2b3c4d-extra",
    "v0.0.0-anants-branch-g1a2b3c4d",
])
def test_an_image_with_any_other_tag_is_kept(prune, monkeypatch, other):
    """Only a version whose every tag is a pull request tag can expire."""
    versions = [_version(1, "sha256:shared", tags=[PR_TAG, other], age_days=400)]
    assert _pr_sweep(prune, monkeypatch, versions)[2] == []


def test_an_expired_image_takes_its_children_with_it(prune, monkeypatch):
    """Left behind, they would wait out the 90-day untagged clock as orphans."""
    versions = [
        _version(1, "sha256:pr", tags=[PR_TAG], age_days=8),
        _version(2, "sha256:pr-amd64", tags=[], age_days=8),
        _version(3, "sha256:pr-attestation", tags=[], age_days=8),
    ]
    _, count, deleted = _pr_sweep(
        prune, monkeypatch, versions,
        children={"sha256:pr": ["sha256:pr-amd64", "sha256:pr-attestation"]},
    )
    assert count == 3
    assert deleted[0] == "sha256:pr", "the index goes before what it references"
    assert set(deleted) == {"sha256:pr", "sha256:pr-amd64", "sha256:pr-attestation"}


def test_a_child_a_kept_image_references_is_kept(prune, monkeypatch):
    versions = [
        _version(1, "sha256:main", tags=["v0.0.0-g0badf00d"], age_days=8),
        _version(2, "sha256:pr", tags=[PR_TAG], age_days=8),
        _version(3, "sha256:shared", tags=[], age_days=8),
    ]
    _, _, deleted = _pr_sweep(
        prune, monkeypatch, versions,
        children={"sha256:main": ["sha256:shared"], "sha256:pr": ["sha256:shared"]},
    )
    assert deleted == ["sha256:pr"]


def test_an_unresolvable_expired_image_blocks_every_deletion(prune, monkeypatch):
    """Its children cannot be told apart from anyone else's, so nothing goes."""
    versions = [
        _version(1, "sha256:pr", tags=[PR_TAG], age_days=8),
        _version(2, "sha256:orphan", tags=[], age_days=400),
    ]

    def boom(*_args):
        raise RuntimeError("502 from ghcr.io")

    _, count, deleted = _pr_sweep(prune, monkeypatch, versions, child_digests=boom)
    assert (count, deleted) == (0, [])


def test_pull_request_expiry_has_its_own_dry_run(prune, monkeypatch):
    """Live expiry must not turn the untagged sweep live, nor the reverse."""
    versions = [
        _version(1, "sha256:pr", tags=[PR_TAG], age_days=8),
        _version(2, "sha256:orphan", tags=[], age_days=400),
    ]
    _, count, deleted = _pr_sweep(
        prune, monkeypatch, versions, dry_run=True, pr_dry_run=False
    )
    assert (count, deleted) == (2, ["sha256:pr"])

    _, count, deleted = _pr_sweep(
        prune, monkeypatch, versions, dry_run=False, pr_dry_run=True
    )
    assert (count, deleted) == (2, ["sha256:orphan"])


def test_pull_request_expiry_is_off_unless_asked_for(prune, monkeypatch):
    versions = [_version(1, "sha256:pr", tags=[PR_TAG], age_days=400)]
    assert _sweep(prune, monkeypatch, versions, keep_days=7)[2] == []


def test_the_images_and_not_the_cache_are_swept_for_pull_requests(prune, monkeypatch):
    swept = _default_sweep(prune, monkeypatch)
    assert swept["snapshot/agent:pr"] == 7
    assert swept["snapshot/cache:pr"] is None
