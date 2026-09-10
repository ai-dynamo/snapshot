# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Generic, versioned benchmark results for Snapshot e2e tests."""

from __future__ import annotations

import argparse
import csv
import json
import os
import re
import tempfile
import time
import uuid
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Protocol


SCHEMA_VERSION = 1
BENCHMARK_VERSION = 2
TEST_TOTAL = "test.total.duration"
VALID_OUTCOMES = {
    "passed",
    "failed",
    "timed_out",
    "skipped",
    "infrastructure_failed",
}
MAX_ERROR_MESSAGE_LENGTH = 4000
NVIDIA_SMI_QUERY = (
    "nvidia-smi --query-gpu=name,uuid,driver_version "
    "--format=csv,noheader,nounits"
)
PUBLIC_STORAGE_PARAMETER_KEYS = ("skuName", "type", "storageType")


class Clock(Protocol):
    def monotonic(self) -> float: ...

    def now(self) -> datetime: ...


class SystemClock:
    def monotonic(self) -> float:
        return time.monotonic()

    def now(self) -> datetime:
        return datetime.now(timezone.utc)


@dataclass
class _Measurement:
    name: str
    display_name: str
    unit: str
    started_monotonic: float | None = None
    start_event: str | None = None
    end_event: str | None = None
    value: float | None = None


@dataclass(frozen=True)
class AgentTimingSummary:
    """One structured timing summary emitted by the Snapshot agent."""

    completed_at: datetime
    duration_seconds: float
    phases_seconds: dict[str, float]
    started_to_complete_seconds: float | None


@dataclass(frozen=True)
class ImagePullSummary:
    """Image pull result reconstructed from kubelet Pod events."""

    duration_seconds: float
    including_wait_seconds: float
    cache_hit: bool
    image_size_bytes: int | None


def parse_nvidia_smi_csv(output: str) -> list[dict[str, str]]:
    """Parses the stable, headerless GPU query used by framework tests."""
    gpus: list[dict[str, str]] = []
    for row in csv.reader(line for line in output.splitlines() if line.strip()):
        if len(row) != 3:
            raise ValueError(f"expected three nvidia-smi columns, got {row!r}")
        model, gpu_uuid, driver_version = (value.strip() for value in row)
        if not all((model, gpu_uuid, driver_version)):
            raise ValueError(f"nvidia-smi returned an empty GPU field: {row!r}")
        gpus.append(
            {
                "model": model,
                "uuid": gpu_uuid,
                "driverVersion": driver_version,
            }
        )
    if not gpus:
        raise ValueError("nvidia-smi returned no GPUs")
    return gpus


def public_storage_parameters(
    parameters: Mapping[str, str] | None,
) -> dict[str, str]:
    """Returns only benchmark-relevant StorageClass parameters.

    StorageClass parameters may include infrastructure identifiers and secret
    references. Benchmark artifacts are retained outside the cluster, so only
    the allowlisted storage-type fields are safe and useful to publish.
    """
    parameters = parameters or {}
    return {
        key: parameters[key]
        for key in PUBLIC_STORAGE_PARAMETER_KEYS
        if key in parameters
    }


