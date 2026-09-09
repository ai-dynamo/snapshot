# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import copy
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
import yaml

from snapshot_e2e import benchmark_history as history


START = datetime(2026, 8, 25, 1, 2, 3, tzinfo=timezone.utc)


def test_validate_result_preserves_unknown_fields() -> None:
    result = _result(extra={"futureField": {"answer": 42}})
    result["startedAt"] = "2026-08-25T04:02:03+03:00"

    validated = history.validate_result(result)

    assert validated["futureField"] == {"answer": 42}
    assert validated["startedAt"] == "2026-08-25T01:02:03.000Z"
    assert validated is not result


@pytest.mark.parametrize(
    "change",
    [
        lambda result: result.update(schemaVersion=99),
        lambda result: result["identity"].update(runAttempt=0),
        lambda result: result.update(outcome="maybe"),
        lambda result: result["measurements"][0].update(value=None),
        lambda result: result["measurements"][0].update(value=float("nan")),
        lambda result: result["events"].append(copy.deepcopy(result["events"][0])),
    ],
)
def test_validate_result_rejects_unsafe_records(change) -> None:
    result = _result()
    change(result)

    with pytest.raises(history.ResultValidationError):
        history.validate_result(result)


def test_store_is_idempotent_and_rebuilds_monthly_indexes(tmp_path: Path) -> None:
    august = _result(run_id="100", started=START, value=10)
    september = _result(
        run_id="200", started=datetime(2026, 9, 2, tzinfo=timezone.utc), value=20
    )

    stored = history.store_results(tmp_path, [september, august])
    history.store_results(tmp_path, [copy.deepcopy(august)])

    raw = sorted((tmp_path / "results" / "v1").rglob("*.json"))
    assert len(raw) == 2
    assert "/test_framework/2026/08/25/100-1.json" in raw[0].as_posix()
    assert [item.result["identity"]["runId"] for item in stored] == ["100", "200"]

    august_index = _ndjson(tmp_path / "index" / "v1" / "2026-08.ndjson")
    september_index = _ndjson(tmp_path / "index" / "v1" / "2026-09.ndjson")
    assert august_index[0]["result"]["identity"]["runId"] == "100"
    assert september_index[0]["result"]["identity"]["runId"] == "200"
    assert august_index[0]["rawPath"].startswith("results/v1/")

    manifest = json.loads((tmp_path / "index" / "manifest.json").read_text())
    assert manifest["recordCount"] == 2
    assert manifest["supportedSchemaVersions"] == [1]
    assert [chunk["month"] for chunk in manifest["chunks"]] == [
        "2026-09",
        "2026-08",
    ]


def test_same_identity_is_ignored_without_creating_a_duplicate(tmp_path: Path) -> None:
    original = _result(run_id="100", value=10)
    corrected = _result(run_id="100", value=11)

    history.store_results(tmp_path, [original])
    history.store_results(tmp_path, [corrected])

    stored = history.load_history(tmp_path)
    assert len(stored) == 1
    assert stored[0].result["measurements"][0]["value"] == 10


def test_comparison_uses_previous_and_median_of_previous_seven() -> None:
    stored = [
        history.StoredResult(
            result=_result(
                run_id=str(index),
                started=START + timedelta(days=index),
                value=float(index),
            ),
            raw_path=f"result-{index}.json",
        )
        for index in range(1, 9)
    ]
    current = _result(run_id="9", started=START + timedelta(days=9), value=20)

    comparison = history.compare_result(current, stored)[0]

    assert comparison["previous"] == {
        "value": 8.0,
        "deltaPercent": 150.0,
        "startedAt": "2026-09-02T01:02:03.000Z",
        "runUrl": "https://github.com/ai-dynamo/snapshot/actions/runs/8",
    }
    assert comparison["median7"] == {
        "value": 5.0,
        "deltaPercent": 300.0,
        "sampleSize": 7,
    }


