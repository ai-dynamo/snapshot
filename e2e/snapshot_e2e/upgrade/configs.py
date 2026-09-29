# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""How the upgrade from FROM to TO is performed."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from snapshot_e2e.infra import setup
from snapshot_e2e.upgrade.context import UpgradeContext, UpgradeSettings


CHART_DIR = Path(__file__).resolve().parents[3] / "charts" / "snapshot"


@dataclass(frozen=True)
class UpgradeConfig:
    name: str
    operator_stays_on_from: bool = False
    agent_stays_on_from: bool = False
    reuse_values: bool = False

    def operator_tag(self, settings: UpgradeSettings) -> str:
        return settings.from_tag if self.operator_stays_on_from else settings.to_tag

    def agent_tag(self, settings: UpgradeSettings) -> str:
        return settings.from_tag if self.agent_stays_on_from else settings.to_tag

    def apply(self, ctx: UpgradeContext) -> None:
        settings = ctx.settings
        setup.install_snapshot_chart(
            kubeconfig=ctx.config.kubeconfig,
            namespace=ctx.config.namespace,
            release=ctx.config.release,
            image_tag=settings.to_tag,
            pvc_name=ctx.config.pvc_name,
            timeout=settings.helm_timeout,
            chart=str(CHART_DIR),
            operator_tag=self.operator_tag(settings),
            agent_tag=self.agent_tag(settings),
            reuse_values=self.reuse_values,
        )


CONFIGS = {config.name: config for config in (UpgradeConfig("full"),)}


def get(name: str) -> UpgradeConfig:
    try:
        return CONFIGS[name]
    except KeyError:
        raise ValueError(f"unknown upgrade config {name!r}; expected one of {sorted(CONFIGS)}") from None
