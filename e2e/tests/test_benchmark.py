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
            "sourceNode": "gpu-node-source",
            "restoreNode": "gpu-node-restore",
            "sourceGpus": [
                {
                    "model": "NVIDIA B200",
                    "uuid": "GPU-123",
                    "driverVersion": "580.1",
                }
            ],
            "restoreGpus": [
                {
                    "model": "NVIDIA B200",
                    "uuid": "GPU-456",
                    "driverVersion": "580.1",
                }
            ],
            "storage": {
                "storageClass": "azurefile-csi-premium",
                "type": "Premium_LRS",
                "provisioner": "file.csi.azure.com",
                "requestedSize": "1Ti",
                "capacity": "1Ti",
            },
        },
        result_dir=tmp_path,
        clock=clock,
        run_id="12345",
        run_attempt=2,
    )
    recorder.define_duration("checkpoint.duration", "Checkpoint")
    recorder.define_measurement("checkpoint.size", "Checkpoint size", "bytes")

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
    assert result["environment"]["sourceGpus"][0]["uuid"] == "GPU-123"
    assert result["environment"]["restoreGpus"][0]["uuid"] == "GPU-456"
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
    summary = recorder.summary()
    assert "Source GPU: NVIDIA B200 (GPU-123, driver 580.1), node gpu-node-source" in summary
    assert "Restore GPU: NVIDIA B200 (GPU-456, driver 580.1), node gpu-node-restore" in summary
    assert (
        "Storage: Premium_LRS (file.csi.azure.com), class azurefile-csi-premium, "
        "requested 1Ti, capacity 1Ti"
    ) in summary
    assert "Full E2E test: 18.75 seconds" in summary


def test_external_event_timestamp_removes_observation_delay(tmp_path) -> None:
    clock = FakeClock()
    recorder = benchmark.BenchmarkRecorder(
        suite="suite",
        case="case",
        test="test",
        result_dir=tmp_path,
        clock=clock,
    )
    recorder.define_duration("restore.duration", "Restore")

    clock.advance(10)
    emitted_at = clock.now()
    clock.advance(2)
    recorder.start_duration_at(
        "restore.duration",
        emitted_at,
        event="restore.requested",
    )
    clock.advance(8)

    assert recorder.finish_duration("restore.duration", event="traffic.ready") == 10
    assert recorder.event_time("restore.requested") == emitted_at
    recorder.finish_test()
    result = json.loads(recorder.finalize("passed").read_text(encoding="utf-8"))
    requested = next(e for e in result["events"] if e["name"] == "restore.requested")
    assert requested["externalTimestamp"] is True
    assert requested["observationDelaySeconds"] == 2.0
    assert "clamped" not in requested
    assert recorder.timing_warnings() == []


def test_external_event_from_a_clock_ahead_of_the_runner_is_flagged(tmp_path) -> None:
    clock = FakeClock()
    recorder = benchmark.BenchmarkRecorder(
        suite="suite",
        case="case",
        test="test",
        result_dir=tmp_path,
        clock=clock,
    )
    recorder.define_duration("restore.duration", "Restore")
    clock.advance(10)
    ahead = clock.now() + timedelta(seconds=3)

    recorder.start_duration_at("restore.duration", ahead, event="restore.requested")
    clock.advance(8)

    assert recorder.finish_duration("restore.duration") == 8
    requested = next(e for e in recorder.as_dict_events() if e["name"] == "restore.requested")
    assert requested["observationDelaySeconds"] == -3.0
    assert requested["clamped"] is True
    warnings = recorder.timing_warnings()
    assert len(warnings) == 1
    assert "cluster clock is ahead" in warnings[0]
    assert warnings[0] in recorder.summary()


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


@pytest.mark.parametrize(
    ("value", "seconds"),
    [
        ("0s", 0.0),
        ("605.387111ms", 0.605387111),
        ("1m10.345884064s", 70.345884064),
        ("2h3m4.5s", 7384.5),
    ],
)
def test_parse_go_duration(value: str, seconds: float) -> None:
    assert benchmark.parse_go_duration(value) == pytest.approx(seconds)


@pytest.mark.parametrize("value", ["", "10", "-1s", "1 second", "1m garbage"])
def test_parse_go_duration_rejects_invalid_values(value: str) -> None:
    with pytest.raises(ValueError):
        benchmark.parse_go_duration(value)