@pytest.mark.parametrize(
    "change",
    [
        lambda result: result.update(benchmarkVersion=3),
        lambda result: result["environment"]["sourceGpus"][0].update(
            model="NVIDIA H100"
        ),
        lambda result: result["environment"].update(frameworkImage="image@sha256:2"),
        lambda result: result["environment"]["storage"].update(type="Premium_LRS"),
        lambda result: result["environment"]["imagePulls"]["source"].update(
            cacheHit=False
        ),
        lambda result: result["environment"].update(
            comparisonDimensions={"replicas": 2}
        ),
    ],
)
def test_comparison_does_not_cross_environment_dimensions(change) -> None:
    prior = _result(run_id="1", started=START, value=10)
    current = _result(run_id="2", started=START + timedelta(days=1), value=20)
    change(prior)

    comparison = history.compare_result(
        current, [history.StoredResult(prior, "prior.json")]
    )[0]

    assert comparison["previous"] is None
    assert comparison["median7"] is None


def test_failed_history_is_visible_but_excluded_from_baseline(tmp_path: Path) -> None:
    passed = _result(run_id="1", started=START, value=10)
    failed = _result(
        run_id="2", started=START + timedelta(days=1), value=99, outcome="failed"
    )
    stored = history.store_results(tmp_path, [passed, failed])
    current = _result(run_id="3", started=START + timedelta(days=2), value=12)

    comparison = history.compare_result(current, stored)[0]

    assert len(stored) == 2
    assert comparison["previous"]["value"] == 10
    index = _ndjson(tmp_path / "index" / "v1" / "2026-08.ndjson")
    assert [entry["result"]["outcome"] for entry in index] == ["passed", "failed"]


def test_collection_synthesizes_missing_and_invalid_artifacts(tmp_path: Path) -> None:
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    valid = _result(case="vllm", run_id="123", started=START)
    (artifacts / "vllm.json").write_text(json.dumps(valid), encoding="utf-8")
    (artifacts / "broken.json").write_text("not json", encoding="utf-8")

    collection = history.collect_current_results(
        artifacts,
        expected_cases=["vllm", "sglang"],
        suite="framework-checkpoint-restore",
        test="test_framework",
        run_id="123",
        run_attempt=1,
        generated_at=START,
        source=valid["source"],
    )

    assert [result["identity"]["case"] for result in collection.results] == [
        "vllm",
        "sglang",
    ]
    assert collection.results[0]["outcome"] == "passed"
    assert collection.results[1]["outcome"] == "infrastructure_failed"
    assert collection.results[1]["measurements"][0]["value"] is None
    assert len(collection.warnings) == 1


def test_aggregate_writes_comparison_artifact_and_readable_summary(
    tmp_path: Path,
) -> None:
    history_dir = tmp_path / "history"
    prior = _result(run_id="1", started=START, value=10)
    history.store_results(history_dir, [prior])
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    current = _result(run_id="2", started=START + timedelta(days=1), value=12)
    (artifacts / "current.json").write_text(json.dumps(current), encoding="utf-8")

    output = history.aggregate(
        artifacts_dir=artifacts,
        history_dir=history_dir,
        output_dir=tmp_path / "output",
        expected_cases=["vllm"],
        suite="framework-checkpoint-restore",
        test="test_framework",
        run_id="2",
        run_attempt=1,
        generated_at=START + timedelta(days=1),
        source=current["source"],
        publish=False,
    )

    assert output["published"] is False
    assert (tmp_path / "output" / "comparison.json").is_file()
    assert len(list((tmp_path / "output" / "current").glob("*.json"))) == 1
    summary = (tmp_path / "output" / "summary.md").read_text()
    assert "vllm — passed" in summary
    assert "Standard_LRS, class azurefile-csi, requested 64Gi, capacity 64Gi" in summary
    assert "source cached, restore cached" in summary
    assert "12.00 s" in summary
    assert "+20.0%" in summary
    assert "comparison-only" in summary
    assert len(history.load_history(history_dir)) == 1


