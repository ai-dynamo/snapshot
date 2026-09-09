# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from types import SimpleNamespace

import pytest

from snapshot_e2e import benchmark
from snapshot_e2e import lifecycle


@dataclass
class FakeClock:
    elapsed: float = 0.0

    def monotonic(self) -> float:
        return 100.0 + self.elapsed

    def now(self) -> datetime:
        return datetime(2026, 9, 9, tzinfo=timezone.utc) + timedelta(seconds=self.elapsed)

    def advance(self, seconds: float) -> None:
        self.elapsed += seconds


def test_recorder_writes_events_measurements_and_environment(tmp_path) -> None:
    clock = FakeClock()
    recorder = benchmark.BenchmarkRecorder(
        suite="framework-checkpoint-restore",
        case="vllm",
        test="test_framework",
        environment={
            "node": "gpu-node",
            "gpus": [
                {
                    "model": "NVIDIA B200",
                    "uuid": "GPU-123",
                    "driverVersion": "580.1",
                }
            ],
        },
        result_dir=tmp_path,
        clock=clock,
        run_id="12345",
        run_attempt=2,
    )
    recorder.define_duration("checkpoint.duration", "Checkpoint")

    clock.advance(2.5)
    recorder.mark_event("source.ready")
    recorder.start_duration("checkpoint.duration", event="checkpoint.requested")
    clock.advance(12.25)
    recorder.finish_duration("checkpoint.duration", event="checkpoint.ready")
    recorder.record_measurement("checkpoint.size", "Checkpoint size", "bytes", 4096)
    clock.advance(4.0)
    recorder.finish_test()

    path = recorder.finalize("passed")
    result = json.loads(path.read_text(encoding="utf-8"))

    assert path.name == "framework-checkpoint-restore-vllm-12345-2.json"
    assert result["schemaVersion"] == 1
    assert result["benchmarkVersion"] == 1
    assert result["identity"] == {
        "suite": "framework-checkpoint-restore",
        "case": "vllm",
        "test": "test_framework",
        "runId": "12345",
        "runAttempt": 2,
    }
    assert result["outcome"] == "passed"
    assert result["finishedAt"] == "2026-09-09T00:00:18.750Z"
    assert result["environment"]["gpus"][0]["uuid"] == "GPU-123"
    assert _measurement(result, "checkpoint.duration") == {
        "name": "checkpoint.duration",
        "displayName": "Checkpoint",
        "unit": "seconds",
        "value": 12.25,
        "status": "complete",
        "startEvent": "checkpoint.requested",
        "endEvent": "checkpoint.ready",
    }
    assert _measurement(result, benchmark.TEST_TOTAL)["value"] == 18.75
    assert _measurement(result, "checkpoint.size") == {
        "name": "checkpoint.size",
        "displayName": "Checkpoint size",
        "unit": "bytes",
        "value": 4096.0,
        "status": "complete",
    }
    assert [event["name"] for event in result["events"]] == [
        "test.started",
        "source.ready",
        "checkpoint.requested",
        "checkpoint.ready",
        "test.finished",
    ]


def test_failure_keeps_incomplete_measurements_and_bounds_error(tmp_path) -> None:
    clock = FakeClock()
    recorder = benchmark.BenchmarkRecorder(
        suite="suite",
        case="case",
        test="test",
        result_dir=tmp_path,
        clock=clock,
        run_id="local-test",
    )
    recorder.define_duration("never.started", "Never started")
    recorder.define_duration("never.finished", "Never finished")
    recorder.start_duration("never.finished", event="work.started")
    clock.advance(3)
    recorder.finish_test()

    result = json.loads(
        recorder.finalize(
            "failed",
            error={"phase": "call", "message": "x" * 5000},
        ).read_text(encoding="utf-8")
    )

    never_started = _measurement(result, "never.started")
    assert never_started["value"] is None
    assert never_started["status"] == "incomplete"
    assert never_started["missingReason"] == "start event not reached"
    never_finished = _measurement(result, "never.finished")
    assert never_finished["value"] is None
    assert never_finished["missingReason"] == "end event not reached"
    assert len(result["error"]["message"]) == benchmark.MAX_ERROR_MESSAGE_LENGTH


def test_parse_nvidia_smi_csv() -> None:
    assert benchmark.parse_nvidia_smi_csv(
        "NVIDIA B200, GPU-one, 580.1\nNVIDIA H100 NVL, GPU-two, 580.1\n"
    ) == [
        {"model": "NVIDIA B200", "uuid": "GPU-one", "driverVersion": "580.1"},
        {"model": "NVIDIA H100 NVL", "uuid": "GPU-two", "driverVersion": "580.1"},
    ]


@pytest.mark.parametrize("output", ["", "NVIDIA B200, GPU-one", "NVIDIA B200,,580.1"])
def test_parse_nvidia_smi_csv_rejects_incomplete_output(output: str) -> None:
    with pytest.raises(ValueError):
        benchmark.parse_nvidia_smi_csv(output)


def test_fallback_is_idempotent(tmp_path) -> None:
    first = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="infrastructure_failed",
        message="pytest did not start",
        result_dir=tmp_path,
    )
    second = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="timed_out",
        message="this must not replace the first result",
        result_dir=tmp_path,
    )

    assert second == first
    assert len(list(tmp_path.glob("*.json"))) == 1
    result = json.loads(first.read_text(encoding="utf-8"))
    assert result["outcome"] == "infrastructure_failed"
    assert _measurement(result, benchmark.TEST_TOTAL)["status"] == "incomplete"
    assert _measurement(result, benchmark.TEST_TOTAL)["value"] is None


def test_wait_for_pod_event_matches_uid(monkeypatch: pytest.MonkeyPatch) -> None:
    wrong = SimpleNamespace(
        reason="RestoreRequested",
        involved_object=SimpleNamespace(name="restore", uid="old-uid"),
    )
    expected = SimpleNamespace(
        reason="RestoreRequested",
        involved_object=SimpleNamespace(name="restore", uid="current-uid"),
    )
    monkeypatch.setattr(lifecycle.k8s, "list_events", lambda namespace: [wrong, expected])

    assert (
        lifecycle.wait_for_pod_event(
            "snapshot-e2e",
            "restore",
            "RestoreRequested",
            pod_uid="current-uid",
            timeout=1,
        )
        is expected
    )


def _measurement(result: dict, name: str) -> dict:
    return next(item for item in result["measurements"] if item["name"] == name)
