# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import importlib.util
import io
import json
import sys
from pathlib import Path
from types import ModuleType

import pytest


SCRIPT = Path(__file__).resolve().parents[2] / "hack" / "resolve-snapshot-image-tag.py"


@pytest.fixture
def resolver(monkeypatch: pytest.MonkeyPatch) -> ModuleType:
    spec = importlib.util.spec_from_file_location("resolve_snapshot_image_tag", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name in ("GITHUB_OUTPUT", "GITHUB_ENV", "SNAPSHOT_E2E_SNAPSHOT_TAG", "GH_TOKEN"):
        monkeypatch.delenv(name, raising=False)
    return module


def publish(monkeypatch: pytest.MonkeyPatch, resolver: ModuleType, published: dict[str, set[str]]) -> None:
    monkeypatch.setattr(
        resolver,
        "package_has_tag",
        lambda package, tag, headers: tag in published.get(package, set()),
    )


def published_everywhere(*tags: str) -> dict[str, set[str]]:
    images = set(tags)
    return {
        "snapshot/operator": images,
        "snapshot/agent": images,
        "snapshot/pagebroker": images,
        "snapshot/snapshot": {tag.removeprefix("v") for tag in tags},
    }


def run_main(monkeypatch: pytest.MonkeyPatch, resolver: ModuleType, *argv: str) -> int:
    monkeypatch.setattr(sys, "argv", ["resolve-snapshot-image-tag.py", *argv])
    return resolver.main()


def test_release_tags_skips_drafts_and_prereleases(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    releases = [
        {"tag_name": "v0.3.0", "draft": True, "prerelease": False},
        {"tag_name": "v0.2.0", "draft": False, "prerelease": True},
        {"tag_name": "v0.1.0", "draft": False, "prerelease": False},
    ]
    urls = []

    def urlopen(request, timeout):
        urls.append(request.full_url)
        return io.BytesIO(json.dumps(releases).encode())

    monkeypatch.setattr(resolver.urllib.request, "urlopen", urlopen)

    assert resolver.release_tags({}) == ["v0.1.0"]
    assert urls == ["https://api.github.com/repos/ai-dynamo/snapshot/releases?per_page=100&page=1"]


def test_latest_minor_releases_takes_newest_patch_of_each_minor(resolver: ModuleType) -> None:
    tags = ["v0.3.0", "v0.1.0", "v0.3.2", "v0.2.1", "v0.3.1", "v0.2.0", "v0.10.0"]

    assert resolver.latest_minor_releases(tags, 3) == ["v0.10.0", "v0.3.2", "v0.2.1"]


def test_latest_minor_releases_ignores_non_release_tags(resolver: ModuleType) -> None:
    tags = ["v0.2.0-rc.1", "v0.0.0-g1a2b3c4d", "operator/v0.1.0", "v0.1.0"]

    assert resolver.latest_minor_releases(tags, 3) == ["v0.1.0"]


@pytest.mark.parametrize(
    ("ref", "expected"),
    [
        ("v0.1.0", "v0.1.0"),
        ("v0.1.0-rc2", "v0.1.0-rc2"),
        ("v0.0.0-g1a2b3c4d", "v0.0.0-g1a2b3c4d"),
        ("1a2b3c4d", "v0.0.0-g1a2b3c4d"),
        ("1A2B3C4D5E6F", "v0.0.0-g1a2b3c4d"),
    ],
)
def test_from_ref_tag_accepts_version_tags_and_commit_hashes(resolver: ModuleType, ref: str, expected: str) -> None:
    assert resolver.from_ref_tag(ref) == expected


def test_from_ref_tag_expands_seven_character_hashes(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(resolver, "expand_commit", lambda prefix: prefix + "d" * 33)

    assert resolver.from_ref_tag("1a2b3c4") == "v0.0.0-g1a2b3c4d"


@pytest.mark.parametrize("ref", ["main", "0.1.0", "v0.1", "not-a-hash"])
def test_from_ref_tag_rejects_other_refs(resolver: ModuleType, ref: str) -> None:
    with pytest.raises(resolver.ResolveError):
        resolver.from_ref_tag(ref)


def test_resolve_from_drops_version_under_test_and_duplicates(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    publish(monkeypatch, resolver, published_everywhere("v0.1.0", "v0.0.0-g1a2b3c4d"))

    resolved = resolver.resolve_from(["v0.1.0", "v0.0.0-g1a2b3c4d", "v0.1.0"], "v0.0.0-g1a2b3c4d", {})

    assert resolved == [{"tag": "v0.1.0", "chart_version": "0.1.0"}]


def test_resolve_from_fails_when_only_version_under_test_remains(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    publish(monkeypatch, resolver, published_everywhere("v0.0.0-g1a2b3c4d"))

    with pytest.raises(resolver.ResolveError, match="no version left"):
        resolver.resolve_from(["v0.0.0-g1a2b3c4d"], "v0.0.0-g1a2b3c4d", {})


def test_resolve_from_names_every_missing_package(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    published = published_everywhere("v0.0.0-g1a2b3c4d")
    published["snapshot/agent"] = set()
    published["snapshot/snapshot"] = set()
    publish(monkeypatch, resolver, published)

    with pytest.raises(resolver.ResolveError) as exc:
        resolver.resolve_from(["v0.0.0-g1a2b3c4d"], "v0.0.0-g99999999", {})

    message = str(exc.value)
    assert "snapshot/agent, snapshot/snapshot" in message
    assert "main and release-branch commits" in message


def test_empty_from_uses_latest_release(
    resolver: ModuleType,
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    output = tmp_path / "github_output"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setattr(resolver, "release_tags", lambda headers: ["v0.1.0", "v0.2.0", "v0.2.1"])
    publish(monkeypatch, resolver, published_everywhere("v0.2.1"))

    assert run_main(monkeypatch, resolver, "--from", "", "--to", "v0.0.0-g1a2b3c4d") == 0

    expected = [{"tag": "v0.2.1", "chart_version": "0.2.1"}]
    assert json.loads(capsys.readouterr().out) == expected
    assert output.read_text(encoding="utf-8") == f"upgrade_from={json.dumps(expected, separators=(',', ':'))}\n"


def test_pagebroker_is_not_required_to_upgrade_from(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    published = published_everywhere("v0.1.0")
    published["snapshot/pagebroker"] = set()
    publish(monkeypatch, resolver, published)

    assert resolver.resolve_from(["v0.1.0"], "v0.0.0-g1a2b3c4d", {}) == [{"tag": "v0.1.0", "chart_version": "0.1.0"}]


def test_from_minor_versions_lists_each_minor(
    resolver: ModuleType,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.0.0-g1a2b3c4d")
    monkeypatch.setattr(resolver, "release_tags", lambda headers: ["v0.1.0", "v0.2.0", "v0.2.1", "v0.3.0"])
    publish(monkeypatch, resolver, published_everywhere("v0.3.0", "v0.2.1"))

    assert run_main(monkeypatch, resolver, "--from-minor-versions", "2") == 0

    assert [entry["tag"] for entry in json.loads(capsys.readouterr().out)] == ["v0.3.0", "v0.2.1"]


def test_from_fails_without_any_release(
    resolver: ModuleType,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    monkeypatch.setattr(resolver, "release_tags", lambda headers: ["v0.1.0-rc.1"])

    assert run_main(monkeypatch, resolver, "--from", "", "--to", "v0.0.0-g1a2b3c4d") == 1

    assert "no published release" in capsys.readouterr().err


def test_from_minor_versions_rejects_zero(
    resolver: ModuleType,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    assert run_main(monkeypatch, resolver, "--from-minor-versions", "0", "--to", "v0.0.0-g1a2b3c4d") == 1

    assert "at least 1" in capsys.readouterr().err


def test_from_modes_exclude_fallback_main(resolver: ModuleType, monkeypatch: pytest.MonkeyPatch) -> None:
    with pytest.raises(SystemExit):
        run_main(monkeypatch, resolver, "--fallback-main", "--from", "v0.1.0")
