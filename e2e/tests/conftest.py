# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.workloads import TestRun


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
