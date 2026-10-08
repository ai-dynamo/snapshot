# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that agent-pod helpers target the agent container."""

from types import SimpleNamespace

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle

CONFIG = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)


@pytest.mark.parametrize(
    "call",
    [
        lambda: lifecycle.checkpoint_artifact_manifest(CONFIG, "node", "uid"),
        lambda: lifecycle.checkpoint_artifact_listing(CONFIG, "node", "uid"),
        lambda: lifecycle.checkpoint_rootfs_file(CONFIG, "node", "uid", "etc/hosts"),
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