def test_parse_agent_timing_summary_matches_identity_and_nanosecond_timestamp() -> None:
    logs = "\n".join(
        [
            '2026-09-09T07:46:35.000000000Z\tINFO\tCheckpoint timing summary\t'
            '{"content":"other","checkpoint":{"duration":"1s","phases":{}}}',
            '[pod/snapshot-agent/agent] 2026-09-09T07:48:42.938213483Z\tINFO\tcontroller\t'
            'Checkpoint timing summary\t{"content":"wanted","checkpoint":'
            '{"duration":"1m10.345884064s","phases":{"criu_dump":'
            '"1m4.861411573s","overlay_capture":"220.658592ms"},'
            '"started_to_complete":"1m10.345894804s"}}',
        ]
    )

    summary = benchmark.parse_agent_timing_summary(
        logs,
        message="Checkpoint timing summary",
        field="checkpoint",
        matches={"content": "wanted"},
    )

    assert summary.completed_at == datetime(
        2026, 9, 9, 7, 48, 42, 938213, tzinfo=timezone.utc
    )
    assert summary.duration_seconds == pytest.approx(70.345884064)
    assert summary.phases_seconds["criu_dump"] == pytest.approx(64.861411573)
    assert summary.phases_seconds["overlay_capture"] == pytest.approx(0.220658592)
    assert summary.started_to_complete_seconds == pytest.approx(70.345894804)


def test_parse_image_pull_events_reports_pull_and_cache_hit() -> None:
    actual = SimpleNamespace(
        reason="Pulled",
        involved_object=SimpleNamespace(uid="wanted"),
        message=(
            'Successfully pulled image "registry/framework@sha256:123" in 1m2.5s '
            '(1m3s including waiting). Image size: 5378192452 bytes.'
        ),
    )
    cached = SimpleNamespace(
        reason="Pulled",
        involved_object=SimpleNamespace(uid="cached"),
        message='Container image "registry/framework@sha256:123" already present on machine',
    )

    pulled = benchmark.parse_image_pull_events([cached, actual], pod_uid="wanted")
    cache_hit = benchmark.parse_image_pull_events([actual, cached], pod_uid="cached")

    assert pulled.duration_seconds == 62.5
    assert pulled.including_wait_seconds == 63
    assert pulled.image_size_bytes == 5_378_192_452
    assert pulled.cache_hit is False
    assert cache_hit.duration_seconds == 0
    assert cache_hit.image_size_bytes is None
    assert cache_hit.cache_hit is True


@pytest.mark.parametrize(
    ("message", "including_wait", "size"),
    [
        ('Successfully pulled image "registry/framework:tag" in 1m2.5s', 62.5, None),
        (
            'Successfully pulled image "registry/framework:tag" in 1m2.5s '
            "(1m3s including waiting)",
            63.0,
            None,
        ),
        (
            'Successfully pulled image "registry/framework:tag" in 1m2.5s '
            "(1m3s including waiting). Image size: 42 bytes.",
            63.0,
            42,
        ),
    ],
)
def test_parse_image_pull_events_accepts_older_kubelet_messages(
    message: str, including_wait: float, size: int | None
) -> None:
    event = SimpleNamespace(
        reason="Pulled",
        involved_object=SimpleNamespace(uid="pod"),
        message=message,
    )

    pulled = benchmark.parse_image_pull_events([event], pod_uid="pod")

    assert pulled.duration_seconds == 62.5
    assert pulled.including_wait_seconds == including_wait
    assert pulled.image_size_bytes == size
    assert pulled.cache_hit is False


def test_public_storage_parameters_excludes_cluster_identifiers() -> None:
    parameters = benchmark.public_storage_parameters(
        {
            "skuName": "Premium_LRS",
            "type": "azurefile",
            "storageType": "ssd",
            "secretName": "storage-credentials",
            "secretNamespace": "kube-system",
            "resourceGroup": "internal-infrastructure",
            "storageAccount": "private-account",
        }
    )

    assert parameters == {
        "skuName": "Premium_LRS",
        "type": "azurefile",
        "storageType": "ssd",
    }


def test_fallback_is_idempotent(tmp_path) -> None:
    first = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="infrastructure_failed",
        message="pytest did not start",
        result_dir=tmp_path,
        run_id="run-1",
        run_attempt=1,
    )
    second = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="timed_out",
        message="this must not replace the first result",
        result_dir=tmp_path,
        run_id="run-1",
        run_attempt=1,
    )

    assert second == first
    assert len(list(tmp_path.glob("*.json"))) == 1
    result = json.loads(first.read_text(encoding="utf-8"))
    assert result["outcome"] == "infrastructure_failed"
    assert _measurement(result, benchmark.TEST_TOTAL)["status"] == "incomplete"
    assert _measurement(result, benchmark.TEST_TOTAL)["value"] is None


def test_fallback_does_not_reuse_another_case_result(tmp_path) -> None:
    existing = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="vllm",
        test="test_framework",
        outcome="infrastructure_failed",
        message="vllm did not start",
        result_dir=tmp_path,
        run_id="run-1",
        run_attempt=1,
    )

    result = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="timed_out",
        message="sglang timed out",
        result_dir=tmp_path,
        run_id="run-1",
        run_attempt=1,
    )

    assert result != existing
    assert len(list(tmp_path.glob("*.json"))) == 2
    written = json.loads(result.read_text(encoding="utf-8"))
    assert written["identity"]["case"] == "sglang"