def test_rebuild_restores_derived_files_from_raw_results(tmp_path: Path) -> None:
    history.store_results(tmp_path, [_result()])
    (tmp_path / "index" / "v1" / "2026-08.ndjson").unlink()
    (tmp_path / "index" / "manifest.json").unlink()

    manifest = history.rebuild_indexes(tmp_path)

    assert manifest["recordCount"] == 1
    assert (tmp_path / "index" / "v1" / "2026-08.ndjson").is_file()
    assert (tmp_path / "index" / "manifest.json").is_file()


def test_summary_escapes_untrusted_text_and_rejects_unsafe_links() -> None:
    result = _result(case="bad|<script>")
    result["source"]["runUrl"] = "javascript:alert(1)"
    output = {
        "published": False,
        "collectionWarnings": [],
        "benchmarks": [{"result": result, "comparisons": []}],
    }

    summary = history.render_summary(output)

    assert "bad\\|&lt;script&gt;" in summary
    assert "javascript:" not in summary
    assert "Workflow: unavailable" in summary


def test_only_scheduled_main_history_job_has_write_permission() -> None:
    repository_root = Path(__file__).resolve().parents[2]
    workflow = yaml.safe_load(
        (repository_root / ".github/workflows/e2e-frameworks.yaml").read_text()
    )

    read_only = workflow["jobs"]["benchmark-history-read-only"]
    publisher = workflow["jobs"]["benchmark-history-publish"]
    assert read_only["permissions"]["contents"] == "read"
    assert publisher["permissions"]["contents"] == "write"
    assert "github.event_name == 'schedule'" in publisher["if"]
    assert "github.ref == 'refs/heads/main'" in publisher["if"]
    assert publisher["concurrency"]["cancel-in-progress"] is False


def _result(
    *,
    case: str = "vllm",
    run_id: str = "1",
    started: datetime = START,
    value: float = 10,
    outcome: str = "passed",
    extra: dict | None = None,
) -> dict:
    finished = started + timedelta(seconds=value)
    result = {
        "schemaVersion": 1,
        "benchmarkVersion": 2,
        "identity": {
            "suite": "framework-checkpoint-restore",
            "case": case,
            "test": "test_framework",
            "runId": run_id,
            "runAttempt": 1,
        },
        "outcome": outcome,
        "startedAt": _timestamp(started),
        "finishedAt": _timestamp(finished),
        "source": {
            "commit": "0123456789abcdef",
            "ref": "refs/heads/main",
            "event": "schedule",
            "runUrl": f"https://github.com/ai-dynamo/snapshot/actions/runs/{run_id}",
            "snapshotTag": "v0.0.0-test",
        },
        "environment": {
            "model": "Qwen/Qwen3-0.6B",
            "frameworkImage": "image@sha256:1",
            "storageClass": "azurefile-csi",
            "modelCacheMode": "shared-nfs",
            "datadogGpuMonitoringMode": "disable",
            "sourceGpus": [
                {
                    "model": "NVIDIA A100-SXM4-80GB",
                    "uuid": "GPU-source",
                    "driverVersion": "595.58.03",
                }
            ],
            "restoreGpus": [
                {
                    "model": "NVIDIA A100-SXM4-80GB",
                    "uuid": "GPU-restore",
                    "driverVersion": "595.58.03",
                }
            ],
            "storage": {
                "storageClass": "azurefile-csi",
                "type": "Standard_LRS",
                "provisioner": "file.csi.azure.com",
                "requestedSize": "64Gi",
                "capacity": "64Gi",
                "accessModes": ["ReadWriteMany"],
                "volumeMode": "Filesystem",
            },
            "imagePulls": {
                "source": {"cacheHit": True},
                "restore": {"cacheHit": True},
            },
        },
        "measurements": [
            {
                "name": "checkpoint.duration",
                "displayName": "Checkpoint",
                "unit": "seconds",
                "value": value,
                "status": "complete",
            }
        ],
        "events": [
            {
                "name": "test.started",
                "offsetSeconds": 0,
                "timestamp": _timestamp(started),
            }
        ],
        "error": None,
    }
    result.update(extra or {})
    return result


def _timestamp(value: datetime) -> str:
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _ndjson(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines()]
