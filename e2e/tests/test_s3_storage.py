# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import re

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import s3

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
        k8s.create_pod(snap.source_pod(config=config, run=run, gpu=False))
        source = snap.wait_for_pod_ready(config.namespace, run.source_pod)
        snap.wait_for_file(config.namespace, run.source_pod, snap.SOURCE_READY)
        checkpoint_observations = snap.wait_for_state_observations(
            config.namespace,
            run.source_pod,
            run.source_token,
            gpu=False,
            minimum=2,
        )
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
        content_uid = content["metadata"]["uid"]

        assert re.fullmatch(r"store-v1-[0-9a-f]{64}", content["spec"]["storage"]["storeID"])
        artifacts = content["status"]["storage"]["artifacts"]
        assert [artifact["containerName"] for artifact in artifacts] == [snap.CONTAINER]

        index = s3.read_json(store, s3.index_key(store, content_uid, snap.CONTAINER))
        assert index["format"] == INDEX_FORMAT
        paths = {entry["path"] for entry in index["files"]}
        assert "manifest.yaml" in paths
        keys = s3.list_keys(store, s3.artifact_root(store, content_uid, snap.CONTAINER))
        root = s3.artifact_root(store, content_uid, snap.CONTAINER)
        assert f"{root}/data/manifest.yaml" in keys
        assert not snap.artifact_root_exists(
            config, source.spec.node_name, content_uid
        ), "the checkpoint must not also be written to the checkpoint volume"

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)

        k8s.create_pod(
            snap.restore_pod(
                config=config, run=run, gpu=False, source_node=source.spec.node_name
            )
        )
        snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "True", "RestoreSucceeded"
        )
        snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)
        snap.assert_restored_state(
            config.namespace,
            run.restore_pod,
            source_token=run.source_token,
            restore_token=run.restore_token,
            checkpoint_observations=checkpoint_observations,
            gpu=False,
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
