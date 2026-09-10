# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Validate, compare, and persist generic Snapshot E2E benchmark results."""

from __future__ import annotations

import argparse
import html
import json
import math
import os
import re
import statistics
import tempfile
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from snapshot_e2e.benchmark import (
    BENCHMARK_VERSION,
    SCHEMA_VERSION,
    TEST_TOTAL,
    VALID_OUTCOMES,
    format_timestamp,
    source_from_environment,
)


HISTORY_FORMAT_VERSION = 1
MANIFEST_VERSION = 1
SUPPORTED_SCHEMA_VERSIONS = {SCHEMA_VERSION}
DEFAULT_SUITE = "framework-checkpoint-restore"
DEFAULT_TEST = "test_framework_checkpoint_restore_serves_inference"


class ResultValidationError(ValueError):
    """A benchmark result cannot safely be read by this history version."""


@dataclass(frozen=True)
class StoredResult:
    result: dict[str, Any]
    raw_path: str


@dataclass(frozen=True)
class Collection:
    results: list[dict[str, Any]]
    warnings: list[str]


def validate_result(value: object, *, origin: str = "benchmark result") -> dict[str, Any]:
    """Return a JSON-safe copy after validating the versioned result envelope."""
    result = _mapping(value, origin)
    schema_version = _positive_integer(result.get("schemaVersion"), f"{origin}.schemaVersion")
    if schema_version not in SUPPORTED_SCHEMA_VERSIONS:
        supported = ", ".join(str(item) for item in sorted(SUPPORTED_SCHEMA_VERSIONS))
        raise ResultValidationError(
            f"{origin}.schemaVersion {schema_version} is unsupported; supported: {supported}"
        )
    _positive_integer(result.get("benchmarkVersion"), f"{origin}.benchmarkVersion")

    identity = _mapping(result.get("identity"), f"{origin}.identity")
    for field in ("suite", "case", "test", "runId"):
        _nonempty_string(identity.get(field), f"{origin}.identity.{field}")
    _positive_integer(identity.get("runAttempt"), f"{origin}.identity.runAttempt")

    outcome = _nonempty_string(result.get("outcome"), f"{origin}.outcome")
    if outcome not in VALID_OUTCOMES:
        raise ResultValidationError(f"{origin}.outcome {outcome!r} is not recognized")

    started = _parse_timestamp(result.get("startedAt"), f"{origin}.startedAt")
    finished = _parse_timestamp(result.get("finishedAt"), f"{origin}.finishedAt")
    if finished < started:
        raise ResultValidationError(f"{origin}.finishedAt precedes startedAt")

    _mapping(result.get("source"), f"{origin}.source")
    environment = _mapping(result.get("environment"), f"{origin}.environment")
    custom_dimensions = environment.get("comparisonDimensions")
    if custom_dimensions is not None:
        _mapping(custom_dimensions, f"{origin}.environment.comparisonDimensions")

    measurements = result.get("measurements")
    if not isinstance(measurements, list) or not measurements:
        raise ResultValidationError(f"{origin}.measurements must be a non-empty array")
    measurement_names: set[str] = set()
    for index, item in enumerate(measurements):
        location = f"{origin}.measurements[{index}]"
        measurement = _mapping(item, location)
        name = _nonempty_string(measurement.get("name"), f"{location}.name")
        if name in measurement_names:
            raise ResultValidationError(f"{origin} contains duplicate measurement {name!r}")
        measurement_names.add(name)
        _nonempty_string(measurement.get("displayName"), f"{location}.displayName")
        _nonempty_string(measurement.get("unit"), f"{location}.unit")
        status = _nonempty_string(measurement.get("status"), f"{location}.status")
        value = measurement.get("value")
        if status == "complete":
            _finite_number(value, f"{location}.value")
        elif status == "incomplete":
            if value is not None:
                raise ResultValidationError(
                    f"{location}.value must be null when status is incomplete"
                )
            _nonempty_string(measurement.get("missingReason"), f"{location}.missingReason")
        else:
            raise ResultValidationError(
                f"{location}.status must be 'complete' or 'incomplete'"
            )

    events = result.get("events")
    if not isinstance(events, list):
        raise ResultValidationError(f"{origin}.events must be an array")
    event_names: set[str] = set()
    for index, item in enumerate(events):
        location = f"{origin}.events[{index}]"
        event = _mapping(item, location)
        name = _nonempty_string(event.get("name"), f"{location}.name")
        if name in event_names:
            raise ResultValidationError(f"{origin} contains duplicate event {name!r}")
        event_names.add(name)
        _finite_number(event.get("offsetSeconds"), f"{location}.offsetSeconds")
        _parse_timestamp(event.get("timestamp"), f"{location}.timestamp")

    error = result.get("error")
    if error is not None:
        error_mapping = _mapping(error, f"{origin}.error")
        _nonempty_string(error_mapping.get("phase"), f"{origin}.error.phase")
        _nonempty_string(error_mapping.get("message"), f"{origin}.error.message")

    try:
        normalized = json.loads(json.dumps(result, allow_nan=False))
    except (TypeError, ValueError) as exc:
        raise ResultValidationError(f"{origin} is not valid JSON: {exc}") from exc
    normalized["startedAt"] = format_timestamp(started)
    normalized["finishedAt"] = format_timestamp(finished)
    for event, original in zip(normalized["events"], events, strict=True):
        event["timestamp"] = format_timestamp(
            _parse_timestamp(original["timestamp"], f"{origin}.events.timestamp")
        )
    return normalized


