# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import importlib.util
import io
import json
from pathlib import Path
import urllib.parse

import pytest


def load_resolver() -> object:
    script = Path(__file__).resolve().parents[2] / "hack" / "resolve-snapshot-image-tag.py"
    spec = importlib.util.spec_from_file_location("resolve_snapshot_image_tag", script)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FakeResponse(io.BytesIO):
    def __init__(self, payload: object) -> None:
        super().__init__(json.dumps(payload).encode("utf-8"))

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


def package_version(tag: str, created_at: str) -> dict[str, object]:
    return {
        "created_at": created_at,
        "metadata": {"container": {"tags": [tag]}},
    }


def test_main_selects_newest_shared_tag_from_reversed_api_order(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    resolver = load_resolver()
    older = "v0.0.0-golder"
    newer = "v0.0.0-gnewer"
    operator_versions = [
        package_version(older, "2026-01-01T00:00:00Z"),
        package_version(newer, "2026-01-03T00:00:00Z"),
        package_version("v0.0.0-goperator-only", "2026-01-04T00:00:00Z"),
    ]
    agent_versions = [
        package_version(older, "2026-01-01T00:01:00Z"),
        package_version(newer, "2026-01-03T00:01:00Z"),
        package_version("v0.0.0-gagent-only", "2026-01-04T00:01:00Z"),
    ]

    def fake_urlopen(request: object, timeout: int) -> FakeResponse:
        assert timeout == 30
        url = getattr(request, "full_url")
        parsed = urllib.parse.urlparse(url)
        page = urllib.parse.parse_qs(parsed.query)["page"][0]
        if page != "1":
            return FakeResponse([])
        if "snapshot%2Foperator" in parsed.path:
            return FakeResponse(operator_versions)
        if "snapshot%2Fagent" in parsed.path:
            return FakeResponse(agent_versions)
        raise AssertionError(f"unexpected url: {url}")

    env_file = tmp_path / "github-env"
    monkeypatch.setenv("GITHUB_ENV", str(env_file))
    monkeypatch.setattr(resolver.urllib.request, "urlopen", fake_urlopen)

    assert resolver.main() == 0

    assert env_file.read_text(encoding="utf-8") == (
        f"SNAPSHOT_E2E_SNAPSHOT_TAG={newer}\n"
    )
    assert f"Resolved Snapshot image tag: {newer}\n" == capsys.readouterr().out