class BenchmarkRecorder:
    """Records a single test case as events and unit-bearing measurements."""

    def __init__(
        self,
        *,
        suite: str,
        case: str,
        test: str,
        environment: Mapping[str, Any] | None = None,
        result_dir: Path | None = None,
        clock: Clock | None = None,
        run_id: str | None = None,
        run_attempt: int | None = None,
        start_test: bool = True,
    ) -> None:
        self.suite = suite
        self.case = case
        self.test = test
        self.environment: dict[str, Any] = dict(environment or {})
        self._clock = clock or SystemClock()
        self._result_dir = result_dir or result_directory()
        self._run_id = run_id or os.environ.get("GITHUB_RUN_ID") or f"local-{uuid.uuid4()}"
        self._run_attempt = run_attempt or _integer_environment("GITHUB_RUN_ATTEMPT", 1)
        self._started_monotonic = self._clock.monotonic()
        self._started_at = self._clock.now()
        self._events: list[dict[str, Any]] = []
        self._event_names: set[str] = set()
        self._event_wall_times: dict[str, datetime] = {}
        self._measurements: dict[str, _Measurement] = {}
        self._outcome: str | None = None
        self._error: dict[str, str] | None = None
        self._finished_at: datetime | None = None
        self._result_path: Path | None = None

        self.define_duration(TEST_TOTAL, "Full E2E test")
        if start_test:
            self.start_duration(TEST_TOTAL, event="test.started", captured=self._capture_started())

    def define_duration(self, name: str, display_name: str) -> None:
        self.define_measurement(name, display_name, "seconds")

    def define_measurement(self, name: str, display_name: str, unit: str) -> None:
        if name in self._measurements:
            raise ValueError(f"measurement {name!r} is already defined")
        if not all((name, display_name, unit)):
            raise ValueError("measurement name, display name, and unit must be non-empty")
        self._measurements[name] = _Measurement(
            name=name,
            display_name=display_name,
            unit=unit,
        )

    def record_measurement(
        self,
        name: str,
        display_name: str,
        unit: str,
        value: float,
    ) -> None:
        """Records a scalar such as bytes or requests/second in the same envelope."""
        measurement = self._measurements.get(name)
        if measurement is None:
            self.define_measurement(name, display_name, unit)
            measurement = self._measurements[name]
        elif measurement.display_name != display_name or measurement.unit != unit:
            raise ValueError(f"measurement {name!r} metadata does not match its definition")
        if measurement.started_monotonic is not None or measurement.value is not None:
            raise ValueError(f"measurement {name!r} has already been recorded")
        measurement.value = float(value)

    def start_duration(
        self,
        name: str,
        *,
        event: str | None = None,
        captured: tuple[float, datetime] | None = None,
    ) -> None:
        measurement = self._measurement(name)
        if measurement.unit != "seconds":
            raise ValueError(f"timed measurement {name!r} must use seconds")
        if measurement.started_monotonic is not None or measurement.value is not None:
            raise ValueError(f"measurement {name!r} has already started")
        monotonic, wall = captured or self._capture()
        measurement.started_monotonic = monotonic
        measurement.start_event = event
        if event:
            self._add_event(event, monotonic, wall)

    def start_duration_at(
        self,
        name: str,
        timestamp: datetime,
        *,
        event: str | None = None,
    ) -> None:
        """Starts a duration at an externally timestamped event.

        Kubernetes events are observed after they are emitted. Estimate the
        corresponding monotonic boundary once, then continue measuring with
        the runner's monotonic clock. This removes poll delay from the start
        without repeatedly subtracting wall clocks across machines.
        """
        timestamp = _utc_datetime(timestamp)
        monotonic, wall = self._capture()
        age_seconds = max(0.0, (wall - timestamp).total_seconds())
        estimated_monotonic = max(
            self._started_monotonic,
            monotonic - age_seconds,
        )
        self.start_duration(
            name,
            event=event,
            captured=(estimated_monotonic, timestamp),
        )

    def finish_duration(self, name: str, *, event: str | None = None) -> float:
        values = self.finish_durations([name], event=event)
        return values[name]

    def finish_durations(
        self,
        names: Sequence[str],
        *,
        event: str | None = None,
    ) -> dict[str, float]:
        if len(set(names)) != len(names):
            raise ValueError("a measurement cannot be finished twice in one call")
        measurements = [self._measurement(name) for name in names]
        for measurement in measurements:
            if measurement.started_monotonic is None:
                raise ValueError(f"measurement {measurement.name!r} has not started")
            if measurement.value is not None:
                raise ValueError(f"measurement {measurement.name!r} has already finished")
        monotonic, wall = self._capture()
        if event:
            self._add_event(event, monotonic, wall)
        values: dict[str, float] = {}
        for measurement in measurements:
            measurement.value = max(0.0, monotonic - measurement.started_monotonic)
            measurement.end_event = event
            values[measurement.name] = measurement.value
        return values

    def mark_event(self, name: str) -> None:
        monotonic, wall = self._capture()
        self._add_event(name, monotonic, wall)

    def event_time(self, name: str) -> datetime:
        try:
            return self._event_wall_times[name]
        except KeyError as exc:
            raise ValueError(f"event {name!r} is not recorded") from exc

    def update_environment(self, **values: Any) -> None:
        self.environment.update(values)

    def finish_test(self) -> None:
        measurement = self._measurement(TEST_TOTAL)
        if measurement.value is not None:
            return
        if measurement.started_monotonic is None:
            self._finished_at = self._finished_at or self._clock.now()
            return
        monotonic, wall = self._capture()
        self._add_event("test.finished", monotonic, wall)
        measurement.value = max(0.0, monotonic - measurement.started_monotonic)
        measurement.end_event = "test.finished"
        self._finished_at = wall

    def finalize(
        self,
        outcome: str,
        *,
        error: Mapping[str, str] | None = None,
    ) -> Path:
        if self._result_path is not None:
            return self._result_path
        if outcome not in VALID_OUTCOMES:
            raise ValueError(f"unknown benchmark outcome {outcome!r}")
        self.finish_test()
        self._outcome = outcome
        self._error = _bounded_error(error)
        self._finished_at = self._finished_at or self._clock.now()
        self._result_path = self._write()
        print(self.summary(), flush=True)
        return self._result_path

    def as_dict(self) -> dict[str, Any]:
        if self._outcome is None or self._finished_at is None:
            raise RuntimeError("benchmark must be finalized before serialization")
        return {
            "schemaVersion": SCHEMA_VERSION,
            "benchmarkVersion": BENCHMARK_VERSION,
            "identity": {
                "suite": self.suite,
                "case": self.case,
                "test": self.test,
                "runId": self._run_id,
                "runAttempt": self._run_attempt,
            },
            "outcome": self._outcome,
            "startedAt": _timestamp(self._started_at),
            "finishedAt": _timestamp(self._finished_at),
            "source": source_from_environment(self._run_id),
            "environment": self.environment,
            "measurements": [
                self._measurement_dict(item) for item in self._measurements.values()
            ],
            "events": self._events,
            "error": self._error,
        }

    def summary(self) -> str:
        outcome = self._outcome or "not finalized"
        lines = [
            f"\n=== E2E benchmark: {self.suite} / {self.case} ===",
            f"Outcome: {outcome}",
        ]
        lines.extend(_gpu_summary_lines(self.environment))
        lines.append(_storage_summary(self.environment))
        ordered = [
            measurement
            for name, measurement in self._measurements.items()
            if name != TEST_TOTAL
        ]
        ordered.append(self._measurement(TEST_TOTAL))
        for measurement in ordered:
            value = (
                f"{measurement.value:.2f} {measurement.unit}"
                if measurement.value is not None
                else "not completed"
            )
            lines.append(f"{measurement.display_name}: {value}")
        if self._result_path is not None:
            lines.append(f"Result: {self._result_path}")
        return "\n".join(lines)

    def _capture_started(self) -> tuple[float, datetime]:
        return self._started_monotonic, self._started_at

    def _capture(self) -> tuple[float, datetime]:
        return self._clock.monotonic(), self._clock.now()

    def _add_event(self, name: str, monotonic: float, wall: datetime) -> None:
        if name in self._event_names:
            raise ValueError(f"event {name!r} is already recorded")
        self._event_names.add(name)
        self._event_wall_times[name] = wall
        self._events.append(
            {
                "name": name,
                "offsetSeconds": round(max(0.0, monotonic - self._started_monotonic), 6),
                "timestamp": _timestamp(wall),
            }
        )

    def _measurement(self, name: str) -> _Measurement:
        try:
            return self._measurements[name]
        except KeyError as exc:
            raise ValueError(f"measurement {name!r} is not defined") from exc

    @staticmethod
    def _measurement_dict(measurement: _Measurement) -> dict[str, Any]:
        completed = measurement.value is not None
        result: dict[str, Any] = {
            "name": measurement.name,
            "displayName": measurement.display_name,
            "unit": measurement.unit,
            "value": round(measurement.value, 6) if completed else None,
            "status": "complete" if completed else "incomplete",
        }
        if measurement.start_event:
            result["startEvent"] = measurement.start_event
        if measurement.end_event:
            result["endEvent"] = measurement.end_event
        if not completed:
            result["missingReason"] = (
                "end event not reached"
                if measurement.started_monotonic is not None
                else "start event not reached"
            )
        return result

    def _write(self) -> Path:
        self._result_dir.mkdir(parents=True, exist_ok=True)
        filename = "-".join(
            _safe_filename(part)
            for part in (self.suite, self.case, self._run_id, str(self._run_attempt))
        )
        path = self._result_dir / f"{filename}.json"
        temporary = path.with_suffix(".json.tmp")
        temporary.write_text(json.dumps(self.as_dict(), indent=2) + "\n", encoding="utf-8")
        os.replace(temporary, path)
        return path


