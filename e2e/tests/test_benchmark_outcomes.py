# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Pytest-level behavior of the ``benchmark`` fixture and the result schema.

The recorder tests exercise ``snapshot_e2e.benchmark`` directly. These run a
small inner pytest session through ``pytester`` so the outcome mapping in
``conftest.py`` is covered for every outcome, and pin the full result document
so schema changes are deliberate.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from snapshot_e2e import benchmark

CONFTEST = Path(__file__).with_name("conftest.py")
GOLDEN = Path(__file__).with_name("data") / "benchmark-result-v1.json"
GITHUB_VARIABLES = (
    "GITHUB_RUN_ID",
    "GITHUB_RUN_ATTEMPT",
    "GITHUB_SERVER_URL",
    "GITHUB_REPOSITORY",
    "GITHUB_SHA",
    "GITHUB_REF",
    "GITHUB_EVENT_NAME",
    "SNAPSHOT_E2E_SNAPSHOT_TAG",
)

INNER_TESTS = '''
import pytest

from snapshot_e2e import lifecycle


def _fail_with_traceback() -> None:
    raise RuntimeError("boom")


@pytest.fixture
def broken():
    raise RuntimeError("fixture setup failed")


def test_passes(benchmark):
    result = benchmark.start(suite="suite", case="case")
    result.define_duration("work.duration", "Work")
    result.start_duration("work.duration", event="work.started")
    result.finish_duration("work.duration", event="work.finished")


def test_fails(benchmark):
    result = benchmark.start(suite="suite", case="case")
    result.define_duration("work.duration", "Work")
    result.start_duration("work.duration", event="work.started")
    _fail_with_traceback()


def test_skips(benchmark):
    benchmark.start(suite="suite", case="case")
    pytest.skip("case not selected")


def test_times_out(benchmark):
    benchmark.start(suite="suite", case="case")
    lifecycle.wait_for("the pod", lambda: None, 0)


def test_setup_fails(benchmark, broken):
    benchmark.start(suite="suite", case="case")


def test_interrupted(benchmark):
    result = benchmark.start(suite="suite", case="case")
    result.define_duration("work.duration", "Work")
    result.start_duration("work.duration", event="work.started")
    raise KeyboardInterrupt
'''


@dataclass
class FakeClock:
    elapsed: float = 0.0

    def monotonic(self) -> float:
        return 1000.0 + self.elapsed

    def now(self) -> datetime:
        return datetime(2026, 9, 10, 9, 0, tzinfo=timezone.utc) + timedelta(
            seconds=self.elapsed
        )

    def advance(self, seconds: float) -> None:
        self.elapsed += seconds


def _run_inner_test(
    pytester: pytest.Pytester,
    monkeypatch: pytest.MonkeyPatch,
    name: str,
    *,
    subprocess: bool = False,
) -> tuple[pytest.RunResult, list[Path]]:
    results = pytester.path / "results"
    monkeypatch.setenv("SNAPSHOT_E2E_BENCHMARK_DIR", str(results))
    monkeypatch.setenv("GITHUB_RUN_ID", "run-1")
    monkeypatch.setenv("GITHUB_RUN_ATTEMPT", "1")
    pytester.makeconftest(CONFTEST.read_text(encoding="utf-8"))
    pytester.makepyfile(test_inner=INNER_TESTS)
    runner = pytester.runpytest_subprocess if subprocess else pytester.runpytest_inprocess
    run = runner("-p", "no:cacheprovider", "-k", name)
    return run, sorted(results.glob("*.json")) if results.exists() else []


def _load(paths: list[Path]) -> dict:
    assert len(paths) == 1, paths
    return json.loads(paths[0].read_text(encoding="utf-8"))


def test_passed_test_is_recorded_as_passed(pytester, monkeypatch) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_passes")

    run.assert_outcomes(passed=1)
    result = _load(paths)
    assert result["outcome"] == "passed"
    assert result["error"] is None
    assert result["identity"]["test"] == "test_passes"
    work = next(m for m in result["measurements"] if m["name"] == "work.duration")
    assert work["status"] == "complete"


def test_assertion_failure_keeps_measurements_and_only_the_crash_line(
    pytester, monkeypatch
) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_fails")

    run.assert_outcomes(failed=1)
    result = _load(paths)
    assert result["outcome"] == "failed"
    assert result["error"] == {"phase": "call", "message": "RuntimeError: boom"}
    assert str(pytester.path) not in json.dumps(result)
    work = next(m for m in result["measurements"] if m["name"] == "work.duration")
    assert work["status"] == "incomplete"
    assert [event["name"] for event in result["events"]] == [
        "test.started",
        "work.started",
        "test.finished",
    ]


