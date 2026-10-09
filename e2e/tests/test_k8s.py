# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for Kubernetes E2E helpers."""

from types import SimpleNamespace

from snapshot_e2e import k8s


def test_restart_operator_waits_for_terminating_previous_pod(monkeypatch) -> None:
    deployment = SimpleNamespace(
        metadata=SimpleNamespace(name="snapshot-operator", generation=2),
        spec=SimpleNamespace(replicas=1),
        status=SimpleNamespace(
            observed_generation=2,
            updated_replicas=1,
            available_replicas=1,
            replicas=1,
        ),
    )
    previous = SimpleNamespace(metadata=SimpleNamespace(deletion_timestamp="terminating"))
    replacement = SimpleNamespace(metadata=SimpleNamespace(deletion_timestamp=None))
    pod_lists = [[previous, replacement], [replacement]]
    pod_list_calls = []

    apps = SimpleNamespace(
        list_namespaced_deployment=lambda *_, **__: SimpleNamespace(items=[deployment]),
        patch_namespaced_deployment=lambda *_, **__: None,
        read_namespaced_deployment=lambda *_, **__: deployment,
    )

    def list_operator_pods(*args, **kwargs):
        pod_list_calls.append((args, kwargs))
        return SimpleNamespace(items=pod_lists.pop(0))

    core = SimpleNamespace(list_namespaced_pod=list_operator_pods)
    monkeypatch.setattr(k8s.client, "AppsV1Api", lambda: apps)
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: core)
    monkeypatch.setattr(k8s.time, "monotonic", lambda: 0)
    monkeypatch.setattr(k8s.time, "sleep", lambda _: None)

    k8s.restart_snapshot_operator("snapshot-system", "snapshot")

    assert len(pod_list_calls) == 2