class BenchmarkSession:
    """Pytest-facing owner that finalizes the recorder during fixture teardown."""

    def __init__(self, test: str, *, result_dir: Path | None = None) -> None:
        self.test = test
        self.result_dir = result_dir
        self.recorder: BenchmarkRecorder | None = None

    def start(
        self,
        *,
        suite: str,
        case: str,
        environment: Mapping[str, Any] | None = None,
    ) -> BenchmarkRecorder:
        if self.recorder is not None:
            raise RuntimeError("a benchmark session can record only one result")
        self.recorder = BenchmarkRecorder(
            suite=suite,
            case=case,
            test=self.test,
            environment=environment,
            result_dir=self.result_dir,
        )
        return self.recorder

    def finalize(
        self,
        outcome: str,
        *,
        error: Mapping[str, str] | None = None,
    ) -> Path | None:
        if self.recorder is None:
            return None
        return self.recorder.finalize(outcome, error=error)


def result_directory() -> Path:
    configured = os.environ.get("SNAPSHOT_E2E_BENCHMARK_DIR")
    if configured:
        return Path(configured)
    return Path(tempfile.gettempdir()) / "snapshot-e2e-benchmarks"


def source_from_environment(run_id: str) -> dict[str, Any]:
    server = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    run_url = f"{server}/{repository}/actions/runs/{run_id}" if repository else None
    return {
        "commit": os.environ.get("GITHUB_SHA"),
        "ref": os.environ.get("GITHUB_REF"),
        "event": os.environ.get("GITHUB_EVENT_NAME", "local"),
        "runUrl": run_url,
        "snapshotTag": os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG"),
    }


