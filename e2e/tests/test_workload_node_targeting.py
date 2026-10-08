# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that workload pods target nodes they can run on."""

import pytest
from kubernetes import client

from snapshot_e2e import k8s, workloads

MIG_SELECTOR = "nvidia.com/mig.config"


def test_workload_pods_target_gpu_nodes(monkeypatch):
    monkeypatch.delenv("SNAPSHOT_E2E_AVOID_MIG", raising=False)

    selector = workloads.workload_scheduling()["nodeSelector"]

    assert selector["nvidia.com/gpu.present"] == "true"
    # Clusters with a single GPU pool do not expose the MIG label at all, so
    # selecting on it would leave every workload pod unschedulable.
    assert MIG_SELECTOR not in selector


def test_workload_pods_avoid_mig_nodes_only_when_opted_in(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_AVOID_MIG", "true")

    selector = workloads.workload_scheduling()["nodeSelector"]

    assert selector[MIG_SELECTOR] == "all-disabled"


def test_cpu_mode_uses_rwo_without_gpu_placement(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "true")
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "direct")
    monkeypatch.setenv("SNAPSHOT_E2E_AVOID_MIG", "true")
    config = k8s.E2EConfig.from_env()
    assert config.pvc_access_mode == "ReadWriteOnce"
    assert workloads.workload_scheduling() == {}
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "test-image")
    run = workloads.TestRun.new("cpu")
    pod = workloads.restore_pod(config=config, run=run, gpu=False, source_node="only-node")
    assert "nodeSelector" not in pod["spec"]
    assert "runtimeClassName" not in pod["spec"]
    assert pod["spec"]["affinity"] == workloads.same_node_affinity("only-node")
    with pytest.raises(ValueError, match="GPU workloads"):
        workloads.restore_pod(config=config, run=run, gpu=True)


def test_default_mode_retains_rwx(monkeypatch):
    monkeypatch.delenv("SNAPSHOT_E2E_CPU_ONLY", raising=False)
    assert k8s.E2EConfig.from_env().pvc_access_mode == "ReadWriteMany"


@pytest.mark.parametrize("value", ["TRUE", "1", "yes"])
def test_invalid_cpu_mode_is_rejected(monkeypatch, value):
    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", value)
    with pytest.raises(ValueError, match="must be true or false"):
        k8s.E2EConfig.from_env()


def test_cpu_mode_rejects_vcluster(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_CPU_ONLY", "true")
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "vcluster")
    with pytest.raises(ValueError, match="requires direct mode"):
        k8s.E2EConfig.from_env()


@pytest.mark.parametrize(
    "count,ready,unschedulable",
    [(0, True, False), (2, True, False), (1, False, False), (1, True, True)],
)
def test_cpu_mode_rejects_incompatible_nodes(monkeypatch, count, ready, unschedulable):
    node = client.V1Node(
        spec=client.V1NodeSpec(unschedulable=unschedulable),
        status=client.V1NodeStatus(
            conditions=[
                client.V1NodeCondition(type="Ready", status="True" if ready else "False")
            ]
        ),
    )
    monkeypatch.setattr(
        client.CoreV1Api, "list_node",
        lambda self: client.V1NodeList(items=[node] * count),
    )
    with pytest.raises(ValueError, match="CPU-only E2E requires"):
        k8s.require_single_cpu_node()


def test_cpu_mode_accepts_one_ready_node(monkeypatch):
    node = client.V1Node(
        spec=client.V1NodeSpec(),
        status=client.V1NodeStatus(
            conditions=[client.V1NodeCondition(type="Ready", status="True")]
        ),
    )
    monkeypatch.setattr(client.CoreV1Api, "list_node", lambda self: client.V1NodeList(items=[node]))
    k8s.require_single_cpu_node()