def load_result(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ResultValidationError(f"cannot read {path}: {exc}") from exc
    return validate_result(value, origin=str(path))


def result_identity(result: Mapping[str, Any]) -> tuple[str, str, str, str, int]:
    identity = result["identity"]
    return (
        str(identity["suite"]),
        str(identity["case"]),
        str(identity["test"]),
        str(identity["runId"]),
        int(identity["runAttempt"]),
    )


def collect_current_results(
    artifacts_dir: Path,
    *,
    expected_cases: Sequence[str],
    suite: str,
    test: str,
    run_id: str,
    run_attempt: int,
    generated_at: datetime,
    source: Mapping[str, Any],
) -> Collection:
    """Collect one valid result per expected case and make failures explicit.

    Results from earlier attempts of the same workflow run are accepted, and
    the newest attempt wins per case: "Re-run failed jobs" re-executes only the
    failed matrix jobs, so a passing framework's result stays in its original
    attempt's artifact and must not be mistaken for a missing one.
    """
    expected = list(dict.fromkeys(expected_cases))
    if not expected:
        raise ValueError("at least one expected benchmark case is required")
    found: dict[str, dict[str, Any]] = {}
    warnings: list[str] = []
    paths = sorted(artifacts_dir.rglob("*.json")) if artifacts_dir.exists() else []
    for path in paths:
        try:
            result = load_result(path)
        except ResultValidationError as exc:
            warnings.append(str(exc))
            continue
        identity = result["identity"]
        actual = (str(identity["suite"]), str(identity["test"]), str(identity["runId"]))
        wanted = (suite, test, run_id)
        attempt = int(identity["runAttempt"])
        if actual != wanted or attempt > run_attempt:
            warnings.append(
                f"ignored {path}: identity {actual + (attempt,)!r} does not belong to "
                f"run {wanted!r} attempt <= {run_attempt}"
            )
            continue
        case = str(identity["case"])
        if case not in expected:
            warnings.append(f"ignored {path}: unexpected benchmark case {case!r}")
            continue
        previous = found.get(case)
        if previous is not None:
            previous_attempt = int(previous["identity"]["runAttempt"])
            if attempt < previous_attempt:
                continue
            if attempt == previous_attempt:
                if _canonical_json(previous) != _canonical_json(result):
                    warnings.append(
                        f"ignored duplicate, conflicting result for case {case!r}: {path}"
                    )
                continue
        found[case] = result

    sibling_tag = next(
        (
            item["source"]["snapshotTag"]
            for item in found.values()
            if isinstance(item.get("source"), Mapping) and item["source"].get("snapshotTag")
        ),
        None,
    )
    synthesized_source = dict(source)
    if not synthesized_source.get("snapshotTag") and sibling_tag:
        synthesized_source["snapshotTag"] = sibling_tag

    results: list[dict[str, Any]] = []
    for case in expected:
        result = found.get(case)
        if result is None:
            detail = "no benchmark artifact was uploaded"
            if warnings:
                detail += "; see collection warnings"
            result = synthesize_missing_result(
                suite=suite,
                case=case,
                test=test,
                run_id=run_id,
                run_attempt=run_attempt,
                generated_at=generated_at,
                source=synthesized_source,
                message=detail,
            )
        results.append(result)
    return Collection(results=results, warnings=warnings)


def synthesize_missing_result(
    *,
    suite: str,
    case: str,
    test: str,
    run_id: str,
    run_attempt: int,
    generated_at: datetime,
    source: Mapping[str, Any],
    message: str,
) -> dict[str, Any]:
    timestamp = format_timestamp(generated_at)
    return validate_result(
        {
            "schemaVersion": SCHEMA_VERSION,
            "benchmarkVersion": BENCHMARK_VERSION,
            "identity": {
                "suite": suite,
                "case": case,
                "test": test,
                "runId": run_id,
                "runAttempt": run_attempt,
            },
            "outcome": "infrastructure_failed",
            "startedAt": timestamp,
            "finishedAt": timestamp,
            "source": dict(source),
            "environment": {
                "collectionError": "matrix job produced no valid benchmark artifact"
            },
            "measurements": [
                {
                    "name": TEST_TOTAL,
                    "displayName": "Full E2E test",
                    "unit": "seconds",
                    "value": None,
                    "status": "incomplete",
                    "missingReason": "test result artifact unavailable",
                }
            ],
            "events": [],
            "error": {"phase": "artifact_collection", "message": message},
        }
    )


def load_history(history_dir: Path) -> list[StoredResult]:
    root = history_dir / "results" / f"v{HISTORY_FORMAT_VERSION}"
    if not root.exists():
        return []
    stored: list[StoredResult] = []
    identities: dict[tuple[str, str, str, str, int], str] = {}
    for path in sorted(root.rglob("*.json")):
        result = load_result(path)
        identity = result_identity(result)
        relative = path.relative_to(history_dir).as_posix()
        if identity in identities:
            raise ResultValidationError(
                f"history contains duplicate identity {identity!r} in "
                f"{identities[identity]} and {relative}"
            )
        identities[identity] = relative
        stored.append(StoredResult(result=result, raw_path=relative))
    return stored


def store_results(history_dir: Path, current: Iterable[dict[str, Any]]) -> list[StoredResult]:
    """Add unseen identities, then deterministically rebuild derived history files."""
    history_dir.mkdir(parents=True, exist_ok=True)
    existing = load_history(history_dir)
    by_identity = {result_identity(item.result): item for item in existing}
    for value in current:
        result = validate_result(value)
        identity = result_identity(result)
        stored = by_identity.get(identity)
        if stored is not None:
            continue
        relative = raw_result_path(result)
        if any(item.raw_path == relative for item in by_identity.values()):
            raise ResultValidationError(f"raw history path collision at {relative}")
        stored = StoredResult(result=result, raw_path=relative)
        _write_json_atomic(history_dir / stored.raw_path, result)
        by_identity[identity] = stored
    stored_results = sorted(by_identity.values(), key=_stored_sort_key)
    rebuild_indexes(history_dir, stored_results)
    return stored_results


def raw_result_path(result: Mapping[str, Any]) -> str:
    identity = result["identity"]
    started = _parse_timestamp(result["startedAt"], "startedAt")
    parts = [
        "results",
        f"v{HISTORY_FORMAT_VERSION}",
        _path_segment(str(identity["suite"])),
        _path_segment(str(identity["case"])),
        _path_segment(str(identity["test"])),
        f"{started.year:04d}",
        f"{started.month:02d}",
        f"{started.day:02d}",
        f"{_path_segment(str(identity['runId']))}-{int(identity['runAttempt'])}.json",
    ]
    return "/".join(parts)


def rebuild_indexes(
    history_dir: Path,
    stored_results: Sequence[StoredResult] | None = None,
) -> dict[str, Any]:
    """Rebuild all derived history files from immutable raw result files."""
    stored = list(stored_results) if stored_results is not None else load_history(history_dir)
    stored.sort(key=_stored_sort_key)
    by_month: dict[str, list[StoredResult]] = {}
    for item in stored:
        month = str(item.result["startedAt"])[:7]
        by_month.setdefault(month, []).append(item)

    index_root = history_dir / "index" / f"v{HISTORY_FORMAT_VERSION}"
    index_root.mkdir(parents=True, exist_ok=True)
    wanted_paths: set[Path] = set()
    chunks: list[dict[str, Any]] = []
    for month in sorted(by_month):
        items = by_month[month]
        relative = Path("index") / f"v{HISTORY_FORMAT_VERSION}" / f"{month}.ndjson"
        path = history_dir / relative
        wanted_paths.add(path)
        lines = [
            _canonical_json(
                {
                    "rawPath": item.raw_path,
                    "comparisonKey": comparison_key(item.result),
                    "result": item.result,
                }
            )
            for item in items
        ]
        _write_text_atomic(path, "\n".join(lines) + "\n")
        chunks.append(
            {
                "path": relative.as_posix(),
                "month": month,
                "recordCount": len(items),
                "firstStartedAt": items[0].result["startedAt"],
                "lastStartedAt": items[-1].result["startedAt"],
            }
        )
    for stale in index_root.glob("*.ndjson"):
        if stale not in wanted_paths:
            stale.unlink()

    finished_times = [str(item.result["finishedAt"]) for item in stored]
    manifest = {
        "manifestVersion": MANIFEST_VERSION,
        "historyFormatVersion": HISTORY_FORMAT_VERSION,
        "supportedSchemaVersions": sorted(SUPPORTED_SCHEMA_VERSIONS),
        "recordCount": len(stored),
        "newestResultAt": max(finished_times) if finished_times else None,
        "chunks": list(reversed(chunks)),
    }
    _write_json_atomic(history_dir / "index" / "manifest.json", manifest)
    return manifest


def comparison_key(result: Mapping[str, Any]) -> str:
    """Canonical string form of the comparison dimensions.

    Written into every monthly index line so readers in other languages group
    results by this exact string instead of re-deriving the dimensions.
    """
    return _canonical_json(comparison_dimensions(result))


def comparison_dimensions(result: Mapping[str, Any]) -> dict[str, Any]:
    """Return stable environment dimensions which can materially affect timing.

    The resolved image digest is preferred over the tag so a re-pushed tag
    starts a new baseline while a re-tagged identical image does not.
    """
    identity = result["identity"]
    environment = result.get("environment", {})
    storage_value = environment.get("storage")
    storage = storage_value if isinstance(storage_value, Mapping) else {}
    image_pulls_value = environment.get("imagePulls")
    image_pulls = image_pulls_value if isinstance(image_pulls_value, Mapping) else {}
    generic_gpu_models = _gpu_models(environment.get("gpus"))
    source_gpu_models = _gpu_models(environment.get("sourceGpus")) or generic_gpu_models
    restore_gpu_models = _gpu_models(environment.get("restoreGpus")) or generic_gpu_models
    access_modes = storage.get("accessModes")
    dimensions = {
        "schemaVersion": result["schemaVersion"],
        "benchmarkVersion": result["benchmarkVersion"],
        "suite": identity["suite"],
        "case": identity["case"],
        "test": identity["test"],
        "sourceGpuModels": source_gpu_models,
        "restoreGpuModels": restore_gpu_models,
        "frameworkImage": (
            environment.get("frameworkImageDigest") or environment.get("frameworkImage")
        ),
        "model": environment.get("model"),
        "storage": {
            "storageClass": storage.get(
                "storageClass", environment.get("storageClass")
            ),
            "type": storage.get("type"),
            "provisioner": storage.get("provisioner"),
            "requestedSize": storage.get("requestedSize"),
            "capacity": storage.get("capacity"),
            "accessModes": sorted(access_modes) if isinstance(access_modes, list) else [],
            "volumeMode": storage.get("volumeMode"),
        },
        "modelCacheMode": environment.get("modelCacheMode"),
        "imageCache": {
            role: _cache_hit(image_pulls.get(role))
            for role in ("source", "restore")
        },
        "datadogGpuMonitoringMode": environment.get("datadogGpuMonitoringMode"),
        "custom": environment.get("comparisonDimensions", {}),
    }
    return json.loads(json.dumps(dimensions, sort_keys=True, allow_nan=False))


def compare_result(
    current: Mapping[str, Any],
    history: Sequence[StoredResult],
) -> list[dict[str, Any]]:
    dimensions = comparison_dimensions(current)
    current_identity = result_identity(current)
    current_sort_key = _result_sort_key(current)
    compatible = [
        item.result
        for item in history
        if item.result.get("outcome") == "passed"
        and result_identity(item.result) != current_identity
        and _result_sort_key(item.result) < current_sort_key
        and comparison_dimensions(item.result) == dimensions
    ]
    compatible.sort(key=_result_sort_key, reverse=True)
    output: list[dict[str, Any]] = []
    for measurement in current["measurements"]:
        comparison: dict[str, Any] = {
            "name": measurement["name"],
            "displayName": measurement["displayName"],
            "unit": measurement["unit"],
            "current": measurement["value"],
            "previous": None,
            "median7": None,
        }
        if measurement["status"] != "complete":
            output.append(comparison)
            continue
        candidates: list[tuple[dict[str, Any], Mapping[str, Any]]] = []
        for prior in compatible:
            prior_measurement = _measurement(prior, str(measurement["name"]))
            if (
                prior_measurement is not None
                and prior_measurement.get("status") == "complete"
                and prior_measurement.get("unit") == measurement["unit"]
            ):
                candidates.append((prior, prior_measurement))
        if candidates:
            prior, prior_measurement = candidates[0]
            prior_value = float(prior_measurement["value"])
            comparison["previous"] = {
                "value": prior_value,
                "deltaPercent": _delta_percent(float(measurement["value"]), prior_value),
                "startedAt": prior["startedAt"],
                "runUrl": prior.get("source", {}).get("runUrl"),
            }
            baseline = candidates[:7]
            median_value = float(
                statistics.median(float(item[1]["value"]) for item in baseline)
            )
            comparison["median7"] = {
                "value": median_value,
                "deltaPercent": _delta_percent(
                    float(measurement["value"]), median_value
                ),
                "sampleSize": len(baseline),
            }
        output.append(comparison)
    return output


def aggregate(
    *,
    artifacts_dir: Path,
    history_dir: Path,
    output_dir: Path,
    expected_cases: Sequence[str],
    suite: str,
    test: str,
    run_id: str,
    run_attempt: int,
    generated_at: datetime,
    source: Mapping[str, Any],
    publish: bool,
) -> dict[str, Any]:
    collection = collect_current_results(
        artifacts_dir,
        expected_cases=expected_cases,
        suite=suite,
        test=test,
        run_id=run_id,
        run_attempt=run_attempt,
        generated_at=generated_at,
        source=source,
    )
    history = load_history(history_dir)
    compared = [
        {"result": result, "comparisons": compare_result(result, history)}
        for result in collection.results
    ]
    output = {
        "formatVersion": HISTORY_FORMAT_VERSION,
        "generatedAt": format_timestamp(generated_at),
        "published": publish,
        "collectionWarnings": collection.warnings,
        "benchmarks": compared,
    }
    output_dir.mkdir(parents=True, exist_ok=True)
    _write_json_atomic(output_dir / "comparison.json", output)
    current_dir = output_dir / "current"
    for result in collection.results:
        identity = result["identity"]
        filename = (
            f"{_path_segment(str(identity['suite']))}-"
            f"{_path_segment(str(identity['case']))}-"
            f"{_path_segment(str(identity['runId']))}-"
            f"{int(identity['runAttempt'])}.json"
        )
        _write_json_atomic(current_dir / filename, result)
    summary = render_summary(output)
    _write_text_atomic(output_dir / "summary.md", summary)
    if publish:
        store_results(history_dir, collection.results)
    return output


def render_summary(aggregate_result: Mapping[str, Any]) -> str:
    lines = ["## E2E framework benchmark comparison", ""]
    lines.append(
        "This scheduled run is eligible for durable history publication."
        if aggregate_result.get("published")
        else "This run is comparison-only; durable history was not modified."
    )
    lines.append("")
    for item in aggregate_result["benchmarks"]:
        result = item["result"]
        identity = result["identity"]
        outcome = str(result["outcome"])
        lines.extend(
            [
                f"### {_markdown(str(identity['case']))} — {_markdown(outcome)}",
                "",
                f"- GPU: {_markdown(_gpu_display(result.get('environment', {})))}",
                f"- Storage: {_markdown(_storage_display(result.get('environment', {})))}",
                f"- Image cache: {_markdown(_image_cache_display(result.get('environment', {})))}",
                f"- Commit: {_commit_link(result.get('source', {}))}",
                f"- Workflow: {_run_link(result.get('source', {}))}",
                "",
                "| Measurement | Current | Previous | Delta | Median (last 7) | Delta |",
                "| --- | ---: | ---: | ---: | ---: | ---: |",
            ]
        )
        for comparison in item["comparisons"]:
            unit = str(comparison["unit"])
            previous = comparison.get("previous")
            median = comparison.get("median7")
            lines.append(
                "| "
                + " | ".join(
                    [
                        _markdown(str(comparison["displayName"])),
                        _format_value(comparison.get("current"), unit),
                        _linked_value(previous, unit),
                        _format_delta(previous),
                        _median_value(median, unit),
                        _format_delta(median),
                    ]
                )
                + " |"
            )
        lines.append("")

    warnings = aggregate_result.get("collectionWarnings", [])
    if warnings:
        lines.extend(["### Collection warnings", ""])
        lines.extend(f"- {_markdown(str(warning))}" for warning in warnings)
        lines.append("")
    return "\n".join(lines)


def _history_from_cli(path: Path) -> Path:
    path.mkdir(parents=True, exist_ok=True)
    return path


def _parse_cli_timestamp(value: str | None) -> datetime:
    if value is None:
        return datetime.now(timezone.utc)
    return _parse_timestamp(value, "--generated-at")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    aggregate_parser = subparsers.add_parser(
        "aggregate", help="validate, compare, and optionally persist current results"
    )
    aggregate_parser.add_argument("--artifacts-dir", type=Path, required=True)
    aggregate_parser.add_argument("--history-dir", type=Path, required=True)
    aggregate_parser.add_argument("--output-dir", type=Path, required=True)
    aggregate_parser.add_argument("--expected-case", action="append", required=True)
    aggregate_parser.add_argument("--suite", default=DEFAULT_SUITE)
    aggregate_parser.add_argument("--test", default=DEFAULT_TEST)
    aggregate_parser.add_argument("--run-id", default=os.environ.get("GITHUB_RUN_ID"))
    aggregate_parser.add_argument(
        "--run-attempt", type=int, default=os.environ.get("GITHUB_RUN_ATTEMPT")
    )
    aggregate_parser.add_argument("--generated-at")
    aggregate_parser.add_argument("--summary-file", type=Path)
    aggregate_parser.add_argument("--publish", action="store_true")

    rebuild_parser = subparsers.add_parser(
        "rebuild", help="rebuild monthly indexes and the manifest from raw results"
    )
    rebuild_parser.add_argument("--history-dir", type=Path, required=True)
    args = parser.parse_args(argv)

    if args.command == "rebuild":
        manifest = rebuild_indexes(_history_from_cli(args.history_dir))
        print(json.dumps(manifest, indent=2))
        return 0

    if not args.run_id:
        parser.error("--run-id or GITHUB_RUN_ID is required")
    if args.run_attempt is None:
        parser.error("--run-attempt or GITHUB_RUN_ATTEMPT is required")
    generated_at = _parse_cli_timestamp(args.generated_at)
    output = aggregate(
        artifacts_dir=args.artifacts_dir,
        history_dir=_history_from_cli(args.history_dir),
        output_dir=args.output_dir,
        expected_cases=args.expected_case,
        suite=args.suite,
        test=args.test,
        run_id=str(args.run_id),
        run_attempt=int(args.run_attempt),
        generated_at=generated_at,
        source=source_from_environment(str(args.run_id)),
        publish=bool(args.publish),
    )
    summary = render_summary(output)
    print(summary)
    if args.summary_file:
        args.summary_file.parent.mkdir(parents=True, exist_ok=True)
        with args.summary_file.open("a", encoding="utf-8") as handle:
            handle.write(summary)
            if not summary.endswith("\n"):
                handle.write("\n")
    return 0


def _mapping(value: object, location: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise ResultValidationError(f"{location} must be an object")
    return value


def _positive_integer(value: object, location: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 1:
        raise ResultValidationError(f"{location} must be a positive integer")
    return value


def _nonempty_string(value: object, location: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ResultValidationError(f"{location} must be a non-empty string")
    return value


def _finite_number(value: object, location: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ResultValidationError(f"{location} must be numeric")
    number = float(value)
    if not math.isfinite(number):
        raise ResultValidationError(f"{location} must be finite")
    return number


def _parse_timestamp(value: object, location: str) -> datetime:
    text = _nonempty_string(value, location)
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ResultValidationError(f"{location} is not an RFC3339 timestamp") from exc
    if parsed.tzinfo is None:
        raise ResultValidationError(f"{location} must include a timezone")
    return parsed.astimezone(timezone.utc)


def _canonical_json(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def _path_segment(value: str) -> str:
    safe = re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-.")
    if not safe:
        raise ResultValidationError(f"cannot create a history path from {value!r}")
    return safe


def _write_json_atomic(path: Path, value: object) -> None:
    _write_text_atomic(path, json.dumps(value, indent=2, sort_keys=True) + "\n")


def _write_text_atomic(path: Path, value: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        "w", encoding="utf-8", dir=path.parent, delete=False
    ) as handle:
        temporary = Path(handle.name)
        try:
            handle.write(value)
        except BaseException:
            handle.close()
            temporary.unlink(missing_ok=True)
            raise
    try:
        os.replace(temporary, path)
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise


def _stored_sort_key(item: StoredResult) -> tuple[datetime, str, int, str, str, str]:
    return _result_sort_key(item.result)


def _result_sort_key(result: Mapping[str, Any]) -> tuple[datetime, str, int, str, str, str]:
    identity = result["identity"]
    return (
        _parse_timestamp(result["startedAt"], "startedAt"),
        str(identity["runId"]),
        int(identity["runAttempt"]),
        str(identity["suite"]),
        str(identity["case"]),
        str(identity["test"]),
    )


def _gpu_models(value: object) -> list[str]:
    if not isinstance(value, list):
        return []
    return sorted(
        {
            str(item["model"])
            for item in value
            if isinstance(item, Mapping) and item.get("model")
        }
    )


def _cache_hit(value: object) -> bool | None:
    if not isinstance(value, Mapping) or not isinstance(value.get("cacheHit"), bool):
        return None
    return bool(value["cacheHit"])


def _measurement(
    result: Mapping[str, Any], name: str
) -> Mapping[str, Any] | None:
    return next(
        (item for item in result["measurements"] if item.get("name") == name),
        None,
    )


def _delta_percent(current: float, baseline: float) -> float | None:
    if baseline == 0:
        return None
    return round(((current - baseline) / baseline) * 100.0, 6)


def _markdown(value: str) -> str:
    escaped = html.escape(value, quote=False)
    return escaped.replace("\\", "\\\\").replace("|", "\\|").replace("\n", " ")


def _gpu_display(environment: Mapping[str, Any]) -> str:
    generic = _gpu_models(environment.get("gpus"))
    source = _gpu_models(environment.get("sourceGpus")) or generic
    restore = _gpu_models(environment.get("restoreGpus")) or generic
    if not source and not restore:
        return "unknown"
    if source == restore:
        return ", ".join(source)
    return f"source {', '.join(source) or 'unknown'}; restore {', '.join(restore) or 'unknown'}"


def _storage_display(environment: Mapping[str, Any]) -> str:
    storage = environment.get("storage")
    if not isinstance(storage, Mapping):
        return str(environment.get("storageClass") or "unknown")
    storage_type = storage.get("type") or "unknown type"
    storage_class = storage.get("storageClass") or "unknown class"
    requested = storage.get("requestedSize") or "unknown size"
    capacity = storage.get("capacity") or "unknown capacity"
    return f"{storage_type}, class {storage_class}, requested {requested}, capacity {capacity}"


def _image_cache_display(environment: Mapping[str, Any]) -> str:
    pulls = environment.get("imagePulls")
    if not isinstance(pulls, Mapping):
        return "unknown"
    labels = []
    for role in ("source", "restore"):
        hit = _cache_hit(pulls.get(role))
        state = "cached" if hit is True else "cold" if hit is False else "unknown"
        labels.append(f"{role} {state}")
    return ", ".join(labels)


def _commit_link(source: object) -> str:
    if not isinstance(source, Mapping):
        return "unknown"
    commit = source.get("commit")
    if not commit:
        return "unknown"
    label = _markdown(str(commit)[:8])
    run_url = source.get("runUrl")
    if (
        isinstance(commit, str)
        and re.fullmatch(r"[0-9a-fA-F]{7,64}", commit)
        and isinstance(run_url, str)
        and "/actions/runs/" in run_url
    ):
        repository_url = run_url.split("/actions/runs/", 1)[0]
        link = _safe_link_url(f"{repository_url}/commit/{commit}")
        if link:
            return f"[{label}]({link})"
    return label


def _run_link(source: object) -> str:
    if not isinstance(source, Mapping) or not source.get("runUrl"):
        return "unavailable"
    link = _safe_link_url(source["runUrl"])
    return f"[open run]({link})" if link else "unavailable"


def _format_value(value: object, unit: str) -> str:
    if value is None:
        return "—"
    number = float(value)
    if unit == "seconds":
        return f"{number:.2f} s"
    if unit == "bytes":
        return _format_bytes(number)
    return f"{number:.2f} {_markdown(unit)}"


def _format_bytes(value: float) -> str:
    units = ["B", "KiB", "MiB", "GiB", "TiB"]
    current = value
    for unit in units:
        if abs(current) < 1024 or unit == units[-1]:
            return f"{current:.2f} {unit}"
        current /= 1024
    return f"{value:.0f} B"


def _linked_value(comparison: object, unit: str) -> str:
    if not isinstance(comparison, Mapping):
        return "—"
    rendered = _format_value(comparison.get("value"), unit)
    run_url = _safe_link_url(comparison.get("runUrl"))
    return f"[{rendered}]({run_url})" if run_url else rendered


def _median_value(comparison: object, unit: str) -> str:
    if not isinstance(comparison, Mapping):
        return "—"
    value = _format_value(comparison.get("value"), unit)
    return f"{value} (n={int(comparison['sampleSize'])})"


def _format_delta(comparison: object) -> str:
    if not isinstance(comparison, Mapping) or comparison.get("deltaPercent") is None:
        return "—"
    return f"{float(comparison['deltaPercent']):+.1f}%"


def _safe_link_url(value: object) -> str | None:
    if not isinstance(value, str) or not value.startswith(("https://", "http://")):
        return None
    if any(character in value for character in ("\n", "\r", "<", ">")):
        return None
    return value.replace("(", "%28").replace(")", "%29").replace(" ", "%20")


if __name__ == "__main__":
    raise SystemExit(main())
