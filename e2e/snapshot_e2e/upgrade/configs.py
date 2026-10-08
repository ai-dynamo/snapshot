# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""How the upgrade from FROM to TO is performed."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from kubernetes import client

from snapshot_e2e.infra import setup
from snapshot_e2e.upgrade import checks
from snapshot_e2e.upgrade.context import UpgradeContext, UpgradeSettings


CHART_DIR = Path(__file__).resolve().parents[3] / "charts" / "snapshot"
STRATEGIC_MERGE_PATCH = "application/strategic-merge-patch+json"


@dataclass(frozen=True)
class UpgradeConfig:
    name: str
    hold_operator: bool = False
    hold_agent: bool = False
    reset_then_reuse_values: bool = False

    def operator_tag(self, settings: UpgradeSettings) -> str:
        return settings.from_tag if self.hold_operator else settings.to_tag

    def agent_tag(self, settings: UpgradeSettings) -> str:
        return settings.from_tag if self.hold_agent else settings.to_tag

    def apply(self, ctx: UpgradeContext) -> None:
        apps = client.AppsV1Api()
        namespace = ctx.config.namespace
        if self.hold_operator:
            deployment = checks.operator_deployment(ctx)
            apps.patch_namespaced_deployment(
                deployment.metadata.name,
                namespace,
                {"spec": {"paused": True}},
                _content_type=STRATEGIC_MERGE_PATCH,
            )
        if self.hold_agent:
            daemonset = checks.agent_daemonset(ctx)
            apps.patch_namespaced_daemon_set(
                daemonset.metadata.name,
                namespace,
                {"spec": {"updateStrategy": {"type": "OnDelete", "rollingUpdate": None}}},
                _content_type=STRATEGIC_MERGE_PATCH,
            )
        settings = ctx.settings
        setup.install_snapshot_chart(
            kubeconfig=ctx.config.kubeconfig,
            namespace=namespace,
            release=ctx.config.release,
            image_tag=settings.to_tag,
            pvc_name=ctx.config.pvc_name,
            timeout=settings.helm_timeout,
            chart=str(CHART_DIR),
            reset_then_reuse_values=self.reset_then_reuse_values,
            helm_overrides=setup.parse_helm_overrides(None, os.environ.get("SNAPSHOT_E2E_HELM_SET")),
        )


CONFIGS = {
    config.name: config
    for config in (
        UpgradeConfig("full"),
        UpgradeConfig("reset-then-reuse-values", reset_then_reuse_values=True),
        UpgradeConfig("operator-first", hold_agent=True),
        UpgradeConfig("agent-first", hold_operator=True),
    )
}


def get(name: str) -> UpgradeConfig:
    try:
        return CONFIGS[name]
    except KeyError:
        raise ValueError(f"unknown upgrade config {name!r}; expected one of {sorted(CONFIGS)}") from None
