# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The S3-compatible store the S3 e2e suite installs Snapshot against.

The store runs outside the cluster, as a service container on the runner. Tests
reach it at ``endpoint``; the cluster reaches the same store at
``cluster_endpoint``.

    python -m snapshot_e2e.s3 prepare    # bucket, namespace, credentials Secret
    python -m snapshot_e2e.s3 helm-set   # chart values, one KEY=VALUE per line
"""

from __future__ import annotations

import argparse
import json
import os
from dataclasses import dataclass
from typing import Any

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError
from kubernetes import client, config

CREDENTIALS_KEY = "credentials"
DEFAULT_NAMESPACE = "snapshot-e2e"


@dataclass(frozen=True)
class S3Settings:
    endpoint: str
    cluster_endpoint: str
    access_key: str
    secret_key: str
    bucket: str
    prefix: str
    region: str
    credentials_secret: str

    @classmethod
    def from_env(cls) -> "S3Settings":
        env = os.environ.get
        return cls(
            endpoint=env("SNAPSHOT_E2E_S3_ENDPOINT", "http://localhost:9000"),
            cluster_endpoint=env(
                "SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", "http://host.k3d.internal:9000"
            ),
            access_key=env("SNAPSHOT_E2E_S3_ACCESS_KEY", "minioadmin"),
            secret_key=env("SNAPSHOT_E2E_S3_SECRET_KEY", "minioadmin"),
            bucket=env("SNAPSHOT_E2E_S3_BUCKET", "snapshot-e2e"),
            prefix=env("SNAPSHOT_E2E_S3_PREFIX", "snapshots").strip("/"),
            region=env("SNAPSHOT_E2E_S3_REGION", "us-east-1"),
            credentials_secret=env(
                "SNAPSHOT_E2E_S3_CREDENTIALS_SECRET", "snapshot-s3-credentials"
            ),
        )


def credentials_file(settings: S3Settings) -> str:
    return (
        "[default]\n"
        f"aws_access_key_id = {settings.access_key}\n"
        f"aws_secret_access_key = {settings.secret_key}\n"
    )


def helm_set(settings: S3Settings) -> list[str]:
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


def artifact_root(settings: S3Settings, content_uid: str, container: str) -> str:
    parts = [settings.prefix, "artifacts", content_uid, "containers", container]
    return "/".join(part for part in parts if part)


def artifact_prefix(settings: S3Settings, content_uid: str) -> str:
    parts = [settings.prefix, "artifacts", content_uid]
    return "/".join(part for part in parts if part) + "/"


def index_key(settings: S3Settings, content_uid: str, container: str) -> str:
    return f"{artifact_root(settings, content_uid, container)}/index.json"


def s3_client(settings: S3Settings) -> Any:
    return boto3.client(
        "s3",
        endpoint_url=settings.endpoint,
        aws_access_key_id=settings.access_key,
        aws_secret_access_key=settings.secret_key,
        region_name=settings.region,
        config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
    )


def list_keys(settings: S3Settings, prefix: str) -> list[str]:
    paginator = s3_client(settings).get_paginator("list_objects_v2")
    keys: list[str] = []
    for page in paginator.paginate(Bucket=settings.bucket, Prefix=prefix):
        keys.extend(item["Key"] for item in page.get("Contents", []))
    return keys


def read_json(settings: S3Settings, key: str) -> dict[str, Any]:
    body = s3_client(settings).get_object(Bucket=settings.bucket, Key=key)["Body"]
    return json.loads(body.read())


def drop_bucket(settings: S3Settings) -> None:
    s3 = s3_client(settings)
    for key in list_keys(settings, ""):
        s3.delete_object(Bucket=settings.bucket, Key=key)
    s3.delete_bucket(Bucket=settings.bucket)


def ensure_bucket(settings: S3Settings) -> None:
    s3 = s3_client(settings)
    try:
        s3.head_bucket(Bucket=settings.bucket)
    except ClientError as exc:
        if exc.response["Error"]["Code"] not in {"404", "NoSuchBucket", "NotFound"}:
            raise
        s3.create_bucket(Bucket=settings.bucket)


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


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("prepare", "helm-set"))
    args = parser.parse_args(argv)
    settings = S3Settings.from_env()
    if args.command == "helm-set":
        print("\n".join(helm_set(settings)))
        return 0
    config.load_kube_config(os.environ.get("KUBECONFIG"))
    ensure_bucket(settings)
    ensure_credentials_secret(
        settings, os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", DEFAULT_NAMESPACE)
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
