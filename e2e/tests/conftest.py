# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from snapshot_e2e import benchmark as benchmark_result
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.workloads import TestRun


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(
    item: pytest.Item,
    call: pytest.CallInfo[None],
):
    outcome = yield
    report = outcome.get_result()
    setattr(item, f"benchmark_report_{call.when}", report)
    setattr(item, f"benchmark_excinfo_{call.when}", call.excinfo)


@pytest.fixture
def config() -> k8s.E2EConfig:
    value = k8s.E2EConfig.from_env()
    k8s.configure(value)
    return value


@pytest.fixture
def run(request: pytest.FixtureRequest, config: k8s.E2EConfig) -> TestRun:
    value = TestRun.new(request.node.name.replace("_", "-")[:24])
    yield value
    lifecycle.cleanup(config, value)


@pytest.fixture
def benchmark(request: pytest.FixtureRequest) -> benchmark_result.BenchmarkSession:
    test_name = getattr(request.node, "originalname", None) or request.node.name
    session = benchmark_result.BenchmarkSession(test_name)
    yield session

    outcome, error = benchmark_outcome(
        report=getattr(request.node, "benchmark_report_call", None),
        excinfo=getattr(request.node, "benchmark_excinfo_call", None),
        interrupted=request.session.exitstatus == pytest.ExitCode.INTERRUPTED,
    )
    session.finalize(outcome, error=error)


def benchmark_outcome(
    *,
    report: pytest.TestReport | None,
    excinfo: pytest.ExceptionInfo[BaseException] | None,
    interrupted: bool,
) -> tuple[str, dict[str, str] | None]:
    """Maps the pytest call report to a benchmark outcome and durable error.

    Only the crash line of a failure is kept. Full tracebacks stay in the
    pytest log and short-lived artifacts because results are retained outside
    the cluster.
    """
    if report is None:
        if interrupted:
            return "timed_out", {
                "phase": "call",
                "message": "pytest was interrupted before the test completed",
            }
        return "infrastructure_failed", {
            "phase": "pytest",
            "message": "pytest produced no call report",
        }
    if report.skipped:
        return "skipped", {"phase": "call", "message": report_message(report)}
    if report.failed:
        timed_out = excinfo is not None and isinstance(
            excinfo.value, lifecycle.LifecycleTimeoutError
        )
        return ("timed_out" if timed_out else "failed"), {
            "phase": "call",
            "message": report_message(report),
        }
    return "passed", None


def report_message(report: pytest.TestReport) -> str:
    longrepr = report.longrepr
    if isinstance(longrepr, tuple) and len(longrepr) == 3:
        return str(longrepr[2])
    crash = getattr(longrepr, "reprcrash", None)
    message = getattr(crash, "message", None)
    if message:
        return str(message)
    return getattr(report, "longreprtext", "") or str(longrepr)
