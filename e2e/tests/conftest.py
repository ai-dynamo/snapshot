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

    report = getattr(request.node, "benchmark_report_call", None)
    if report is None:
        session.finalize(
            "infrastructure_failed",
            error={"phase": "pytest", "message": "pytest produced no call report"},
        )
    elif report.skipped:
        session.finalize(
            "skipped",
            error={"phase": "call", "message": _report_message(report)},
        )
    elif report.failed:
        session.finalize(
            "failed",
            error={"phase": "call", "message": _report_message(report)},
        )
    else:
        session.finalize("passed")


def _report_message(report: pytest.TestReport) -> str:
    longreprtext = getattr(report, "longreprtext", "")
    return longreprtext or str(report.longrepr)
