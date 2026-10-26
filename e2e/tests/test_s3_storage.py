# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import re

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import s3, s3_lifecycle

INDEX_FORMAT = "snapshot.s3-checkpoint/v1"


@pytest.fixture
def store() -> s3.S3Settings:
    return s3.S3Settings.from_env()


@pytest.mark.s3
@pytest.mark.snapshot_success
def test_checkpoint_is_published_to_s3_and_restores_from_it(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    store: s3.S3Settings,
) -> None:
    try:
        captured = s3_lifecycle.capture(config, run)
        content = captured.content

        assert re.fullmatch(r"store-v1-[0-9a-f]{64}", content["spec"]["storage"]["storeID"])
        artifacts = content["status"]["storage"]["artifacts"]
        assert [artifact["containerName"] for artifact in artifacts] == [snap.CONTAINER]

        root = s3.artifact_root(store, captured.content_uid, snap.CONTAINER)
        index = s3.read_json(store, s3.index_key(store, captured.content_uid, snap.CONTAINER))
        assert index["format"] == INDEX_FORMAT
        assert "manifest.yaml" in {entry["path"] for entry in index["files"]}
        assert f"{root}/data/manifest.yaml" in s3.list_keys(store, root)
        assert not snap.artifact_root_exists(
            config, captured.node, captured.content_uid
        ), "the checkpoint must not also be written to the checkpoint volume"

        s3_lifecycle.restore_and_verify(config, run, captured)
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.s3
@pytest.mark.snapshot_success
def test_deleting_a_snapshot_removes_its_objects_from_the_store(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    store: s3.S3Settings,
) -> None:
    try:
        captured = s3_lifecycle.capture(config, run)
        prefix = s3.artifact_prefix(store, captured.content_uid)
        assert s3.list_keys(store, prefix)
        content_name = captured.content["metadata"]["name"]

        snap.delete_podsnapshot(config.namespace, run.snapshot_name)
        snap.wait_for_custom_object_deleted(
            config.namespace, run.snapshot_name, snap.PODSNAPSHOTS
        )
        snap.wait_for_custom_object_deleted(
            None, content_name, snap.PODSNAPSHOTCONTENTS
        )
        snap.wait_for(
            f"objects under {prefix} removed",
            lambda: True if not s3.list_keys(store, prefix) else None,
            timeout=180,
            poll_interval=5,
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
