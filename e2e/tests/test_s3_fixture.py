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


@pytest.mark.parametrize(
    ("prefix", "expected"),
    [
        ("snapshots", "snapshots/artifacts/uid-1/containers/main/index.json"),
        ("/snapshots/", "snapshots/artifacts/uid-1/containers/main/index.json"),
        ("", "artifacts/uid-1/containers/main/index.json"),
    ],
)
def test_index_key_is_under_the_configured_prefix_only(
    monkeypatch: pytest.MonkeyPatch, prefix: str, expected: str
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_PREFIX", prefix)
    assert s3.index_key(s3.S3Settings.from_env(), "uid-1", "main") == expected


def test_settings_follow_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_BUCKET", "other")
    monkeypatch.setenv("SNAPSHOT_E2E_S3_CLUSTER_ENDPOINT", "http://s3.test:9000")
    settings = s3.S3Settings.from_env()
    assert settings.bucket == "other"
    assert settings.cluster_endpoint == "http://s3.test:9000"


def test_artifact_prefix_covers_every_container_of_one_content(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_S3_PREFIX", "snapshots")
    settings = s3.S3Settings.from_env()
    prefix = s3.artifact_prefix(settings, "uid-1")
    assert prefix == "snapshots/artifacts/uid-1/"
    assert s3.artifact_root(settings, "uid-1", "main").startswith(prefix)
