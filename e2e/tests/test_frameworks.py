# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint/restore e2e for the inference framework guides.

One test per framework (vLLM, SGLang, TensorRT-LLM), all with the same shape,
driven by the guide's own program and manifests (see framework_workloads):

1. The source pod loads the model, generates once, records the text in the
   guide's precheck file, pauses the engine, and writes ready-for-snapshot.
   The guide's readiness probe is `cat ready-for-snapshot`, so Ready means
   checkpointable, and the precheck file existing before the PodSnapshot is
   created proves the engine served before capture.
2. A PodSnapshot captures it. The dump terminates the source process.
3. A restore pod built from the guide's restore manifest is pinned to the
   source node. Its own entrypoint stays inert (SNAPSHOT_RESTORE_STANDBY=1);
   the agent restores the checkpointed process into it, which resumes the
   engine, generates again, and serves /generate.
4. The test asserts the restore condition, the restore-ready file, a live
   /generate answer, and that the placeholder never loaded a model itself —
   a restore that silently degraded to a cold start must not pass.

Select frameworks with SNAPSHOT_E2E_FRAMEWORK=vllm[,sglang,...]; CI runs one
per matrix job. Point SNAPSHOT_E2E_FRAMEWORK_IMAGE at a local build to test an
unpublished guide change.
"""

from __future__ import annotations

import pytest

from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import inference
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


@pytest.fixture(params=sorted(frameworks.FRAMEWORKS))
def framework(request: pytest.FixtureRequest) -> frameworks.FrameworkSpec:
    name = request.param
    if name not in frameworks.selected_frameworks():
        pytest.skip(f"{name} not selected by SNAPSHOT_E2E_FRAMEWORK")
    return frameworks.FRAMEWORKS[name]


@pytest.mark.framework
@pytest.mark.gpu
def test_framework_checkpoint_restore_serves_inference(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    framework: frameworks.FrameworkSpec,
) -> None:
    source_node: str | None = None
    try:
        # Shared NFS cache when configured (offline, no download); otherwise the
        # guide's own cache plumbing, which downloads from Hugging Face.
        model_cache = frameworks.SharedModelCache.from_env()
        if model_cache is not None:
            pv, pvc = fw.shared_model_cache_volume(config=config, cache=model_cache)
            snap.ensure_pv(pv)
            snap.ensure_pvc(pvc)
        else:
            guide_pvc = fw.model_cache_pvc(config=config, spec=framework)
            if guide_pvc is not None:
                snap.ensure_pvc(guide_pvc)

        # --- source: load, generate, pause, ready-for-snapshot -------------
        k8s.create_pod(
            fw.source_pod(config=config, run=run, spec=framework, model_cache=model_cache)
        )
        source = snap.wait_for_pod_ready(
            config.namespace,
            run.source_pod,
            timeout=frameworks.SOURCE_READY_TIMEOUT_SECONDS,
        )
        source_node = source.spec.node_name

        precheck = inference.read_control_file(
            config.namespace, run.source_pod, framework.precheck_file
        ).strip()
        assert precheck, f"{framework.precheck_file} is empty: engine did not generate before capture"
        print(f"[{framework.name}] pre-checkpoint generation: {precheck!r}")

        # --- checkpoint -----------------------------------------------------
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
            timeout=frameworks.CHECKPOINT_TIMEOUT_SECONDS,
        )
        assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert content["spec"]["source"]["nodeName"] == source_node

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(
            config.namespace, run.source_pod, timeout=frameworks.POD_DELETE_TIMEOUT_SECONDS
        )

        # --- restore --------------------------------------------------------
        k8s.create_pod(
            fw.restore_pod(
                config=config,
                run=run,
                spec=framework,
                source_node=source_node,
                model_cache=model_cache,
            )
        )
        snap.wait_for_restored_condition(
            config.namespace,
            run.restore_pod,
            "True",
            "RestoreSucceeded",
            timeout=frameworks.RESTORE_TIMEOUT_SECONDS,
        )
        snap.wait_for_file(
            config.namespace,
            run.restore_pod,
            framework.restore_ready_file,
            timeout=frameworks.RESTORE_TIMEOUT_SECONDS,
        )
        restored_text = inference.read_control_file(
            config.namespace, run.restore_pod, framework.restore_ready_file
        ).strip()
        assert restored_text, f"{framework.restore_ready_file} is empty"
        print(f"[{framework.name}] first post-restore generation: {restored_text!r}")

        # --- inference after restore ---------------------------------------
        answer = inference.request_generate(config.namespace, run.restore_pod, frameworks.PROMPT)
        print(f"[{framework.name}] /generate after restore: {answer!r}")

        # The placeholder's own entrypoint must have stayed in standby. If it
        # had loaded a model, its log would show the pre-checkpoint line and
        # the "restore" would be an ordinary cold start wearing a Restored
        # condition.
        restore_logs = k8s.pod_logs(config.namespace, run.restore_pod, tail_lines=2000)
        assert "pre-checkpoint output=" not in restore_logs, (
            "restore placeholder loaded a model itself instead of staying in standby"
        )
    except Exception:
        snap.debug_dump_framework(config, run, source_node=source_node)
        raise
