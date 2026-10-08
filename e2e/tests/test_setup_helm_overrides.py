# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for extra Helm --set assignments during chart install."""

import pytest

from snapshot_e2e.infra import setup


def test_cli_assignments_are_kept_in_order():
    assert setup.parse_helm_overrides(
        ["image.agent.pullPolicy=IfNotPresent", "runtime.socketPath=/run/k3s.sock"]
    ) == ["image.agent.pullPolicy=IfNotPresent", "runtime.socketPath=/run/k3s.sock"]


def test_environment_fallback_is_newline_separated():
    # Newlines rather than commas: a comma is meaningful to helm --set itself.
    assert setup.parse_helm_overrides(
        None, "image.agent.tag=local\n\n  runtime.storageDir=/var/lib/k3s  \n"
    ) == ["image.agent.tag=local", "runtime.storageDir=/var/lib/k3s"]


def test_assignments_reach_helm_verbatim():
    # helm --set owns comma handling, so callers escape literal commas as `\,`
    # and we must not rewrite either form on the way through.
    assert setup.parse_helm_overrides(
        [r"a.b=one\,two", "c.d={one,two}"]
    ) == [r"a.b=one\,two", "c.d={one,two}"]


def test_cli_wins_over_the_environment():
    assert setup.parse_helm_overrides(["a=cli"], "b=env") == ["a=cli"]


@pytest.mark.parametrize("entry", ["image.agent.tag", "=orphan"])
def test_assignments_without_a_key_and_value_are_rejected(entry):
    with pytest.raises(setup.SetupError, match="KEY=VALUE"):
        setup.parse_helm_overrides([entry])


def test_overrides_are_appended_after_chart_defaults(monkeypatch):
    commands = []
    monkeypatch.setattr(setup, "run", lambda command, **_: commands.append(command))

    setup.install_snapshot_chart(
        kubeconfig=None,
        namespace="snapshot-e2e",
        release="snapshot",
        image_tag="local",
        pvc_name="snapshot-pvc",
        timeout="6m",
        helm_overrides=["image.agent.tag=override"],
    )

    (command,) = commands
    # Helm resolves repeated keys last-wins, so the override has to come after
    # the chart defaults to take effect.
    assert command[-2:] == ["--set", "image.agent.tag=override"]
    assert command.index("image.agent.tag=local") < command.index(
        "image.agent.tag=override"
    )


def test_install_enables_bound_publications_for_the_lifecycle_suite(monkeypatch):
    commands = []
    monkeypatch.setattr(setup, "run", lambda command, **_: commands.append(command))

    setup.install_snapshot_chart(
        kubeconfig=None,
        namespace="snapshot-e2e",
        release="snapshot",
        image_tag="local",
        pvc_name="snapshot-pvc",
        timeout="6m",
    )

    (command,) = commands
    assert "pageBroker.artifactAddressing=true" in command
    assert "operator.bindNewContents=true" in command
    assert command[-2:] == ["--set-json", "daemonset.imagePullSecrets=[]"]
