# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import traceback
from datetime import datetime, timezone
from typing import Iterator

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.upgrade import checks
from snapshot_e2e.upgrade import configs
from snapshot_e2e.upgrade import scenarios
from snapshot_e2e.upgrade.context import ScenarioState, UpgradeContext, UpgradeSettings


pytestmark = pytest.mark.upgrade


@pytest.fixture(scope="module")
def upgraded() -> Iterator[UpgradeContext]:
    config = k8s.E2EConfig.from_env()
    k8s.configure(config)
    settings = UpgradeSettings.from_env()
    upgrade = configs.get(settings.config)
    ctx = UpgradeContext(config=config, settings=settings)
    title = f"Upgrade {settings.from_tag} → {settings.to_tag} ({settings.config}, {settings.profile})"
    try:
        installed = checks.installed_tags(ctx)
        assert set(installed.values()) == {settings.from_tag}, (
            f"installed Snapshot images are {installed}; install {settings.from_tag} before the upgrade test"
        )
        for scenario in scenarios.selected(settings):
            if not scenario.applies_to(ctx):
                continue
            state = ScenarioState(run=ctx.new_run(scenario.run_prefix))
            ctx.states[scenario.name] = state
            try:
                with ctx.timings.phase(f"PreUpgrade {scenario.name}"):
                    scenario.pre_upgrade(ctx, state)
            except Exception as exc:
                state.pre_upgrade_error = exc
                traceback.print_exc()
                lifecycle.debug_dump(config, state.run)

        ctx.revision_before, _ = checks.helm_release(ctx)
        ctx.upgrade_started = datetime.now(timezone.utc)
        try:
            with ctx.timings.phase(f"Upgrade ({upgrade.name})"):
                upgrade.apply(ctx)
        except Exception as exc:
            ctx.upgrade_error = exc
        yield ctx
    finally:
        ctx.timings.publish(title)
        for state in ctx.states.values():
            try:
                lifecycle.cleanup(config, state.run)
            except Exception as exc:
                print(f"cleanup warning for {state.run.suffix}: {exc}")


def require_upgrade(ctx: UpgradeContext) -> None:
    if ctx.upgrade_error is not None:
        pytest.fail(f"the upgrade did not complete, see test_upgrade_completed: {ctx.upgrade_error}")


def test_upgrade_completed(upgraded: UpgradeContext) -> None:
    upgrade = configs.get(upgraded.settings.config)
    try:
        if upgraded.upgrade_error is not None:
            raise upgraded.upgrade_error
        with upgraded.timings.phase("Upgrade checks"):
            checks.assert_upgrade_completed(upgraded, upgrade)
    except Exception:
        checks.dump_diagnostics(upgraded)
        raise


def test_global_invariants(upgraded: UpgradeContext) -> None:
    require_upgrade(upgraded)
    with upgraded.timings.phase("Global invariants"):
        checks.assert_global_invariants(upgraded)


@pytest.mark.parametrize("name", [scenario.name for scenario in scenarios.SCENARIOS])
def test_post_upgrade(upgraded: UpgradeContext, name: str) -> None:
    state = upgraded.states.get(name)
    if state is None:
        pytest.skip(f"{name} is not selected for this run")
    require_upgrade(upgraded)
    if state.pre_upgrade_error is not None:
        pytest.fail(f"PreUpgrade of {name} failed: {state.pre_upgrade_error}")
    try:
        with upgraded.timings.phase(f"PostUpgrade {name}"):
            scenarios.by_name(name).post_upgrade(upgraded, state)
    except Exception:
        lifecycle.debug_dump(upgraded.config, state.run)
        raise