def test_skipped_test_is_recorded_as_skipped(pytester, monkeypatch) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_skips")

    run.assert_outcomes(skipped=1)
    result = _load(paths)
    assert result["outcome"] == "skipped"
    assert result["error"] == {"phase": "call", "message": "Skipped: case not selected"}


def test_lifecycle_timeout_is_recorded_as_timed_out(pytester, monkeypatch) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_times_out")

    run.assert_outcomes(failed=1)
    result = _load(paths)
    assert result["outcome"] == "timed_out"
    assert result["error"]["phase"] == "call"
    assert result["error"]["message"].startswith(
        "snapshot_e2e.lifecycle.LifecycleTimeoutError: timed out waiting for the pod"
    )


def test_setup_failure_writes_no_result_so_the_workflow_fallback_applies(
    pytester, monkeypatch
) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_setup_fails")

    run.assert_outcomes(errors=1)
    assert paths == []


def test_interrupted_session_is_recorded_as_timed_out(pytester, monkeypatch) -> None:
    run, paths = _run_inner_test(pytester, monkeypatch, "test_interrupted", subprocess=True)

    assert run.ret == pytest.ExitCode.INTERRUPTED
    result = _load(paths)
    assert result["outcome"] == "timed_out"
    assert result["error"] == {
        "phase": "call",
        "message": "pytest was interrupted before the test completed",
    }
    assert [event["name"] for event in result["events"]] == [
        "test.started",
        "work.started",
        "test.finished",
    ]


def test_result_document_matches_golden(tmp_path, monkeypatch) -> None:
    for name in GITHUB_VARIABLES:
        monkeypatch.delenv(name, raising=False)
    clock = FakeClock()
    recorder = benchmark.BenchmarkRecorder(
        suite="framework-checkpoint-restore",
        case="vllm",
        test="test_framework_checkpoint_restore_serves_inference",
        environment={
            "namespace": "snapshot-e2e",
            "model": "Qwen/Qwen3-0.6B",
            "frameworkImage": "registry.example/vllm:tag",
            "frameworkImageDigest": "registry.example/vllm@sha256:abc123",
            "sourceNode": "gpu-node-a",
            "sourceGpus": [
                {"model": "NVIDIA B200", "uuid": "GPU-1", "driverVersion": "580.1"}
            ],
            "restoreGpus": [],
            "restoreGpuCollectionError": "exec failed",
            "restoreNodeGpuProduct": "NVIDIA-B200",
        },
        result_dir=tmp_path,
        clock=clock,
        run_id="123456789",
        run_attempt=1,
    )
    recorder.define_duration("checkpoint.duration", "Checkpoint (API to Ready)")
    recorder.define_duration("restore.to_traffic.duration", "Restore requested to traffic ready")
    recorder.define_duration("restore.criu_restore.duration", "Restore CRIU restore")
    recorder.define_measurement("checkpoint.size", "Checkpoint size", "bytes")

    clock.advance(30)
    recorder.mark_event("source.ready")
    recorder.start_duration("checkpoint.duration", event="checkpoint.requested")
    clock.advance(12.5)
    recorder.finish_duration("checkpoint.duration", event="checkpoint.ready")
    recorder.record_measurement("checkpoint.size", "Checkpoint size", "bytes", 4096)
    clock.advance(5)
    requested_at = clock.now()
    clock.advance(0.75)
    recorder.start_duration_at(
        "restore.to_traffic.duration", requested_at, event="restore.requested"
    )
    clock.advance(20)
    recorder.finish_duration("restore.to_traffic.duration", event="traffic.ready")
    clock.advance(1)
    recorder.finish_test()
    recorder.record_measurement(
        "restore.criu_restore.duration", "Restore CRIU restore", "seconds", 8.25
    )
    recorder.finalize("failed", error={"phase": "call", "message": "AssertionError: x"})

    document = recorder.as_dict()
    if os.environ.get("SNAPSHOT_E2E_UPDATE_GOLDEN") == "1":
        GOLDEN.parent.mkdir(parents=True, exist_ok=True)
        GOLDEN.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    assert document == json.loads(GOLDEN.read_text(encoding="utf-8")), (
        "benchmark result schema changed; review the diff and rerun with "
        "SNAPSHOT_E2E_UPDATE_GOLDEN=1 to accept it"
    )
