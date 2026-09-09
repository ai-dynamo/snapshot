# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from typing import Any

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


def test_pod_logs_forwards_container(monkeypatch: Any) -> None:
    arguments: dict[str, Any] = {}

    class CoreV1Api:
        def read_namespaced_pod_log(self, **kwargs: Any) -> str:
            arguments.update(kwargs)
            return "logs"

    monkeypatch.setattr(k8s.client, "CoreV1Api", CoreV1Api)

    assert k8s.pod_logs("test", "snapshot-agent", container="pagebroker") == "logs"
    assert arguments["container"] == "pagebroker"


def test_host_monitoring_executes_in_agent_container(monkeypatch: Any) -> None:
    call: dict[str, Any] = {}
    config = k8s.E2EConfig(
        namespace="test",
        release="snapshot",
        pvc_name="snapshot-pvc",
        kubeconfig=None,
    )

    monkeypatch.setattr(
        snap,
        "checkpoint_agent_pod",
        lambda _config, _node: "snapshot-agent",
    )

    def exec_command(
        namespace: str,
        pod: str,
        command: str,
        *,
        container: str | None = None,
    ) -> str:
        call.update(
            namespace=namespace,
            pod=pod,
            command=command,
            container=container,
        )
        return "monitoring"

    monkeypatch.setattr(k8s, "exec_command", exec_command)

    assert snap.host_monitoring_agents(config, "gpu-node") == "monitoring"
    assert call["container"] == "agent"


def test_agent_and_pagebroker_logs_select_their_containers(
    monkeypatch: Any,
) -> None:
    containers: list[str | None] = []
    config = k8s.E2EConfig(
        namespace="test",
        release="snapshot",
        pvc_name="snapshot-pvc",
        kubeconfig=None,
    )

    def pod_logs(
        namespace: str,
        pod: str,
        *,
        tail_lines: int = 120,
        container: str | None = None,
    ) -> str:
        containers.append(container)
        return f"{namespace}/{pod}:{tail_lines}"

    monkeypatch.setattr(k8s, "pod_logs", pod_logs)

    snap._dump_agent_logs(config, "snapshot-agent", "gpu-node")
    snap._dump_pagebroker_logs(config, "snapshot-agent", "gpu-node")

    assert containers == ["agent", "pagebroker"]
