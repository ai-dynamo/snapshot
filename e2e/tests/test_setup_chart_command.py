# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from snapshot_e2e.infra import setup


BASE = {
    "namespace": "snapshot-e2e",
    "release": "snapshot",
    "image_tag": "v0.0.0-g1a2b3c4d",
    "pvc_name": "snapshot-pvc",
    "timeout": "6m",
}


def set_values(command: list[str]) -> list[str]:
    return [command[i + 1] for i, arg in enumerate(command) if arg in ("--set", "--set-json")]


def test_default_command_installs_the_local_chart() -> None:
    command = setup.snapshot_chart_command(**BASE)

    assert command[:5] == ["helm", "upgrade", "--install", "snapshot", "./charts/snapshot"]
    assert "--version" not in command
    assert "--reset-then-reuse-values" not in command
    assert set_values(command) == [
        "image.operator.tag=v0.0.0-g1a2b3c4d",
        "image.agent.tag=v0.0.0-g1a2b3c4d",
        "storage.pvc.create=false",
        "storage.pvc.name=snapshot-pvc",
        "operator.artifactCleanup.scanInterval=5s",
        "daemonset.imagePullSecrets=[]",
    ]


def test_published_chart_is_installed_at_its_version() -> None:
    command = setup.snapshot_chart_command(
        **{**BASE, "image_tag": "v0.1.0"},
        chart="oci://ghcr.io/ai-dynamo/snapshot/snapshot",
        chart_version="0.1.0",
    )

    assert command[4] == "oci://ghcr.io/ai-dynamo/snapshot/snapshot"
    assert command[command.index("--version") + 1] == "0.1.0"
    assert "image.operator.tag=v0.1.0" in set_values(command)


def test_component_tags_override_the_shared_tag() -> None:
    command = setup.snapshot_chart_command(**BASE, agent_tag="v0.1.0")

    assert set_values(command)[:2] == [
        "image.operator.tag=v0.0.0-g1a2b3c4d",
        "image.agent.tag=v0.1.0",
    ]


def test_reset_then_reuse_values_sets_only_the_image_tags() -> None:
    command = setup.snapshot_chart_command(**BASE, reset_then_reuse_values=True)

    assert "--reset-then-reuse-values" in command
    assert set_values(command) == [
        "image.operator.tag=v0.0.0-g1a2b3c4d",
        "image.agent.tag=v0.0.0-g1a2b3c4d",
    ]


@pytest.mark.parametrize(
    ("env", "expected"),
    [
        ({}, ("./charts/snapshot", None)),
        (
            {
                "SNAPSHOT_E2E_CHART_REF": "oci://ghcr.io/ai-dynamo/snapshot/snapshot",
                "SNAPSHOT_E2E_CHART_VERSION": "0.1.0",
            },
            ("oci://ghcr.io/ai-dynamo/snapshot/snapshot", "0.1.0"),
        ),
        ({"SNAPSHOT_E2E_CHART_REF": "", "SNAPSHOT_E2E_CHART_VERSION": ""}, ("./charts/snapshot", None)),
    ],
)
def test_chart_source_comes_from_the_environment(
    monkeypatch: pytest.MonkeyPatch,
    env: dict[str, str],
    expected: tuple[str, str | None],
) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_CHART_REF", raising=False)
    monkeypatch.delenv("SNAPSHOT_E2E_CHART_VERSION", raising=False)
    for name, value in env.items():
        monkeypatch.setenv(name, value)

    args = setup.parse_args(["--phase", "snapshot-install"])

    assert (args.chart_ref, args.chart_version) == expected
