# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
import yaml

from snapshot_e2e import benchmark_history as history
from snapshot_e2e import benchmark_preview as preview

from test_benchmark_history import START, _result


def test_prepare_preview_overlays_current_run_without_changing_source_history(
    tmp_path: Path,
) -> None:
    history_dir = tmp_path / "history-copy"
    history.store_results(history_dir, [_result(run_id="100")])
    results_dir = tmp_path / "current"
    results_dir.mkdir()
    current = _result(
        case="sglang",
        run_id="200",
        started=START + timedelta(days=1),
    )
    (results_dir / "sglang.json").write_text(json.dumps(current), encoding="utf-8")

    metadata = preview.prepare_preview(
        results_dir=results_dir,
        history_dir=history_dir,
        previews_dir=tmp_path / "previews",
        key="pr-250",
        run_id="200",
        run_attempt=1,
        source_event="push",
        source_branch="pull-request/250",
        source_commit="0123456789abcdef",
        source_run_url="https://github.com/ai-dynamo/snapshot/actions/runs/200",
        pull_request=250,
        generated_at=START + timedelta(days=1),
    )

    preview_root = tmp_path / "previews" / "pr-250"
    manifest = json.loads((preview_root / "index" / "manifest.json").read_text())
    published_metadata = json.loads((preview_root / "preview.json").read_text())
    assert manifest["recordCount"] == 2
    assert metadata["historyRecordCount"] == 1
    assert metadata["previewRecordCount"] == 1
    assert metadata["combinedRecordCount"] == 2
    assert published_metadata["source"]["pullRequest"] == 250
    assert published_metadata["expiresAt"] == "2026-09-09T01:02:03.000Z"


def test_prepare_preview_rejects_result_from_a_different_run(tmp_path: Path) -> None:
    results_dir = tmp_path / "current"
    results_dir.mkdir()
    (results_dir / "vllm.json").write_text(
        json.dumps(_result(run_id="wrong")), encoding="utf-8"
    )

    with pytest.raises(preview.PreviewValidationError, match="does not belong"):
        preview.prepare_preview(
            results_dir=results_dir,
            history_dir=tmp_path / "history",
            previews_dir=tmp_path / "previews",
            key="run-200",
            run_id="200",
            run_attempt=1,
            source_event="workflow_dispatch",
            source_branch="feature",
            source_commit="0123456789abcdef",
            source_run_url="https://github.com/ai-dynamo/snapshot/actions/runs/200",
            generated_at=START,
        )

    assert not (tmp_path / "previews").exists()


def test_prepare_preview_rejects_symlinked_artifact(tmp_path: Path) -> None:
    outside = tmp_path / "outside.json"
    outside.write_text(json.dumps(_result(run_id="200")), encoding="utf-8")
    results_dir = tmp_path / "current"
    results_dir.mkdir()
    link = results_dir / "vllm.json"
    try:
        link.symlink_to(outside)
    except OSError:
        pytest.skip("symlinks are not available")

    with pytest.raises(preview.PreviewValidationError, match="unexpected"):
        preview.prepare_preview(
            results_dir=results_dir,
            history_dir=tmp_path / "history",
            previews_dir=tmp_path / "previews",
            key="run-200",
            run_id="200",
            run_attempt=1,
            source_event="workflow_dispatch",
            source_branch="feature",
            source_commit="0123456789abcdef",
            source_run_url="https://github.com/ai-dynamo/snapshot/actions/runs/200",
            generated_at=START,
        )


def test_prepare_preview_prunes_only_expired_well_formed_previews(
    tmp_path: Path,
) -> None:
    previews = tmp_path / "previews"
    for key, expires in (
        ("run-1", START - timedelta(seconds=1)),
        ("run-2", START + timedelta(days=1)),
    ):
        root = previews / key
        root.mkdir(parents=True)
        (root / "preview.json").write_text(
            json.dumps({"key": key, "expiresAt": _timestamp(expires)}),
            encoding="utf-8",
        )
    malformed = previews / "run-3"
    malformed.mkdir()
    (malformed / "preview.json").write_text("not json", encoding="utf-8")
    unrelated = previews / "notes"
    unrelated.mkdir()

    removed = preview.prune_expired_previews(previews, now=START)

    assert removed == ["run-1"]
    assert not (previews / "run-1").exists()
    assert (previews / "run-2").is_dir()
    assert malformed.is_dir()
    assert unrelated.is_dir()


def test_pages_workflow_isolates_preview_write_permissions() -> None:
    repository_root = Path(__file__).resolve().parents[2]
    workflow = yaml.safe_load(
        (repository_root / ".github/workflows/e2e-benchmark-pages.yaml").read_text()
    )

    publisher = workflow["jobs"]["prepare-preview"]
    builder = workflow["jobs"]["build"]
    deployer = workflow["jobs"]["deploy"]
    assert publisher["permissions"] == {"actions": "read", "contents": "write"}
    assert publisher["steps"][0]["with"]["ref"] == "main"
    assert "workflow_run.event != 'schedule'" in publisher["if"]
    assert builder["permissions"] == {"contents": "read"}
    assert deployer["permissions"] == {"pages": "write", "id-token": "write"}


def _timestamp(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
