# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint/restore of an inference framework guide workload over S3.

The same flow as test_frameworks.py -- the guide's source pod serves once,
a PodSnapshot captures it, a restore pod resumes it and serves again -- but
Snapshot is installed with an S3-compatible store (SNAPSHOT_E2E_STORAGE=s3)
instead of the checkpoint volume. Select frameworks with
SNAPSHOT_E2E_FRAMEWORK, as for the PVC test.
"""

from __future__ import annotations

import dataclasses
import os
import re
import time

import pytest

from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import inference
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


S3_PHASE_TIMEOUT_SECONDS = 1800


@pytest.fixture(params=sorted(frameworks.FRAMEWORKS))
def framework(request: pytest.FixtureRequest) -> frameworks.FrameworkSpec:
    name = request.param
    if name not in frameworks.selected_frameworks():
        pytest.skip(f"{name} not selected by SNAPSHOT_E2E_FRAMEWORK")
    return dataclasses.replace(
        frameworks.framework_spec(name),
        checkpoint_timeout_seconds=S3_PHASE_TIMEOUT_SECONDS,
        restore_timeout_seconds=S3_PHASE_TIMEOUT_SECONDS,
    )


@pytest.mark.s3_gpu
@pytest.mark.gpu
@pytest.mark.snapshot_success
def test_framework_checkpoint_restore_over_s3_serves_inference(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    framework: frameworks.FrameworkSpec,
) -> None:
    if os.environ.get("SNAPSHOT_E2E_STORAGE") != "s3":
        pytest.skip("Snapshot is not installed with S3 storage (SNAPSHOT_E2E_STORAGE=s3)")
    framework_image: str | None = None
    source_node: str | None = None
    try:
        framework_image = frameworks.framework_image(framework)
        model_cache = frameworks.SharedModelCache.from_env()
        if model_cache is not None:
            pv, pvc = fw.shared_model_cache_volume(config=config, cache=model_cache)
            snap.ensure_pv(pv)
            snap.ensure_pvc(pvc)
        else:
            guide_pvc = fw.model_cache_pvc(config=config, spec=framework)
            if guide_pvc is not None:
                snap.ensure_pvc(guide_pvc)

        k8s.apply_configmap(config.namespace, fw.app_configmap(config=config, spec=framework))
        k8s.create_pod(
            fw.source_pod(
                config=config,
                run=run,
                spec=framework,
                image=framework_image,
                model_cache=model_cache,
            )
        )
        source = snap.wait_for_pod_ready(
            config.namespace,
            run.source_pod,
            timeout=framework.source_ready_timeout_seconds,
        )
        source_node = source.spec.node_name
        source_main = next(c for c in source.spec.containers if c.name == frameworks.CONTAINER)
        model, revision, _ = frameworks.guide_model(source_main.args)
        prompts = inference.chat_prompts(config.namespace, run.source_pod, model, revision)

        checkpoint_started = time.monotonic()
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
            timeout=framework.checkpoint_timeout_seconds,
        )
        print(f"[{framework.name}] checkpoint to S3: {time.monotonic() - checkpoint_started:.0f}s")
        assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert re.fullmatch(r"store-v1-[0-9a-f]{64}", content["spec"]["storage"]["storeID"])
        artifacts = content["status"]["storage"]["artifacts"]
        assert [artifact["containerName"] for artifact in artifacts] == [frameworks.CONTAINER]
        assert not snap.artifact_root_exists(
            config, source_node, content["metadata"]["uid"]
        ), "the checkpoint must not also be written to the checkpoint volume"

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(
            config.namespace, run.source_pod, timeout=frameworks.POD_DELETE_TIMEOUT_SECONDS
        )

        restore_started = time.monotonic()
        k8s.create_pod(
            fw.restore_pod(
                config=config,
                run=run,
                spec=framework,
                source_node=source_node,
                image=framework_image,
                model_cache=model_cache,
            )
        )
        restored_pod, restored_text = snap.wait_for_restore_traffic_ready(
            config.namespace,
            run.restore_pod,
            ready_file=framework.restore_ready_file,
            error_file=framework.restore_error_file,
            timeout=framework.restore_timeout_seconds,
        )
        print(f"[{framework.name}] restore from S3 to traffic: {time.monotonic() - restore_started:.0f}s")
        assert restored_pod.spec.node_name == source_node
        assert restored_text.strip(), f"{framework.restore_ready_file} is empty"

        inference.verify_chat_answers(config.namespace, run.restore_pod, prompts)
        restore_logs = k8s.pod_logs(config.namespace, run.restore_pod, tail_lines=2000)
        assert "pre-checkpoint output=" not in restore_logs, (
            "the restore container loaded a model itself instead of being restored"
        )
    except Exception:
        try:
            snap.debug_dump_framework(
                config, run, source_node=source_node, image=framework_image
            )
        except Exception as debug_exc:  # noqa: BLE001 - must not mask the original failure
            print(f"framework debug dump failed: {type(debug_exc).__name__}: {debug_exc}")
        raise
