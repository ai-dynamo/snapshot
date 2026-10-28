# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The S3-compatible store the S3 e2e suite installs Snapshot against.

The store runs inside the cluster, on the host network of one node, and the
chart reaches it at ``cluster_endpoint``. Jobs that share a node need different
``SNAPSHOT_E2E_S3_PORT`` and ``SNAPSHOT_E2E_S3_CONSOLE_PORT`` values.

    python -m snapshot_e2e.s3 helm-set   # chart values, one KEY=VALUE per line
    python -m snapshot_e2e.s3 deploy     # the store itself, inside the cluster
    python -m snapshot_e2e.s3 endpoint   # the node address that store is reached at
"""

from __future__ import annotations

import argparse
import os
import time
from dataclasses import dataclass

from kubernetes import client, config

CREDENTIALS_KEY = "credentials"
DEFAULT_NAMESPACE = "snapshot-e2e"
STORE_NAME = "snapshot-s3"
DEFAULT_STORE_PORT = 19000
DEFAULT_STORE_CONSOLE_PORT = 19001
STORE_IMAGE = (
    "docker.io/bitnamilegacy/minio:2025.7.23-debian-12-r5"
    "@sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20"
)
STORE_READY_TIMEOUT_SECONDS = 300


@dataclass(frozen=True)
class S3Settings:
    cluster_endpoint: str
    access_key: str
    secret_key: str
    bucket: str
    prefix: str
    region: str
    credentials_secret: str
    port: int
    console_port: int

    @classmethod
    def from_env(cls) -> "S3Settings":
        env = os.environ.get
        return cls(
            cluster_endpoint=env("SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", ""),
            access_key=env("SNAPSHOT_E2E_S3_ACCESS_KEY", "minioadmin"),
            secret_key=env("SNAPSHOT_E2E_S3_SECRET_KEY", "minioadmin"),
            bucket=env("SNAPSHOT_E2E_S3_BUCKET", "snapshot-e2e"),
            prefix=env("SNAPSHOT_E2E_S3_PREFIX", "snapshots").strip("/"),
            region=env("SNAPSHOT_E2E_S3_REGION", "us-east-1"),
            credentials_secret=env(
                "SNAPSHOT_E2E_S3_CREDENTIALS_SECRET", "snapshot-s3-credentials"
            ),
            port=int(env("SNAPSHOT_E2E_S3_PORT", str(DEFAULT_STORE_PORT))),
            console_port=int(
                env("SNAPSHOT_E2E_S3_CONSOLE_PORT", str(DEFAULT_STORE_CONSOLE_PORT))
            ),
        )


def credentials_file(settings: S3Settings) -> str:
    return (
        "[default]\n"
        f"aws_access_key_id = {settings.access_key}\n"
        f"aws_secret_access_key = {settings.secret_key}\n"
    )


def helm_set(settings: S3Settings) -> list[str]:
    if not settings.cluster_endpoint:
        raise ValueError(
            "SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT is required: the address the cluster uses to reach the store"
        )
    return [
        "storage.type=s3",
        f"storage.s3.endpoint={settings.cluster_endpoint}",
        f"storage.s3.region={settings.region}",
        f"storage.s3.bucket={settings.bucket}",
        f"storage.s3.prefix={settings.prefix}",
        f"storage.s3.credentialsSecretRef={settings.credentials_secret}",
        "storage.s3.addressing=path",
        "storage.s3.allowHttp=true",
        "operator.bindNewContents=true",
    ]


def ensure_credentials_secret(settings: S3Settings, namespace: str) -> None:
    api = client.CoreV1Api()
    try:
        api.create_namespace(
            client.V1Namespace(metadata=client.V1ObjectMeta(name=namespace))
        )
    except client.ApiException as exc:
        if exc.status != 409:
            raise
    secret = client.V1Secret(
        metadata=client.V1ObjectMeta(name=settings.credentials_secret),
        string_data={CREDENTIALS_KEY: credentials_file(settings)},
    )
    try:
        api.create_namespaced_secret(namespace, secret)
    except client.ApiException as exc:
        if exc.status != 409:
            raise
        api.replace_namespaced_secret(settings.credentials_secret, namespace, secret)


def host_endpoint(host_ip: str, port: int = DEFAULT_STORE_PORT) -> str:
    return f"http://{host_ip}:{port}"


def store_endpoint(namespace: str, port: int = DEFAULT_STORE_PORT) -> str:
    pods = client.CoreV1Api().list_namespaced_pod(
        namespace, label_selector=f"app.kubernetes.io/name={STORE_NAME}"
    )
    for pod in pods.items:
        if pod.status.host_ip:
            return host_endpoint(pod.status.host_ip, port)
    raise LookupError(f"no {STORE_NAME} pod with a node address in {namespace}")


def store_deployment(settings: S3Settings, namespace: str) -> client.V1Deployment:
    labels = {"app.kubernetes.io/name": STORE_NAME}
    env = {
        "MINIO_ROOT_USER": settings.access_key,
        "MINIO_ROOT_PASSWORD": settings.secret_key,
        "MINIO_DEFAULT_BUCKETS": settings.bucket,
        "MINIO_API_PORT_NUMBER": str(settings.port),
        "MINIO_CONSOLE_PORT_NUMBER": str(settings.console_port),
    }
    container = client.V1Container(
        name="store",
        image=STORE_IMAGE,
        env=[client.V1EnvVar(name=name, value=value) for name, value in env.items()],
        ports=[client.V1ContainerPort(container_port=settings.port)],
        readiness_probe=client.V1Probe(
            http_get=client.V1HTTPGetAction(
                path="/minio/health/ready", port=settings.port
            ),
            period_seconds=2,
        ),
        resources=client.V1ResourceRequirements(
            requests={"cpu": "500m", "memory": "1Gi"}
        ),
        volume_mounts=[
            client.V1VolumeMount(name="data", mount_path="/bitnami/minio/data")
        ],
    )
    return client.V1Deployment(
        metadata=client.V1ObjectMeta(name=STORE_NAME, namespace=namespace, labels=labels),
        spec=client.V1DeploymentSpec(
            replicas=1,
            selector=client.V1LabelSelector(match_labels=labels),
            template=client.V1PodTemplateSpec(
                metadata=client.V1ObjectMeta(labels=labels),
                spec=client.V1PodSpec(
                    host_network=True,
                    dns_policy="ClusterFirstWithHostNet",
                    containers=[container],
                    volumes=[
                        client.V1Volume(
                            name="data", empty_dir=client.V1EmptyDirVolumeSource()
                        )
                    ],
                ),
            ),
        ),
    )


def deploy_store(settings: S3Settings, namespace: str) -> None:
    ensure_credentials_secret(settings, namespace)
    apps = client.AppsV1Api()
    try:
        apps.create_namespaced_deployment(
            namespace, store_deployment(settings, namespace)
        )
    except client.ApiException as exc:
        if exc.status != 409:
            raise
    deadline = time.monotonic() + STORE_READY_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        status = apps.read_namespaced_deployment_status(STORE_NAME, namespace).status
        if (status.ready_replicas or 0) >= 1:
            return
        time.sleep(3)
    raise TimeoutError(f"{STORE_NAME} was not ready in {namespace}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("helm-set", "deploy", "endpoint"))
    args = parser.parse_args(argv)
    settings = S3Settings.from_env()
    namespace = os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", DEFAULT_NAMESPACE)
    if args.command == "helm-set":
        print("\n".join(helm_set(settings)))
        return 0
    config.load_kube_config(os.environ.get("KUBECONFIG"))
    if args.command == "endpoint":
        print(store_endpoint(namespace, settings.port))
        return 0
    deploy_store(settings, namespace)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