def write_fallback(
    *,
    suite: str,
    case: str,
    test: str,
    outcome: str,
    message: str,
    result_dir: Path | None = None,
) -> Path:
    directory = result_dir or result_directory()
    prefix = "-".join(_safe_filename(part) for part in (suite, case))
    existing = (
        sorted(directory.glob(f"{prefix}-*.json")) if directory.exists() else []
    )
    if existing:
        print(f"Benchmark result already exists: {existing[0]}")
        return existing[0]
    recorder = BenchmarkRecorder(
        suite=suite,
        case=case,
        test=test,
        environment={
            "sourceGpus": [],
            "restoreGpus": [],
            "sourceGpuCollectionError": "test did not reach source GPU discovery",
            "restoreGpuCollectionError": "test did not reach restore GPU discovery",
            "storageCollectionError": "test did not reach storage discovery",
        },
        result_dir=directory,
        start_test=False,
    )
    return recorder.finalize(
        outcome,
        error={"phase": "pytest", "message": message},
    )


def _gpu_summary_lines(environment: Mapping[str, Any]) -> list[str]:
    return [
        _gpu_summary(environment, role="source"),
        _gpu_summary(environment, role="restore"),
    ]


def _gpu_summary(environment: Mapping[str, Any], *, role: str) -> str:
    title = role.capitalize()
    gpus = environment.get(f"{role}Gpus")
    if isinstance(gpus, list) and gpus:
        rendered = []
        for gpu in gpus:
            if not isinstance(gpu, Mapping):
                continue
            rendered.append(
                f"{gpu.get('model', 'unknown')} "
                f"({gpu.get('uuid', 'unknown')}, driver {gpu.get('driverVersion', 'unknown')})"
            )
        if rendered:
            node = environment.get(f"{role}Node", "unknown node")
            return f"{title} GPU: {'; '.join(rendered)}, node {node}"
    reason = environment.get(f"{role}GpuCollectionError")
    unknown = f"unknown ({reason})" if reason else "unknown"
    return f"{title} GPU: {unknown}"


