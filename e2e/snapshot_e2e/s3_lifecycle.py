# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Capture, restore and chart changes shared by the S3 e2e tests."""

from __future__ import annotations

import json
import os
import subprocess
from dataclasses import dataclass
from typing import Any, Sequence

from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e.infra.setup import DEFAULT_CHART

PAGEBROKER_CONTAINER = "pagebroker"
HELM_TIMEOUT = "6m"


@dataclass(frozen=True)
class Capture:
    source: Any
    node: str
    observations: int
    content: dict[str, Any]

    @property
    def content_uid(self) -> str:
        return self.content["metadata"]["uid"]


def start_source(config: k8s.E2EConfig, run: snap.TestRun) -> tuple[Any, str, int]:
    k8s.create_pod(snap.source_pod(config=config, run=run, gpu=False))
    source = snap.wait_for_pod_ready(config.namespace, run.source_pod)
    snap.wait_for_file(config.namespace, run.source_pod, snap.SOURCE_READY)
    observations = snap.wait_for_state_observations(
        config.namespace,
        run.source_pod,
        run.source_token,
        gpu=False,
        minimum=2,
    )
    return source, source.spec.node_name, observations


def capture(config: k8s.E2EConfig, run: snap.TestRun) -> Capture:
    source, node, observations = start_source(config, run)
    snap.create_podsnapshot(
        config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
    )
    _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
    return Capture(source, node, observations, content)


def restore_and_verify(
    config: k8s.E2EConfig, run: snap.TestRun, captured: Capture
) -> None:
    k8s.delete_pod(config.namespace, run.source_pod)
    snap.wait_for_pod_deleted(config.namespace, run.source_pod)
    k8s.create_pod(
        snap.restore_pod(
            config=config, run=run, gpu=False, source_node=captured.node
        )
    )
    snap.wait_for_restored_condition(
        config.namespace, run.restore_pod, "True", "RestoreSucceeded"
    )
    snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)
    snap.assert_restored_state(
        config.namespace,
        run.restore_pod,
        source_token=run.source_token,
        restore_token=run.restore_token,
        checkpoint_observations=captured.observations,
        gpu=False,
    )


def release_revision(config: k8s.E2EConfig) -> int:
    output = _helm(config, ["status", config.release, "--output", "json"])
    return int(json.loads(output)["version"])


def upgrade_chart(
    config: k8s.E2EConfig, values: Sequence[str], *, wait: bool = True
) -> None:
    command = [
        "upgrade",
        config.release,
        os.environ.get("SNAPSHOT_E2E_CHART_REF") or DEFAULT_CHART,
        "--reuse-values",
    ]
    if wait:
        command += ["--wait", "--timeout", HELM_TIMEOUT]
    for value in values:
        command += ["--set", value]
    _helm(config, command)


def rollback_chart(config: k8s.E2EConfig, revision: int) -> None:
    _helm(
        config,
        ["rollback", config.release, str(revision), "--wait", "--timeout", HELM_TIMEOUT],
    )


def previous_container_logs(namespace: str, pod: str, container: str) -> str:
    try:
        return client.CoreV1Api().read_namespaced_pod_log(
            name=pod, namespace=namespace, container=container, previous=True
        )
    except client.ApiException:
        return k8s.pod_logs(namespace, pod, container=container)


def _helm(config: k8s.E2EConfig, args: list[str]) -> str:
    env = os.environ.copy()
    if config.kubeconfig:
        env["KUBECONFIG"] = config.kubeconfig
    result = subprocess.run(
        ["helm", *args, "--namespace", config.namespace],
        env=env,
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise AssertionError(
            f"helm {args[0]} failed ({result.returncode}): {result.stderr.strip()}"
        )
    return result.stdout
