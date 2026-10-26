# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import json

import pytest
from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import s3_lifecycle

MODEL_STREAMER_FILE = "model-streamer.json"
MODEL_STREAMER_KEY = "pageBroker.transferEngine.modelStreamer"
UNKNOWN_KEY_MESSAGE = "invalid Model Streamer configuration: unknown field"


@pytest.fixture
def restore_release(config: k8s.E2EConfig):
    revision = s3_lifecycle.release_revision(config)
    yield
    s3_lifecycle.rollback_chart(config, revision)


def storage_config_map(config: k8s.E2EConfig) -> dict[str, str]:
    api = client.CoreV1Api()
    for item in api.list_namespaced_config_map(config.namespace).items:
        if MODEL_STREAMER_FILE in (item.data or {}):
            return item.data
    return {}


@pytest.mark.s3
@pytest.mark.serial
@pytest.mark.snapshot_success
def test_model_streamer_settings_reach_pagebroker_and_restore_still_works(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    restore_release: None,
) -> None:
    try:
        s3_lifecycle.upgrade_chart(
            config,
            [
                f"{MODEL_STREAMER_KEY}.s3Reader.concurrency=2",
                f"{MODEL_STREAMER_KEY}.logging.toStderr=true",
            ],
        )
        rendered = json.loads(storage_config_map(config)[MODEL_STREAMER_FILE])
        assert rendered["s3Reader"]["concurrency"] == 2
        assert rendered["logging"]["toStderr"] is True

        captured = s3_lifecycle.capture(config, run)
        s3_lifecycle.restore_and_verify(config, run, captured)
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.s3
@pytest.mark.serial
@pytest.mark.snapshot_failure
def test_unknown_model_streamer_key_stops_pagebroker_from_starting(
    config: k8s.E2EConfig,
    restore_release: None,
) -> None:
    s3_lifecycle.upgrade_chart(config, [f"{MODEL_STREAMER_KEY}.notASetting=1"], wait=False)

    def refused() -> str | None:
        for pod in k8s.list_snapshot_pods(
            config.namespace, config.release, "snapshot-agent"
        ):
            status = next(
                (
                    item
                    for item in pod.status.container_statuses or []
                    if item.name == s3_lifecycle.PAGEBROKER_CONTAINER
                ),
                None,
            )
            if status and status.restart_count >= 1:
                logs = s3_lifecycle.previous_container_logs(
                    config.namespace, pod.metadata.name, s3_lifecycle.PAGEBROKER_CONTAINER
                )
                if UNKNOWN_KEY_MESSAGE in logs:
                    return logs
        return None

    logs = snap.wait_for(
        "PageBroker to refuse the unknown Model Streamer key",
        refused,
        timeout=300,
        poll_interval=5,
    )
    assert UNKNOWN_KEY_MESSAGE in logs
