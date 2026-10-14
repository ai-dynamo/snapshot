# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from kubernetes import client

from snapshot_e2e import k8s


def pod(*entries: client.V1ManagedFieldsEntry) -> client.V1Pod:
    return client.V1Pod(metadata=client.V1ObjectMeta(name="p", managed_fields=list(entries) or None))


def test_status_writers_name_each_writer_and_its_conditions() -> None:
    writers = k8s.pod_status_writers(
        pod(
            client.V1ManagedFieldsEntry(
                manager="kubelet",
                operation="Update",
                subresource="status",
                time="t1",
                fields_v1={"f:status": {"f:conditions": {'k:{"type":"Ready"}': {}}, "f:phase": {}}},
            ),
            client.V1ManagedFieldsEntry(
                manager="snapshot-agent",
                operation="Apply",
                subresource="status",
                time="t2",
                fields_v1={"f:status": {"f:conditions": {'k:{"type":"nvidia.com/Restored"}': {}}}},
            ),
            client.V1ManagedFieldsEntry(
                manager="syncer",
                operation="Update",
                subresource="status",
                time="t3",
                fields_v1={"f:status": {"f:containerStatuses": {}, "f:phase": {}}},
            ),
        )
    )

    assert writers == [
        "kubelet Update subresource=status at=t1 status=['conditions', 'phase'] conditions=['Ready']",
        "snapshot-agent Apply subresource=status at=t2 status=['conditions'] conditions=['nvidia.com/Restored']",
        "syncer Update subresource=status at=t3 status=['containerStatuses', 'phase'] conditions=[]",
    ]


def test_non_status_writers_are_ignored() -> None:
    entry = client.V1ManagedFieldsEntry(manager="kubectl", operation="Update", time="t0", fields_v1={"f:metadata": {}})

    assert k8s.pod_status_writers(pod(entry)) == []


def test_pod_without_managed_fields_has_no_writers() -> None:
    assert k8s.pod_status_writers(pod()) == []
