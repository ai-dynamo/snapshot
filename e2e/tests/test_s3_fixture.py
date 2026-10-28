# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import configparser
import os

import pytest

from snapshot_e2e import s3
from snapshot_e2e.infra import setup


@pytest.fixture
def settings(monkeypatch: pytest.MonkeyPatch) -> s3.S3Settings:
    for name in list(os.environ):
        if name.startswith("SNAPSHOT_E2E_S3_"):
            monkeypatch.delenv(name)
    monkeypatch.setenv("SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", "http://172.18.0.1:9000")
    return s3.S3Settings.from_env()


def test_credentials_file_is_the_shared_credentials_format(
    settings: s3.S3Settings,
) -> None:
    parser = configparser.ConfigParser()
    parser.read_string(s3.credentials_file(settings))
    assert dict(parser["default"]) == {
        "aws_access_key_id": settings.access_key,
        "aws_secret_access_key": settings.secret_key,
    }


def test_helm_values_select_a_plain_http_path_style_store(
    settings: s3.S3Settings,
) -> None:
    values = dict(item.split("=", 1) for item in s3.helm_set(settings))
    assert values["storage.type"] == "s3"
    assert values["storage.s3.endpoint"] == settings.cluster_endpoint
    assert values["storage.s3.addressing"] == "path"
    assert values["storage.s3.allowHttp"] == "true"
    assert values["storage.s3.credentialsSecretRef"] == settings.credentials_secret
    assert values["operator.bindNewContents"] == "true"


def test_helm_values_pass_through_the_installer_unchanged(
    settings: s3.S3Settings,
) -> None:
    lines = s3.helm_set(settings)
    assert setup.parse_helm_overrides(None, "\n".join(lines)) == lines


def test_helm_values_need_the_cluster_endpoint(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", raising=False)
    with pytest.raises(ValueError, match="SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT"):
        s3.helm_set(s3.S3Settings.from_env())


def test_settings_follow_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_BUCKET", "other")
    monkeypatch.setenv("SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", "http://s3.test:9000")
    settings = s3.S3Settings.from_env()
    assert settings.bucket == "other"
    assert settings.cluster_endpoint == "http://s3.test:9000"


def test_store_ports_follow_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_PORT", "19010")
    monkeypatch.setenv("SNAPSHOT_E2E_S3_CONSOLE_PORT", "19011")
    settings = s3.S3Settings.from_env()
    container = s3.store_deployment(settings, "ns").spec.template.spec.containers[0]
    env = {item.name: item.value for item in container.env}
    assert env["MINIO_API_PORT_NUMBER"] == "19010"
    assert env["MINIO_CONSOLE_PORT_NUMBER"] == "19011"
    assert container.ports[0].container_port == 19010
    assert container.readiness_probe.http_get.port == 19010
    assert s3.host_endpoint("10.0.0.5", settings.port) == "http://10.0.0.5:19010"


def test_store_ports_default_to_the_single_job_ports(
    settings: s3.S3Settings,
) -> None:
    assert (settings.port, settings.console_port) == (19000, 19001)


def test_store_login_follows_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_ACCESS_KEY", "run-access")
    monkeypatch.setenv("SNAPSHOT_E2E_S3_SECRET_KEY", "run-secret-key")
    settings = s3.S3Settings.from_env()
    container = s3.store_deployment(settings, "ns").spec.template.spec.containers[0]
    env = {item.name: item.value for item in container.env}
    assert env["MINIO_ROOT_USER"] == "run-access"
    assert env["MINIO_ROOT_PASSWORD"] == "run-secret-key"
    credentials = s3.credentials_file(settings)
    assert "run-access" in credentials and "run-secret-key" in credentials
    assert "minioadmin" not in credentials