def _storage_summary(environment: Mapping[str, Any]) -> str:
    storage = environment.get("storage")
    if isinstance(storage, Mapping):
        storage_type = storage.get("type", "unknown type")
        provisioner = storage.get("provisioner", "unknown type")
        storage_class = storage.get("storageClass", "unknown class")
        requested = storage.get("requestedSize", "unknown")
        capacity = storage.get("capacity", "unknown")
        return (
            f"Storage: {storage_type} ({provisioner}), class {storage_class}, "
            f"requested {requested}, capacity {capacity}"
        )
    reason = environment.get("storageCollectionError")
    configured = environment.get("storageClass")
    suffix = f" ({reason})" if reason else ""
    return f"Storage: {configured or 'unknown'}{suffix}"


_GO_DURATION_PART = re.compile(
    r"(?P<value>\d+(?:\.\d+)?)(?P<unit>ns|us|[µμ]s|ms|s|m|h)"
)
_GO_DURATION_MULTIPLIERS = {
    "ns": 1e-9,
    "us": 1e-6,
    "µs": 1e-6,
    "μs": 1e-6,
    "ms": 1e-3,
    "s": 1.0,
    "m": 60.0,
    "h": 3600.0,
}
_RFC3339 = re.compile(
    r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})"
)


def parse_go_duration(value: str) -> float:
    """Converts a non-negative Go duration string to seconds."""
    matches = list(_GO_DURATION_PART.finditer(value))
    if not matches or "".join(match.group(0) for match in matches) != value:
        raise ValueError(f"invalid Go duration {value!r}")
    return sum(
        float(match.group("value")) * _GO_DURATION_MULTIPLIERS[match.group("unit")]
        for match in matches
    )


def parse_agent_timing_summary(
    logs: str,
    *,
    message: str,
    field: str,
    matches: Mapping[str, str],
) -> AgentTimingSummary:
    """Finds the newest matching structured timing summary in agent logs."""
    for line in reversed(logs.splitlines()):
        marker = line.find(message)
        if marker < 0:
            continue
        timestamp_match = _RFC3339.search(line[:marker])
        if timestamp_match is None:
            continue
        payload_text = line[marker + len(message) :].strip()
        try:
            payload = json.loads(payload_text)
        except json.JSONDecodeError:
            continue
        if not isinstance(payload, dict) or any(
            str(payload.get(key)) != expected for key, expected in matches.items()
        ):
            continue
        summary = payload.get(field)
        if not isinstance(summary, dict):
            raise ValueError(f"agent {message!r} payload has no {field!r} object")
        phases = summary.get("phases")
        if not isinstance(phases, dict):
            raise ValueError(f"agent {message!r} payload has no phases object")
        started_to_complete = summary.get("started_to_complete")
        return AgentTimingSummary(
            completed_at=_parse_rfc3339(timestamp_match.group(0)),
            duration_seconds=parse_go_duration(str(summary["duration"])),
            phases_seconds={
                str(name): parse_go_duration(str(duration))
                for name, duration in phases.items()
            },
            started_to_complete_seconds=(
                parse_go_duration(str(started_to_complete))
                if started_to_complete is not None
                else None
            ),
        )
    match_text = ", ".join(f"{key}={value!r}" for key, value in matches.items())
    raise ValueError(f"no {message!r} agent timing summary matched {match_text}")


