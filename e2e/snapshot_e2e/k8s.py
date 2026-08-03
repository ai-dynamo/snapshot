# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Kubernetes helpers shared by Snapshot e2e tests."""

from __future__ import annotations

import os
from dataclasses import dataclass

from kubernetes import client
from kubernetes.client import ApiException

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
        return cls(
            namespace=os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", "snapshot-e2e"),
            release=os.environ.get("SNAPSHOT_E2E_SNAPSHOT_RELEASE", "snapshot"),
            pvc_name=os.environ.get("SNAPSHOT_E2E_PVC_NAME", "snapshot-pvc"),
            kubeconfig=os.environ.get("SNAPSHOT_E2E_TARGET_KUBECONFIG")
            or os.environ.get("KUBECONFIG"),
        )


def configure(config: E2EConfig) -> None:
    load_config(config.kubeconfig, None)


def read_namespace(name: str) -> client.V1Namespace:
    return client.CoreV1Api().read_namespace(name)


def read_pvc(namespace: str, name: str) -> client.V1PersistentVolumeClaim:
    return client.CoreV1Api().read_namespaced_persistent_volume_claim(name, namespace)


def read_crd(name: str) -> client.V1CustomResourceDefinition:
    return client.ApiextensionsV1Api().read_custom_resource_definition(name)


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


def list_snapshot_deployments(
    namespace: str,
    release: str,
    component: str,
) -> list[client.V1Deployment]:
    return client.AppsV1Api().list_namespaced_deployment(
        namespace=namespace,
        label_selector=snapshot_selector(release, component),
    ).items


def list_snapshot_daemonsets(
    namespace: str,
    release: str,
    component: str,
) -> list[client.V1DaemonSet]:
    return client.AppsV1Api().list_namespaced_daemon_set(
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


def deployment_available(deployment: client.V1Deployment) -> bool:
    desired = deployment.spec.replicas or 1
    status = deployment.status
    observed = status.observed_generation or 0
    generation = deployment.metadata.generation or 0
    available = status.available_replicas or 0
    updated = status.updated_replicas or 0
    return observed >= generation and available >= desired and updated >= desired


def daemonset_ready(daemonset: client.V1DaemonSet) -> bool:
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    ready = status.number_ready or 0
    updated = status.updated_number_scheduled or 0
    return desired > 0 and ready >= desired and updated >= desired


def api_error_detail(exc: ApiException) -> str:
    return f"status={exc.status}, reason={exc.reason}, body={exc.body}"
