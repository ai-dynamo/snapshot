# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The S3-compatible store the S3 e2e suite installs Snapshot against.

The store runs inside the cluster, on the host network of one node. Jobs that
share a node need different ``SNAPSHOT_E2E_S3_PORT`` and
``SNAPSHOT_E2E_S3_CONSOLE_PORT`` values. The store listens on the node network,
so ``SNAPSHOT_E2E_S3_ACCESS_KEY`` and ``SNAPSHOT_E2E_S3_SECRET_KEY`` are required.

    python -m snapshot_e2e.s3 deploy     # the store, then chart values, one KEY=VALUE per line
"""

from __future__ import annotations

import argparse
import ipaddress
import os
import time
from dataclasses import dataclass

from kubernetes import client, config

CREDENTIALS_KEY = "credentials"
ROOT_USER_KEY = "MINIO_ROOT_USER"
ROOT_PASSWORD_KEY = "MINIO_ROOT_PASSWORD"
DEFAULT_NAMESPACE = "snapshot-e2e"
BUCKET = "snapshot-e2e"
PREFIX = "snapshots"
REGION = "us-east-1"
CREDENTIALS_SECRET = "snapshot-s3-credentials"
ACCESS_KEY_ENV = "SNAPSHOT_E2E_S3_ACCESS_KEY"
SECRET_KEY_ENV = "SNAPSHOT_E2E_S3_SECRET_KEY"
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
    access_key: str
    secret_key: str
    port: int
    console_port: int

    @classmethod
    def from_env(cls) -> "S3Settings":
        env = os.environ
        missing = [name for name in (ACCESS_KEY_ENV, SECRET_KEY_ENV) if not env.get(name)]
        if missing:
            raise ValueError(
                f"{', '.join(missing)} required: the store listens on the node network "
                "and must not use a default login"
            )
        return cls(
            access_key=env[ACCESS_KEY_ENV],
            secret_key=env[SECRET_KEY_ENV],
            port=int(env.get("SNAPSHOT_E2E_S3_PORT", DEFAULT_STORE_PORT)),
            console_port=int(
                env.get("SNAPSHOT_E2E_S3_CONSOLE_PORT", DEFAULT_STORE_CONSOLE_PORT)
            ),
        )


def credentials_file(settings: S3Settings) -> str:
    return (
        "[default]\n"
        f"aws_access_key_id = {settings.access_key}\n"
        f"aws_secret_access_key = {settings.secret_key}\n"
    )


def helm_set(endpoint: str) -> list[str]:
    return [
        "storage.type=s3",
        f"storage.s3.endpoint={endpoint}",
        f"storage.s3.region={REGION}",
        f"storage.s3.bucket={BUCKET}",
        f"storage.s3.prefix={PREFIX}",
        f"storage.s3.credentialsSecretRef={CREDENTIALS_SECRET}",
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
        metadata=client.V1ObjectMeta(name=CREDENTIALS_SECRET),
        string_data={
            CREDENTIALS_KEY: credentials_file(settings),
            ROOT_USER_KEY: settings.access_key,
            ROOT_PASSWORD_KEY: settings.secret_key,
        },
    )
    try:
        api.create_namespaced_secret(namespace, secret)
    except client.ApiException as exc:
        if exc.status != 409:
            raise
        api.replace_namespaced_secret(CREDENTIALS_SECRET, namespace, secret)


def host_endpoint(host_ip: str, port: int = DEFAULT_STORE_PORT) -> str:
    try:
        host = f"[{host_ip}]" if ipaddress.ip_address(host_ip).version == 6 else host_ip
    except ValueError:
        host = host_ip
    return f"http://{host}:{port}"


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
        "MINIO_DEFAULT_BUCKETS": BUCKET,
        "MINIO_API_PORT_NUMBER": str(settings.port),
        "MINIO_CONSOLE_PORT_NUMBER": str(settings.console_port),
    }
    login = [
        client.V1EnvVar(
            name=key,
            value_from=client.V1EnvVarSource(
                secret_key_ref=client.V1SecretKeySelector(
                    name=CREDENTIALS_SECRET, key=key
                )
            ),
        )
        for key in (ROOT_USER_KEY, ROOT_PASSWORD_KEY)
    ]
    container = client.V1Container(
        name="store",
        image=STORE_IMAGE,
        env=login + [client.V1EnvVar(name=name, value=value) for name, value in env.items()],
        ports=[
            client.V1ContainerPort(container_port=settings.port),
            client.V1ContainerPort(container_port=settings.console_port),
        ],
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
    parser.add_argument("command", choices=("deploy",))
    parser.parse_args(argv)
    try:
        settings = S3Settings.from_env()
    except ValueError as exc:
        parser.error(str(exc))
    namespace = os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", DEFAULT_NAMESPACE)
    config.load_kube_config(os.environ.get("KUBECONFIG"))
    deploy_store(settings, namespace)
    print("\n".join(helm_set(store_endpoint(namespace, settings.port))))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
