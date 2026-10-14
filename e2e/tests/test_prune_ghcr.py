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
           child_digests=None, dry_run=False):
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
        "ai-dynamo", "snapshot/cache", "token", keep_days, dry_run
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

    def record(_org, package, _token, keep_days, _dry_run):
        calls[package] = keep_days
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
