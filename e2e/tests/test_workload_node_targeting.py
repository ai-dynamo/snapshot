# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks that workload pods target nodes they can run on."""

from snapshot_e2e import workloads

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
