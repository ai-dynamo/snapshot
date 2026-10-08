# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Every exec into the agent pod must name the container it enters.

The agent pod runs the PageBroker sidecar beside the agent, and `kubectl exec`
against a multi-container pod fails outright when it is not told which one to
enter. The agent pod had a single container once, so these helpers relied on
the API server picking it; adding the sidecar broke all of them at once, and
only on a real cluster. These are the cluster-free version of that failure.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle

DAEMONSET = (
    Path(__file__).resolve().parents[2]
    / "charts"
    / "snapshot"
    / "templates"
    / "daemonset.yaml"
)

CONFIG = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)
CONTENT_UID = "content-uid"


def record_exec(monkeypatch) -> list[dict]:
    """Capture every exec the helper under test issues, without a cluster."""
    calls: list[dict] = []

    def fake_exec(namespace, pod, command, *, container=None):
        calls.append({"pod": pod, "command": command, "container": container})
        # Enough of a payload for exec_payload's marker split to succeed.
        return f"{k8s.PAYLOAD_MARKER}\n{{}}"

    monkeypatch.setattr(lifecycle, "checkpoint_agent_pod", lambda *_: "snapshot-agent-x")
    monkeypatch.setattr(k8s, "exec_command", fake_exec)
    return calls


# Every helper that reaches into the agent pod. Listed by name so a new one is
# a deliberate addition here rather than an untested path.
AGENT_HELPERS = [
    pytest.param(
        lambda: lifecycle.checkpoint_artifact_manifest(CONFIG, "node", CONTENT_UID),
        id="checkpoint_artifact_manifest",
    ),
    pytest.param(
        lambda: lifecycle.checkpoint_artifact_listing(CONFIG, "node", CONTENT_UID),
        id="checkpoint_artifact_listing",
    ),
    pytest.param(
        lambda: lifecycle.checkpoint_rootfs_file(CONFIG, "node", CONTENT_UID, "f"),
        id="checkpoint_rootfs_file",
    ),
    pytest.param(
        lambda: lifecycle.corrupt_checkpoint_image(CONFIG, "node", CONTENT_UID),
        id="corrupt_checkpoint_image",
    ),
    pytest.param(
        lambda: lifecycle.artifact_root_exists(CONFIG, "node", CONTENT_UID),
        id="artifact_root_exists",
    ),
    pytest.param(
        lambda: lifecycle.create_artifact_staging_file(CONFIG, "node", CONTENT_UID),
        id="create_artifact_staging_file",
    ),
    pytest.param(
        lambda: lifecycle.host_monitoring_agents(CONFIG, "node"),
        id="host_monitoring_agents",
    ),
    pytest.param(
        lambda: lifecycle.runtime_image_id(CONFIG, "node", "containerd://abc"),
        id="runtime_image_id",
    ),
]


@pytest.mark.parametrize("call", AGENT_HELPERS)
def test_agent_helpers_name_the_container(monkeypatch, call):
    calls = record_exec(monkeypatch)
    try:
        call()
    except AssertionError:
        # Some helpers parse their output; the fake payload is not meant to
        # satisfy them. The exec it issued on the way is what matters.
        pass
    assert calls, "helper issued no exec at all"
    for issued in calls:
        assert issued["container"] == lifecycle.AGENT_CONTAINER


def daemonset_container_names() -> list[str]:
    """Container names from the agent DaemonSet, excluding init containers.

    Read out of the template rather than a rendered chart so this needs no
    Helm; the names are plain YAML even where the surrounding fields are not.
    """
    names: list[str] = []
    section = None
    for line in DAEMONSET.read_text(encoding="utf-8").splitlines():
        if match := re.fullmatch(r" {6}(\w+):", line):
            section = match.group(1)
        elif match := re.fullmatch(r" {8}- name: ([\w-]+)", line):
            if section == "containers":
                names.append(match.group(1))
    return names


def test_the_agent_container_is_named_as_the_chart_names_it():
    assert lifecycle.AGENT_CONTAINER in daemonset_container_names()


def test_the_agent_pod_really_has_more_than_one_container():
    """Otherwise the requirement above is hypothetical and these tests are not.

    If the sidecar is ever removed, exec would start defaulting again and this
    test should be deleted along with the container argument — not left to
    assert a constraint the cluster no longer has.
    """
    assert len(daemonset_container_names()) > 1
