# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint/restore e2e for the inference framework guides.

One test per framework (vLLM, SGLang, TensorRT-LLM), all with the same shape,
driven by the guide's own program and manifests (see framework_workloads):

1. The source pod loads the model, generates once, pauses the engine, and
   writes ready-for-snapshot. The generation is synchronous and precedes the
   sentinel, so Ready means the engine served before capture and the process
   is checkpointable.
2. A PodSnapshot captures it. The dump terminates the source process.
3. A restore pod built from the guide's restore manifest is pinned to the
   source node. Its own entrypoint stays inert (`sleep infinity`); the agent
   restores the checkpointed process into it, which resumes the engine,
   generates again, and serves /generate.
4. The test asserts the restore condition, the restore-ready file, a live
   /generate answer, and that the placeholder never loaded a model itself —
   a restore that silently degraded to a cold start must not pass.

Select frameworks with SNAPSHOT_E2E_FRAMEWORK=vllm[,sglang,...]; CI runs one
per matrix job. Point SNAPSHOT_E2E_FRAMEWORK_IMAGE at a local build to test an
unpublished guide change.
"""

from __future__ import annotations

import os

import pytest

from snapshot_e2e import benchmark as benchmark_result
from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import inference
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap


CHECKPOINT_DURATION = "checkpoint.duration"
RESTORE_TO_TRAFFIC_DURATION = "restore.to_traffic.duration"
RESTORE_POD_TO_TRAFFIC_DURATION = "restore.pod_create_to_traffic.duration"


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
    benchmark: benchmark_result.BenchmarkSession,
) -> None:
    result = benchmark.start(
        suite="framework-checkpoint-restore",
        case=framework.name,
        environment={
            "namespace": config.namespace,
            "model": framework.model,
            "storageClass": os.environ.get(
                "SNAPSHOT_E2E_STORAGE_CLASS", "cluster-default"
            ),
            "modelCacheMode": "unknown",
            "datadogGpuMonitoringMode": os.environ.get(
                "SNAPSHOT_E2E_DATADOG_GPU_MONITORING", "unknown"
            ),
            "gpus": [],
        },
    )
    result.define_duration(CHECKPOINT_DURATION, "Checkpoint")
    result.define_duration(RESTORE_TO_TRAFFIC_DURATION, "Restore to traffic")
    result.define_duration(
        RESTORE_POD_TO_TRAFFIC_DURATION,
        "Restore pod create to traffic",
    )
    framework_image: str | None = None
    source_node: str | None = None
    try:
        framework_image = frameworks.framework_image(framework)
        result.update_environment(frameworkImage=framework_image)
        # Shared NFS cache when configured (offline, no download); otherwise the
        # guide's own cache plumbing, which downloads from Hugging Face.
        model_cache = frameworks.SharedModelCache.from_env()
        result.update_environment(
            modelCacheMode="shared-nfs" if model_cache is not None else "framework-default"
        )
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
            timeout=frameworks.SOURCE_READY_TIMEOUT_SECONDS,
        )
        source_node = source.spec.node_name
        result.mark_event("source.ready")
        result.update_environment(node=source_node)
        _record_gpu_environment(result, config.namespace, run.source_pod)
        # Recorded on success too, so a flaky restore failure can be correlated
        # with whether Datadog GPU monitoring was active on the node.
        print(
            f"[{framework.name}] host monitoring agents on {source_node}:\n"
            f"{snap.host_monitoring_agents(config, source_node)}"
        )

        result.start_duration(CHECKPOINT_DURATION, event="checkpoint.requested")
        snap.create_podsnapshot(
            config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
        )
        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
            timeout=frameworks.CHECKPOINT_TIMEOUT_SECONDS,
        )
        result.finish_duration(CHECKPOINT_DURATION, event="checkpoint.ready")
        assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert content["spec"]["source"]["nodeName"] == source_node

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(
            config.namespace, run.source_pod, timeout=frameworks.POD_DELETE_TIMEOUT_SECONDS
        )

        result.start_duration(
            RESTORE_POD_TO_TRAFFIC_DURATION,
            event="restore.pod_create.requested",
        )
        restore_pod = k8s.create_pod(
            fw.restore_pod(
                config=config,
                run=run,
                spec=framework,
                source_node=source_node,
                image=framework_image,
                model_cache=model_cache,
            )
        )
        result.mark_event("restore.pod_created")
        snap.wait_for_pod_event(
            config.namespace,
            run.restore_pod,
            "RestoreRequested",
            pod_uid=str(restore_pod.metadata.uid),
            timeout=framework.restore_timeout_seconds,
        )
        result.start_duration(RESTORE_TO_TRAFFIC_DURATION, event="restore.requested")
        snap.wait_for_restored_condition(
            config.namespace,
            run.restore_pod,
            "True",
            "RestoreSucceeded",
            timeout=framework.restore_timeout_seconds,
        )
        result.mark_event("restore.succeeded")
        restored_text = snap.wait_for_restore_outcome(
            config.namespace,
            run.restore_pod,
            ready_file=framework.restore_ready_file,
            error_file=framework.restore_error_file,
            timeout=framework.restore_timeout_seconds,
        ).strip()
        result.finish_durations(
            [RESTORE_TO_TRAFFIC_DURATION, RESTORE_POD_TO_TRAFFIC_DURATION],
            event="traffic.ready",
        )
        assert restored_text, f"{framework.restore_ready_file} is empty"
        print(f"[{framework.name}] first post-restore generation: {restored_text!r}")

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
        result.mark_event("inference.verified")
        result.finish_test()
    except Exception:
        # End the functional-test timer before diagnostics, which can take
        # minutes and are not part of the benchmark definition.
        result.finish_test()
        try:
            snap.debug_dump_framework(
                config, run, source_node=source_node, image=framework_image
            )
        except Exception as debug_exc:  # noqa: BLE001 - must not mask the original failure
            print(f"framework debug dump failed: {type(debug_exc).__name__}: {debug_exc}")
        raise


def _record_gpu_environment(
    result: benchmark_result.BenchmarkRecorder,
    namespace: str,
    pod: str,
) -> None:
    """Records the GPU visible to the workload without failing the e2e test."""
    try:
        output = k8s.exec_command(
            namespace,
            pod,
            f"{benchmark_result.NVIDIA_SMI_QUERY} 2>/dev/null",
        )
        result.update_environment(gpus=benchmark_result.parse_nvidia_smi_csv(output))
    except Exception as exc:  # noqa: BLE001 - metadata is not a functional assertion
        message = f"{type(exc).__name__}: {exc}"
        result.update_environment(gpus=[], gpuCollectionError=message)
        print(f"benchmark GPU metadata unavailable: {message}")
