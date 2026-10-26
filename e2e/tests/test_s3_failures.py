# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import s3, s3_lifecycle


@pytest.fixture
def store() -> s3.S3Settings:
    return s3.S3Settings.from_env()


@pytest.mark.s3
@pytest.mark.snapshot_failure
def test_capture_fails_without_the_bucket_and_publishes_nothing(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    store: s3.S3Settings,
) -> None:
    try:
        source, _, _ = s3_lifecycle.start_source(config, run)
        s3.drop_bucket(store)
        try:
            snap.create_podsnapshot(
                config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
            )
            pod_snapshot, content = snap.wait_for_snapshot_failed(
                config.namespace, run.snapshot_name, timeout=900
            )
        finally:
            s3.ensure_bucket(store)

        ready = snap.condition(pod_snapshot, "Ready")
        assert ready is None or ready.get("status") != "True"
        failed = snap.condition(pod_snapshot, "Failed")
        assert failed and failed.get("reason")
        assert store.secret_key not in (failed.get("message") or "")
        if content:
            assert not content.get("status", {}).get("storage")
            assert not s3.list_keys(
                store, s3.artifact_prefix(store, content["metadata"]["uid"])
            )
    except Exception:
        snap.debug_dump(config, run)
        raise
