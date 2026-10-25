# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os

import pytest

from snapshot_e2e import benchmark as benchmark_result
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.workloads import TestRun

# Comma-separated test names that this run must actually execute. Set by CI,
# unset locally. See e2e/README.md for what it is for.
REQUIRED_TESTS_ENV = "SNAPSHOT_E2E_REQUIRED_TESTS"

# What each required name matched when collection found it, before anything was
# deselected. Comparing against that, rather than against the names that
# survive, is what makes a dropped parameter case visible: the bare function
# name still matches the cases that remain.
COLLECTED_MATCHES = pytest.StashKey[dict[str, set[str]]]()


def required_test_names() -> set[str]:
    raw = os.environ.get(REQUIRED_TESTS_ENV, "")
    return {name.strip() for name in raw.split(",") if name.strip()}


def item_names(item: pytest.Item) -> set[str]:
    """The names a required entry may match, parametrized or not.

    A parametrized item's ``name`` carries its parameters, so the bare function
    name is accepted too and one entry covers every case of it.
    """
    return {item.name, getattr(item, "originalname", None) or item.name}


def matches(items: list[pytest.Item], name: str) -> set[str]:
    return {item.nodeid for item in items if name in item_names(item)}


# tryfirst, to run before marker and -k deselection, which happen in this same
# hook. This records only; the comparison needs the final selection.
@pytest.hookimpl(tryfirst=True)
def pytest_collection_modifyitems(
    session: pytest.Session,
    config: pytest.Config,
    items: list[pytest.Item],
) -> None:
    required = required_test_names()
    if not required:
        return
    session.stash[COLLECTED_MATCHES] = {
        name: matches(items, name) for name in required
    }


def pytest_collection_finish(session: pytest.Session) -> None:
    """Refuse to run at all when a required case will not run.

    Checked at the end of collection rather than during a run, because a case
    that was never selected has nothing left to report a failure against.
    """
    collected = session.stash.get(COLLECTED_MATCHES, None)
    if not collected:
        return
    problems = []
    for name, found in sorted(collected.items()):
        if not found:
            problems.append(f"{name} (no such test was collected)")
            continue
        dropped = found - matches(session.items, name)
        if dropped:
            problems.append(f"{name} (deselected: {', '.join(sorted(dropped))})")
    if problems:
        raise pytest.UsageError(
            f"{REQUIRED_TESTS_ENV} requires tests that this run did not select: "
            + "; ".join(problems)
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
