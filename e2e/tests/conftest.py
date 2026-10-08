# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import pytest

from snapshot_e2e import benchmark as benchmark_result
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.workloads import TestRun

# Tests named here must actually run. A case that stops being selected, or
# starts skipping, is the one regression a green check cannot show: pytest
# exits 0 having run whatever it found, and exits non-zero only when it
# collected nothing at all. CI names the cases it is there to prove; local runs
# leave this unset, where skipping is legitimate.
REQUIRED_TESTS_ENV = "SNAPSHOT_E2E_REQUIRED_TESTS"


def required_test_names() -> set[str]:
    raw = os.environ.get(REQUIRED_TESTS_ENV, "")
    return {name.strip() for name in raw.split(",") if name.strip()}


def item_names(item: pytest.Item) -> set[str]:
    """The names a required entry may match, parametrized or not.

    A parametrized item's ``name`` carries its parameters, so the bare function
    name is accepted too and one entry covers every case of it.
    """
    return {item.name, getattr(item, "originalname", None) or item.name}


# trylast, because deselection by marker and by -k happens in this same hook:
# running first would see the full collection and find nothing missing.
@pytest.hookimpl(trylast=True)
def pytest_collection_modifyitems(
    session: pytest.Session,
    config: pytest.Config,
    items: list[pytest.Item],
) -> None:
    """Refuse to run at all when a required case was not selected.

    Deselection is checked here rather than at the end because a run that
    never collects the case has nothing left to report it against.
    """
    required = required_test_names()
    if not required:
        return
    selected = {name for item in items for name in item_names(item)}
    missing = sorted(required - selected)
    if missing:
        raise pytest.UsageError(
            f"{REQUIRED_TESTS_ENV} requires tests that this run did not select: "
            + ", ".join(missing)
        )


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(
    item: pytest.Item,
    call: pytest.CallInfo[None],
):
    outcome = yield
    report = outcome.get_result()
    setattr(item, f"benchmark_report_{call.when}", report)
    setattr(item, f"benchmark_excinfo_{call.when}", call.excinfo)
    # A required case that skips leaves the suite green while proving nothing,
    # so the skip is the failure. Rewriting the report rather than failing the
    # session keeps the reason attached to the test that produced it.
    if report.skipped and item_names(item) & required_test_names():
        report.outcome = "failed"
        report.longrepr = (
            f"{item.name} is required to run, but it was skipped: {report_message(report)}"
        )


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
    # Plain-string longrepr (e.g. --tb=no) is a whole traceback; keep only the
    # crash line so the durable error stays a single line.
    text = str(getattr(report, "longreprtext", "") or str(longrepr)).strip()
    lines = [line for line in text.splitlines() if line.strip()]
    return lines[-1].strip() if lines else ""
