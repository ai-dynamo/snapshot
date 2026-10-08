# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the canonical failure case against observations without a cluster."""

import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest
import yaml
import test_snapshot_lifecycle as cases

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap

PROBE_ENV = "SNAPSHOT_E2E_RESTORE_FAILURE_PROBE"


def exercise_case(monkeypatch, *, probe="false", cpu_only=True, restore_reason="RestoreFailed"):
    monkeypatch.setenv(PROBE_ENV, probe)
    config = k8s.E2EConfig("test-ns", "snapshot", "pvc", None, cpu_only=cpu_only)
    run = SimpleNamespace(source_pod="source", snapshot_name="snapshot", restore_pod="restore")
    ready = {"status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    content = {**ready, "metadata": {"uid": "owned-content"}}
    calls = []
    monkeypatch.setattr(cases, "create_valid_checkpoint", lambda *_args, **_kw: (None, "node", None))
    monkeypatch.setattr(snap, "wait_for_snapshot_ready", lambda *_args, **_kw: (ready, content))
    monkeypatch.setattr(snap, "checkpoint_agent_pod", lambda *_: "agent-pod")
    monkeypatch.setattr(k8s, "exec_command", lambda *args, **kw: calls.append((args, kw)))
    monkeypatch.setattr(k8s, "delete_pod", lambda *_: None)
    monkeypatch.setattr(snap, "wait_for_pod_deleted", lambda *_: None)
    monkeypatch.setattr(snap, "restore_pod", lambda **_: {})
    monkeypatch.setattr(k8s, "create_pod", lambda *_: None)
    monkeypatch.setattr(snap, "debug_dump", lambda *_: None)
    monkeypatch.setattr(cases, "assert_restore_events", lambda *_: None)
    monkeypatch.setattr(cases, "restore_event_reasons", lambda *_: {"RestoreFailed"})
    failed = SimpleNamespace(status=SimpleNamespace(conditions=[SimpleNamespace(
        type=snap.RESTORED_CONDITION,
        status="True" if restore_reason == "RestoreSucceeded" else "False",
        reason=restore_reason,
        message="CRIU could not read the damaged inventory",
    )]))
    monkeypatch.setattr(k8s, "read_pod", lambda *_: failed)
    # Keep the real terminal-condition check. No clock wait is necessary: a
    # terminal failure returns or raises on its first observation.
    cases.test_restore_from_a_damaged_checkpoint_fails_and_keeps_the_snapshot(config, run)
    return calls


def test_expected_restore_failure_still_passes(monkeypatch):
    calls = exercise_case(monkeypatch)
    assert len(calls) == 1
    args, kwargs = calls[0]
    assert args[:2] == ("test-ns", "agent-pod")
    assert "owned-content/containers/main/inventory.img" in args[2]
    assert kwargs == {"container": "agent"}


def test_probe_does_not_accept_a_terminal_restore_failure(monkeypatch):
    with pytest.raises(AssertionError, match="unexpected terminal condition.*RestoreFailed"):
        exercise_case(monkeypatch, probe="true")


def test_probe_does_not_force_red_when_corruption_was_ignored(monkeypatch):
    # A green probe is rejected by qualification. Do not synthesize a failure
    # after restore success and accidentally accept an ineffective injection.
    exercise_case(monkeypatch, probe="true", restore_reason="RestoreSucceeded")


def test_probe_wait_is_bounded(monkeypatch):
    def wait_for(description, check, timeout, **kwargs):
        assert "Restored=True/RestoreSucceeded" in description
        assert timeout == 60
        raise TimeoutError("probe deadline")

    monkeypatch.setattr(snap, "wait_for", wait_for)
    with pytest.raises(TimeoutError, match="probe deadline"):
        exercise_case(monkeypatch, probe="true")


@pytest.mark.parametrize("value", ["TRUE", "1", "typo"])
def test_probe_rejects_invalid_values_before_capture(monkeypatch, value):
    monkeypatch.setattr(cases, "create_valid_checkpoint", lambda *_a, **_kw: pytest.fail("capture must not run"))
    monkeypatch.setenv(PROBE_ENV, value)
    with pytest.raises(ValueError, match="must be true or false"):
        cases.test_restore_from_a_damaged_checkpoint_fails_and_keeps_the_snapshot(
            k8s.E2EConfig("ns", "release", "pvc", None, cpu_only=True), None,
        )


def test_probe_rejects_non_cpu_mode_before_capture(monkeypatch):
    monkeypatch.setenv(PROBE_ENV, "true")
    with pytest.raises(ValueError, match="requires CPU-only mode"):
        cases.test_restore_from_a_damaged_checkpoint_fails_and_keeps_the_snapshot(
            k8s.E2EConfig("ns", "release", "pvc", None), None,
        )


def test_probe_makes_the_real_pytest_run_fail(pytester, monkeypatch):
    # The inner test does not catch the canonical test's assertion. Verify the
    # process exit verdict CI consumes, rather than just testing a helper.
    monkeypatch.delenv("SNAPSHOT_E2E_REQUIRED_TESTS", raising=False)
    tests_dir = str(Path(__file__).parent)
    pytester.makepyfile(test_probe=f"""
import sys
sys.path.insert(0, {tests_dir!r})
from test_cpu_restore_failure_probe import exercise_case

def test_real_restore_failure_probe(monkeypatch):
    exercise_case(monkeypatch, probe='true')
""")
    result = pytester.runpytest_inprocess("-p", "no:cacheprovider", "-q")
    result.assert_outcomes(failed=1)
    assert result.ret == pytest.ExitCode.TESTS_FAILED
    result.stdout.fnmatch_lines(["*unexpected terminal condition*RestoreFailed*"])


def test_workflow_rejects_a_green_manual_probe():
    workflow = yaml.safe_load((Path(__file__).parents[2] / ".github/workflows/e2e-cpu.yaml").read_text())
    steps = workflow["jobs"]["cpu-e2e"]["steps"]
    names = [step["name"] for step in steps]
    guard = steps[names.index("Reject an unexpectedly successful restore-failure probe")]
    assert "success()" in guard["if"]
    assert "github.event_name == 'workflow_dispatch'" in guard["if"]
    assert "inputs.qualify_restore_failure" in guard["if"]
    assert names.index("Run CPU checkpoint/restore tests") < names.index(guard["name"]) < names.index("Collect diagnostics")
    # Execute the exact workflow shell: unexpected pytest success must become
    # a blocking, explicitly ineffective-injection verdict with diagnostics.
    result = subprocess.run(["bash", "-e", "-c", guard["run"]], capture_output=True, text=True)
    assert result.returncode == 1
    assert "::error::" in result.stdout
    assert "did not prove a restore failure" in result.stdout
    assert steps[-1]["name"] == "Delete k3d cluster"
    assert steps[-1]["if"] == "${{ always() }}"


def test_manual_qualification_is_not_cancelled_by_a_pr_update():
    workflow = yaml.safe_load((Path(__file__).parents[2] / ".github/workflows/e2e-cpu.yaml").read_text())
    concurrency = workflow["concurrency"]
    assert concurrency["cancel-in-progress"] == "${{ github.event_name == 'pull_request' }}"
    assert "github.event_name" in concurrency["group"]
    assert "github.event.pull_request.number || github.run_id" in concurrency["group"]
