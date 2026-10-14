# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the canonical failure case against observations without a cluster."""

import xml.etree.ElementTree as ET
from pathlib import Path
from types import SimpleNamespace

import pytest
import yaml
import test_snapshot_lifecycle as cases

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e.restore_failure_qualification import PROBE_TEST, verify_junit

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

def {PROBE_TEST}(monkeypatch):
    exercise_case(monkeypatch, probe='true')
""")
    report = pytester.path / "cpu-junit.xml"
    result = pytester.runpytest_inprocess(
        "-p", "no:cacheprovider", "-q", f"--junitxml={report}"
    )
    result.assert_outcomes(failed=1)
    assert result.ret == pytest.ExitCode.TESTS_FAILED
    result.stdout.fnmatch_lines(["*unexpected terminal condition*RestoreFailed*"])
    # The nested test uses a unique module name to avoid pytest's import-file
    # collision with the real lifecycle test loaded by this process.
    root = ET.parse(report)
    case = root.find(".//testcase")
    assert case is not None
    case.set("classname", "test_snapshot_lifecycle")
    case.set("name", PROBE_TEST)
    root.write(report)
    verify_junit(report, "failure")


def test_junit_rejects_unrelated_failures_and_missing_evidence(tmp_path):
    report = tmp_path / "cpu-junit.xml"
    root = ET.Element("testsuites")
    suite = ET.SubElement(root, "testsuite")
    probe = ET.SubElement(suite, "testcase", classname="test_snapshot_lifecycle", name=PROBE_TEST)
    expected = (
        "AssertionError: restore reached unexpected terminal condition for "
        "snapshot-e2e/restore-pod: RestoreFailed: damaged inventory"
    )
    ET.SubElement(probe, "failure", message=expected)
    ET.ElementTree(root).write(report)

    verify_junit(report, "failure")
    with pytest.raises(ValueError, match="outcome was 'success'"):
        verify_junit(report, "success")

    ET.SubElement(suite, "testcase", classname="test_snapshot_lifecycle", name="other_test")
    ET.SubElement(suite[-1], "failure", message="AssertionError: unrelated failure")
    ET.ElementTree(root).write(report)
    with pytest.raises(ValueError, match="only the damaged-checkpoint case may fail"):
        verify_junit(report, "failure")
    suite.remove(suite[-1])

    failure = suite[0].find("failure")
    assert failure is not None
    failure.set("message", "TimeoutError: restore never reached a terminal condition")
    failure.text = expected  # A traceback mentioning the phrase is insufficient.
    ET.ElementTree(root).write(report)
    with pytest.raises(ValueError, match="did not fail on terminal RestoreFailed"):
        verify_junit(report, "failure")
    failure.set("message", expected)
    ET.SubElement(suite, "testcase", classname="test_snapshot_lifecycle", name="other_test")
    ET.SubElement(suite[-1], "error", message="fixture setup failed")
    ET.ElementTree(root).write(report)
    with pytest.raises(ValueError, match="only the damaged-checkpoint case may fail"):
        verify_junit(report, "failure")
    report.unlink()
    with pytest.raises(ValueError, match="missing or invalid"):
        verify_junit(report, "failure")


def test_workflow_checks_manual_probe_junit_after_pytest_failure():
    workflow = yaml.safe_load((Path(__file__).parents[2] / ".github/workflows/e2e-cpu.yaml").read_text())
    steps = workflow["jobs"]["cpu-e2e"]["steps"]
    names = [step["name"] for step in steps]
    cpu_tests = steps[names.index("Run CPU checkpoint/restore tests")]
    guard = steps[names.index("Verify restore-failure negative control")]
    assert cpu_tests["id"] == "cpu_tests"
    assert '--junitxml="${RUNNER_TEMP}/cpu-e2e-junit.xml"' in cpu_tests["run"]
    assert "!cancelled()" in guard["if"]
    assert "github.event_name == 'workflow_dispatch'" in guard["if"]
    assert "inputs.qualify_restore_failure" in guard["if"]
    assert guard["env"]["CPU_PYTEST_OUTCOME"] == "${{ steps.cpu_tests.outcome }}"
    assert "snapshot_e2e.restore_failure_qualification" in guard["run"]
    assert names.index(cpu_tests["name"]) < names.index(guard["name"]) < names.index("Collect diagnostics")
    assert 'cp "${RUNNER_TEMP}/cpu-e2e-junit.xml"' in steps[names.index("Collect diagnostics")]["run"]
    assert steps[-1]["name"] == "Delete k3d cluster"
    assert steps[-1]["if"] == "${{ always() }}"


def test_manual_qualification_is_not_cancelled_by_a_pr_update():
    workflow = yaml.safe_load((Path(__file__).parents[2] / ".github/workflows/e2e-cpu.yaml").read_text())
    concurrency = workflow["concurrency"]
    assert concurrency["cancel-in-progress"] == "${{ github.event_name == 'pull_request' }}"
    assert "github.event_name" in concurrency["group"]
    assert "github.event.pull_request.number || github.run_id" in concurrency["group"]