def test_fallback_does_not_reuse_a_stale_result_from_another_run(
    tmp_path, monkeypatch: pytest.MonkeyPatch
) -> None:
    stale = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="infrastructure_failed",
        message="left behind on a reused runner workspace",
        result_dir=tmp_path,
        run_id="run-1",
        run_attempt=1,
    )
    monkeypatch.setenv("GITHUB_RUN_ID", "run-2")
    monkeypatch.setenv("GITHUB_RUN_ATTEMPT", "3")

    current = benchmark.write_fallback(
        suite="framework-checkpoint-restore",
        case="sglang",
        test="test_framework",
        outcome="timed_out",
        message="current run timed out",
        result_dir=tmp_path,
    )

    assert current != stale
    assert current.name == "framework-checkpoint-restore-sglang-run-2-3.json"
    written = json.loads(current.read_text(encoding="utf-8"))
    assert written["identity"] == {
        "suite": "framework-checkpoint-restore",
        "case": "sglang",
        "test": "test_framework",
        "runId": "run-2",
        "runAttempt": 3,
    }
    assert written["outcome"] == "timed_out"


def test_wait_for_pod_event_matches_uid(monkeypatch: pytest.MonkeyPatch) -> None:
    wrong = SimpleNamespace(
        reason="RestoreRequested",
        involved_object=SimpleNamespace(name="restore", uid="old-uid"),
    )
    expected = SimpleNamespace(
        reason="RestoreRequested",
        involved_object=SimpleNamespace(name="restore", uid="current-uid"),
        event_time=datetime(2026, 9, 9, 7, 0, tzinfo=timezone.utc),
    )
    selectors: list[dict[str, str] | None] = []

    def list_events(namespace: str, *, field_selector=None):
        selectors.append(field_selector)
        return [wrong, expected]

    monkeypatch.setattr(lifecycle.k8s, "list_events", list_events)

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
    assert selectors == [
        {
            "involvedObject.name": "restore",
            "involvedObject.uid": "current-uid",
            "reason": "RestoreRequested",
        }
    ]
    assert lifecycle.pod_event_timestamp(expected) == expected.event_time


def test_wait_for_raises_a_lifecycle_timeout() -> None:
    with pytest.raises(lifecycle.LifecycleTimeoutError, match="timed out waiting for never"):
        lifecycle.wait_for("never", lambda: None, 0)


def _poll_without_sleeping(description, fn, timeout, **kwargs):
    while True:
        result = fn()
        if result is not None:
            return result


def test_combined_restore_wait_records_each_boundary_once(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    pending = SimpleNamespace(
        status=SimpleNamespace(phase="Running", conditions=[]),
    )
    succeeded_condition = SimpleNamespace(
        type="nvidia.com/Restored",
        status="True",
        reason="RestoreSucceeded",
        message="restored",
    )
    succeeded = SimpleNamespace(
        status=SimpleNamespace(phase="Running", conditions=[succeeded_condition]),
    )
    pods = iter([pending, pending, succeeded, succeeded])
    outputs = iter(["", "__snapshot_e2e_outcome__:ready\nfirst generation"])
    observed: list[str] = []
    exec_calls: list[str] = []

    def exec_command(namespace: str, name: str, command: str) -> str:
        exec_calls.append(observed[-1] if observed else "before-restore")
        return next(outputs)

    monkeypatch.setattr(lifecycle.k8s, "read_pod", lambda namespace, name: next(pods))
    monkeypatch.setattr(lifecycle.k8s, "exec_command", exec_command)
    monkeypatch.setattr(lifecycle, "wait_for", _poll_without_sleeping)

    pod, text = lifecycle.wait_for_restore_traffic_ready(
        "snapshot-e2e",
        "restore",
        ready_file="/ready",
        error_file="/error",
        timeout=1,
        on_restore_succeeded=lambda: observed.append("restore"),
        on_traffic_ready=lambda: observed.append("traffic"),
    )

    assert pod is succeeded
    assert text == "first generation"
    assert observed == ["restore", "traffic"]
    assert exec_calls == ["restore", "restore"]


def test_combined_restore_wait_fails_when_restored_pod_terminates(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    restored = SimpleNamespace(
        type="nvidia.com/Restored",
        status="True",
        reason="RestoreSucceeded",
        message="restored",
    )
    terminated = SimpleNamespace(
        status=SimpleNamespace(phase="Failed", conditions=[restored]),
    )
    monkeypatch.setattr(lifecycle.k8s, "read_pod", lambda namespace, name: terminated)
    monkeypatch.setattr(
        lifecycle.k8s,
        "exec_command",
        lambda namespace, name, command: pytest.fail(
            "must not exec into a terminal pod"
        ),
    )
    monkeypatch.setattr(
        lifecycle,
        "wait_for",
        lambda description, fn, timeout, **kwargs: fn(),
    )

    with pytest.raises(AssertionError, match="reached phase Failed"):
        lifecycle.wait_for_restore_traffic_ready(
            "snapshot-e2e",
            "restore",
            ready_file="/ready",
            error_file="/error",
            timeout=1,
        )


def _measurement(result: dict, name: str) -> dict:
    return next(item for item in result["measurements"] if item["name"] == name)
