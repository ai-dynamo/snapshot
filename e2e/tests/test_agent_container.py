# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that agent-pod helpers target the agent container."""

import re
from pathlib import Path
from types import SimpleNamespace

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle

CONFIG = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)
DAEMONSET = (
    Path(__file__).resolve().parents[2]
    / "charts"
    / "snapshot"
    / "templates"
    / "daemonset.yaml"
)


@pytest.mark.parametrize(
    "call",
    [
        lambda: lifecycle.checkpoint_artifact_manifest(CONFIG, "node", "uid"),
        lambda: lifecycle.checkpoint_artifact_listing(CONFIG, "node", "uid"),
        lambda: lifecycle.checkpoint_rootfs_file(CONFIG, "node", "uid", "etc/hosts"),
        lambda: lifecycle.corrupt_checkpoint_image(CONFIG, "node", "uid"),
        lambda: lifecycle.artifact_root_exists(CONFIG, "node", "uid"),
        lambda: lifecycle.create_artifact_staging_file(CONFIG, "node", "uid"),
        lambda: lifecycle.host_monitoring_agents(CONFIG, "node"),
        lambda: lifecycle._dump_nvidia_smi(CONFIG, "agent-pod", "node"),
        lambda: lifecycle._dump_kernel_log(CONFIG, "agent-pod", "node"),
    ],
)
def test_agent_pod_exec_names_the_agent_container(monkeypatch, call):
    calls = []

    def exec_command(namespace, pod, command, *, container=None):
        calls.append((namespace, pod, container))
        return f"{k8s.PAYLOAD_MARKER}\n"

    monkeypatch.setattr(lifecycle, "checkpoint_agent_pod", lambda *_: "agent-pod")
    monkeypatch.setattr(k8s, "exec_command", exec_command)

    call()

    assert calls == [("test-ns", "agent-pod", "agent")]


def test_agent_log_dump_names_the_agent_container(monkeypatch):
    calls = []

    def pod_logs(namespace, name, *, tail_lines=120, container=None):
        calls.append((namespace, name, container))
        return ""

    monkeypatch.setattr(k8s, "pod_logs", pod_logs)

    lifecycle._dump_agent_logs(CONFIG, "agent-pod", "node")

    assert calls == [("test-ns", "agent-pod", "agent")]


def test_snapshot_pod_log_dump_reads_every_container(monkeypatch):
    agent = SimpleNamespace(
        metadata=SimpleNamespace(name="agent-pod"),
        status=SimpleNamespace(phase="Running"),
        spec=SimpleNamespace(
            containers=[SimpleNamespace(name="pagebroker"), SimpleNamespace(name="agent")]
        ),
    )
    core = SimpleNamespace(
        list_namespaced_pod=lambda *_, **__: SimpleNamespace(items=[agent])
    )
    calls = []

    def pod_logs(namespace, name, *, tail_lines=120, container=None):
        calls.append((name, container))
        return ""

    monkeypatch.setattr(lifecycle.client, "CoreV1Api", lambda: core)
    monkeypatch.setattr(k8s, "pod_logs", pod_logs)

    lifecycle.print_snapshot_controller_logs(CONFIG)

    assert calls == [("agent-pod", "pagebroker"), ("agent-pod", "agent")]


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
    """Otherwise the checks above are hypothetical and they do not say so.

    If the sidecar is ever removed, exec starts defaulting again and this file
    should go with the container argument, rather than be left asserting a
    constraint the cluster no longer has.
    """
    assert len(daemonset_container_names()) > 1
