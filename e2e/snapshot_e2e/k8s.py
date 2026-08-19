# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Kubernetes helpers shared by Snapshot e2e tests."""

from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

from kubernetes import client
from kubernetes.client import ApiException
from kubernetes.stream import stream

from snapshot_e2e.infra.preflight import load_config


SNAPSHOT_LABEL = "app.kubernetes.io/name=snapshot"


@dataclass(frozen=True)
class E2EConfig:
    namespace: str
    release: str
    pvc_name: str
    kubeconfig: str | None

    @classmethod
    def from_env(cls) -> "E2EConfig":
        mode = os.environ.get("SNAPSHOT_E2E_MODE", "direct")
        if mode == "vcluster":
            kubeconfig = os.environ.get(
                "SNAPSHOT_E2E_TARGET_KUBECONFIG"
            ) or os.environ.get("KUBECONFIG")
        else:
            kubeconfig = os.environ.get("KUBECONFIG")

        return cls(
            namespace=os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", "snapshot-e2e"),
            release=os.environ.get("SNAPSHOT_E2E_SNAPSHOT_RELEASE", "snapshot"),
            pvc_name=os.environ.get("SNAPSHOT_E2E_PVC_NAME", "snapshot-pvc"),
            kubeconfig=kubeconfig,
        )


def configure(config: E2EConfig) -> None:
    load_config(config.kubeconfig, None)


def read_namespace(name: str) -> client.V1Namespace:
    return client.CoreV1Api().read_namespace(name)


def read_pvc(namespace: str, name: str) -> client.V1PersistentVolumeClaim:
    return client.CoreV1Api().read_namespaced_persistent_volume_claim(name, namespace)


def read_crd(name: str) -> client.V1CustomResourceDefinition:
    return client.ApiextensionsV1Api().read_custom_resource_definition(name)


def list_events(namespace: str) -> list[client.CoreV1Event]:
    return client.CoreV1Api().list_namespaced_event(namespace).items


def create_pod(body: dict[str, Any]) -> client.V1Pod:
    return client.CoreV1Api().create_namespaced_pod(
        namespace=body["metadata"]["namespace"],
        body=body,
    )


def read_pod(namespace: str, name: str) -> client.V1Pod:
    return client.CoreV1Api().read_namespaced_pod(name=name, namespace=namespace)


# JOB_NAME_LABEL is the label the batch/v1 Job controller stamps on every pod
# it creates (batch.kubernetes.io/job-name). A SnapshotJob's source pod name
# is not predictable (the Job controller appends a random suffix to the Job's
# own name), so this is how the source pod is found.
JOB_NAME_LABEL = "batch.kubernetes.io/job-name"


def list_job_pods(namespace: str, job_name: str) -> list[client.V1Pod]:
    return client.CoreV1Api().list_namespaced_pod(
        namespace=namespace,
        label_selector=f"{JOB_NAME_LABEL}={job_name}",
    ).items


def delete_pod(namespace: str, name: str) -> bool:
    try:
        client.CoreV1Api().delete_namespaced_pod(name=name, namespace=namespace)
        return True
    except ApiException as exc:
        if exc.status == 404:
            return False
        raise


def pod_logs(namespace: str, name: str, *, tail_lines: int = 120) -> str:
    try:
        return client.CoreV1Api().read_namespaced_pod_log(
            name=name,
            namespace=namespace,
            tail_lines=tail_lines,
            _preload_content=True,
        )
    except ApiException as exc:
        return f"<logs unavailable: {api_error_detail(exc)}>"


def exec_command(namespace: str, pod: str, command: str) -> str:
    return stream(
        client.CoreV1Api().connect_get_namespaced_pod_exec,
        pod,
        namespace,
        command=["/bin/bash", "-lc", command],
        stderr=True,
        stdin=False,
        stdout=True,
        tty=False,
    )


def snapshot_custom_resource_api_is_accessible(namespace: str) -> None:
    api = client.CustomObjectsApi()
    api.list_namespaced_custom_object(
        group="nvidia.com",
        version="v1alpha1",
        namespace=namespace,
        plural="podsnapshots",
    )
    api.list_cluster_custom_object(
        group="nvidia.com",
        version="v1alpha1",
        plural="podsnapshotcontents",
    )


def list_snapshot_daemonsets(
    namespace: str,
    release: str,
    component: str,
) -> list[client.V1DaemonSet]:
    return client.AppsV1Api().list_namespaced_daemon_set(
        namespace=namespace,
        label_selector=snapshot_selector(release, component),
    ).items


def list_snapshot_pods(
    namespace: str,
    release: str,
    component: str,
) -> list[client.V1Pod]:
    return client.CoreV1Api().list_namespaced_pod(
        namespace=namespace,
        label_selector=snapshot_selector(release, component),
    ).items


def snapshot_selector(release: str, component: str) -> str:
    return ",".join(
        [
            SNAPSHOT_LABEL,
            f"app.kubernetes.io/instance={release}",
            f"app.kubernetes.io/component={component}",
        ]
    )


def pod_containers_ready(pod: client.V1Pod) -> bool:
    statuses = list(pod.status.container_statuses or [])
    return bool(statuses) and all(status.ready for status in statuses)


def pod_readiness_detail(pod: client.V1Pod) -> str:
    statuses = [
        f"{status.name}:{status.ready}"
        for status in pod.status.container_statuses or []
    ]
    return (
        f"{pod.metadata.name} phase={pod.status.phase} "
        f"node={pod.spec.node_name or '<none>'} "
        f"containers={','.join(statuses) or '<none>'}"
    )


def daemonset_ready(daemonset: client.V1DaemonSet) -> bool:
    if not daemonset_observed(daemonset):
        return False
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    ready = status.number_ready or 0
    updated = status.updated_number_scheduled or 0
    return desired > 0 and ready >= desired and updated >= desired


def daemonset_scheduled(daemonset: client.V1DaemonSet) -> bool:
    if not daemonset_observed(daemonset):
        return False
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    current = status.current_number_scheduled or 0
    updated = status.updated_number_scheduled or 0
    return desired > 0 and current >= desired and updated >= desired


def daemonset_observed(daemonset: client.V1DaemonSet) -> bool:
    observed = daemonset.status.observed_generation or 0
    generation = daemonset.metadata.generation or 0
    return observed >= generation


def daemonset_readiness_detail(daemonset: client.V1DaemonSet) -> str:
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    current = status.current_number_scheduled or 0
    ready = status.number_ready or 0
    updated = status.updated_number_scheduled or 0
    available = status.number_available or 0
    selector = daemonset.spec.template.spec.node_selector or {}
    return (
        f"{daemonset.metadata.name} desired={desired} current={current} ready={ready} "
        f"updated={updated} available={available} nodeSelector={selector}"
    )


def api_error_detail(exc: ApiException) -> str:
    return f"status={exc.status}, reason={exc.reason}, body={exc.body}"
