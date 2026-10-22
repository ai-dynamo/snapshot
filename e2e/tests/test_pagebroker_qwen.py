# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Opt-in Qwen GPU smoke cases for the PageBroker stack."""

import os
from dataclasses import asdict
import json
from pathlib import Path

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e import pagebroker_qwen as qwen
from snapshot_e2e.workloads import TestRun as WorkloadRun


pytestmark = [
    pytest.mark.gpu,
    pytest.mark.pagebroker_qwen,
    pytest.mark.skipif(os.environ.get("SNAPSHOT_E2E_PAGEBROKER_QWEN") != "1",
                       reason="set SNAPSHOT_E2E_PAGEBROKER_QWEN=1 to run both Qwen smoke cases"),
]


@pytest.fixture
def qwen_run(config: k8s.E2EConfig):
    run = WorkloadRun.new("pagebroker-qwen")
    # Record ownership before creating resources so the runner can recover after a timeout.
    if ownership := os.environ.get("SNAPSHOT_E2E_PAGEBROKER_QWEN_OWNERSHIP"):
        with Path(ownership).open("x") as output:
            json.dump({"namespace": config.namespace, "run": asdict(run)}, output)
    yield run
    try:
        lifecycle.cleanup(config, run)
    finally:
        qwen.delete_app(config, run)


@pytest.mark.parametrize("case", qwen.CASES, ids=lambda case: case.name)
def test_pagebroker_qwen_repeated_restore(config, qwen_run, case, benchmark):
    result = benchmark.start(suite="pagebroker-qwen-smoke", case=case.name,
                             environment={"namespace": config.namespace})
    qwen.exercise(config, qwen_run, case, result)