_IMAGE_PULLED = re.compile(
    r'Successfully pulled image ".+" in (?P<duration>\S+) '
    r'\((?P<including_wait>\S+) including waiting\)\. '
    r'Image size: (?P<size>\d+) bytes\.'
)


def parse_image_pull_events(
    events: Sequence[object],
    *,
    pod_uid: str,
) -> ImagePullSummary:
    """Extracts the framework image pull duration for one Pod UID.

    Framework pods use the same image for their init and main containers. If
    both emit events, the longest real pull represents the cache population;
    subsequent "already present" events are cache hits, not additional pulls.
    """
    pulls: list[ImagePullSummary] = []
    cached = False
    for event in events:
        involved = getattr(event, "involved_object", None)
        if (
            getattr(event, "reason", None) != "Pulled"
            or str(getattr(involved, "uid", "") or "") != pod_uid
        ):
            continue
        message = str(getattr(event, "message", "") or "")
        match = _IMAGE_PULLED.search(message)
        if match:
            pulls.append(
                ImagePullSummary(
                    duration_seconds=parse_go_duration(match.group("duration")),
                    including_wait_seconds=parse_go_duration(
                        match.group("including_wait")
                    ),
                    cache_hit=False,
                    image_size_bytes=int(match.group("size")),
                )
            )
        elif "already present on machine" in message:
            cached = True
    if pulls:
        return max(pulls, key=lambda item: item.including_wait_seconds)
    if cached:
        return ImagePullSummary(
            duration_seconds=0.0,
            including_wait_seconds=0.0,
            cache_hit=True,
            image_size_bytes=None,
        )
    raise ValueError(f"no Pulled event found for pod UID {pod_uid!r}")


def _timestamp(value: datetime) -> str:
    value = _utc_datetime(value)
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _utc_datetime(value: datetime) -> datetime:
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc)


def _parse_rfc3339(value: str) -> datetime:
    # datetime accepts RFC3339 offsets but only retains microseconds; trim the
    # agent's nanosecond precision explicitly instead of relying on runtime
    # version-specific parsing behavior.
    normalized = value.replace("Z", "+00:00")
    match = re.match(r"^(.*\.)(\d+)([+-]\d{2}:\d{2})$", normalized)
    if match:
        normalized = f"{match.group(1)}{match.group(2)[:6]}{match.group(3)}"
    return _utc_datetime(datetime.fromisoformat(normalized))


def _safe_filename(value: str) -> str:
    safe = re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-.")
    return safe or "unknown"


def _integer_environment(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc


def _bounded_error(error: Mapping[str, str] | None) -> dict[str, str] | None:
    if error is None:
        return None
    return {
        "phase": str(error.get("phase", "unknown")),
        "message": str(error.get("message", ""))[-MAX_ERROR_MESSAGE_LENGTH:],
    }


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    fallback = subparsers.add_parser("fallback", help="write a result if pytest wrote none")
    fallback.add_argument("--suite", required=True)
    fallback.add_argument("--case", required=True)
    fallback.add_argument("--test", required=True)
    fallback.add_argument("--outcome", choices=sorted(VALID_OUTCOMES), required=True)
    fallback.add_argument("--message", required=True)
    args = parser.parse_args(argv)
    if args.command == "fallback":
        write_fallback(
            suite=args.suite,
            case=args.case,
            test=args.test,
            outcome=args.outcome,
            message=args.message,
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
