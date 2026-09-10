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
RESTORE_AGENT_TO_TRAFFIC_DURATION = "restore.agent_complete_to_traffic.duration"
RESTORE_AGENT_TO_TRAFFIC_DISPLAY_NAME = "Agent restore complete to traffic ready"
GPU_PRODUCT_NODE_LABEL = "nvidia.com/gpu.product"


def _phase_display_name(phase: str) -> str:
    acronyms = {"criu": "CRIU", "cuda": "CUDA", "gpu": "GPU"}
    return " ".join(acronyms.get(word, word) for word in phase.split("_"))


def agent_duration(operation: str, phase: str | None = None) -> tuple[str, str]:
    """Measurement name and display name for an agent total or phase."""
    title = operation.capitalize()
    if phase is None:
        return f"{operation}.agent.duration", f"{title} (agent)"
    return f"{operation}.{phase}.duration", f"{title} {_phase_display_name(phase)}"


def image_pull_duration(role: str, *, including_wait: bool) -> tuple[str, str]:
    title = role.capitalize()
    if including_wait:
        return (
            f"{role}.image_pull_including_wait.duration",
            f"{title} image pull including wait",
        )
    return f"{role}.image_pull.duration", f"{title} image pull"


EXPECTED_COLLECTED_DURATIONS = dict(
    [
        agent_duration("checkpoint"),
        agent_duration("checkpoint", "criu_dump"),
        agent_duration("restore"),
        agent_duration("restore", "criu_restore"),
        (RESTORE_AGENT_TO_TRAFFIC_DURATION, RESTORE_AGENT_TO_TRAFFIC_DISPLAY_NAME),
        image_pull_duration("source", including_wait=False),
        image_pull_duration("source", including_wait=True),
        image_pull_duration("restore", including_wait=False),
        image_pull_duration("restore", including_wait=True),
    ]
)


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
            "sourceGpus": [],
            "restoreGpus": [],
        },
    )
    result.define_duration(CHECKPOINT_DURATION, "Checkpoint (API to Ready)")
    result.define_duration(
        RESTORE_TO_TRAFFIC_DURATION,
        "Restore requested to traffic ready",
    )
    result.define_duration(
        RESTORE_POD_TO_TRAFFIC_DURATION,
        "Restore pod create to traffic",
    )
    for name, display_name in EXPECTED_COLLECTED_DURATIONS.items():
        result.define_duration(name, display_name)
    framework_image: str | None = None
    source_node: str | None = None
    try:
        _record_storage_environment(result, config)
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
        result.update_environment(sourceNode=source_node)
        _record_framework_image_digest(result, source)
        _record_gpu_environment(
            result,
            config.namespace,
            run.source_pod,
            role="source",
            node=source_node,
        )
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
        restore_requested = snap.wait_for_pod_event(
            config.namespace,
            run.restore_pod,
            "RestoreRequested",
            pod_uid=str(restore_pod.metadata.uid),
            timeout=framework.restore_timeout_seconds,
        )
        _start_duration_from_pod_event(
            result,
            RESTORE_TO_TRAFFIC_DURATION,
            "restore.requested",
            restore_requested,
        )
        restored_pod, restored_text = snap.wait_for_restore_traffic_ready(
            config.namespace,
            run.restore_pod,
            ready_file=framework.restore_ready_file,
            error_file=framework.restore_error_file,
            timeout=framework.restore_timeout_seconds,
            on_restore_succeeded=lambda: result.mark_event("restore.succeeded"),
            on_traffic_ready=lambda: result.finish_durations(
                [RESTORE_TO_TRAFFIC_DURATION, RESTORE_POD_TO_TRAFFIC_DURATION],
                event="traffic.ready",
            ),
        )
        restored_text = restored_text.strip()
        restore_node = restored_pod.spec.node_name
        result.update_environment(restoreNode=restore_node)
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
        # Metadata collection is deliberately outside test.total.duration so
        # extra exec/log calls do not inflate the functional benchmark.
        _record_gpu_environment(
            result,
            config.namespace,
            run.restore_pod,
            role="restore",
            node=restore_node,
        )
        _record_image_pulls(
            result,
            namespace=config.namespace,
            source_pod_uid=str(source.metadata.uid),
            restore_pod_uid=str(restored_pod.metadata.uid),
        )
        _record_agent_timings(
            result,
            config=config,
            source_node=source_node,
            restore_node=restore_node,
            snapshot_name=run.snapshot_name,
            content_name=content["metadata"]["name"],
            content_uid=content["metadata"]["uid"],
        )
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
    *,
    role: str,
    node: str | None,
) -> None:
    """Records the GPU visible to the workload without failing the e2e test.

    `nvidia-smi` inside the workload is the preferred source. The node's
    `nvidia.com/gpu.product` label is recorded alongside it and stands in as
    the GPU model when the exec fails, so the result stays comparable.
    """
    if role not in {"source", "restore"}:
        raise ValueError(f"unknown GPU role {role!r}")
    gpu_product = _node_gpu_product(result, role=role, node=node)
    try:
        output = k8s.exec_command(
            namespace,
            pod,
            f"{benchmark_result.NVIDIA_SMI_QUERY} 2>/dev/null",
        )
        result.update_environment(
            **{f"{role}Gpus": benchmark_result.parse_nvidia_smi_csv(output)}
        )
    except Exception as exc:  # noqa: BLE001 - metadata is not a functional assertion
        message = f"{type(exc).__name__}: {exc}"
        fallback = (
            [
                {
                    "model": gpu_product,
                    "uuid": "unknown",
                    "driverVersion": "unknown",
                    "source": GPU_PRODUCT_NODE_LABEL,
                }
            ]
            if gpu_product
            else []
        )
        result.update_environment(
            **{f"{role}Gpus": fallback, f"{role}GpuCollectionError": message}
        )
        print(f"benchmark {role} GPU metadata unavailable: {message}")


def _node_gpu_product(
    result: benchmark_result.BenchmarkRecorder,
    *,
    role: str,
    node: str | None,
) -> str | None:
    try:
        if not node:
            raise ValueError("pod has no node")
        labels = k8s.read_node(node).metadata.labels or {}
        product = labels.get(GPU_PRODUCT_NODE_LABEL)
        if not product:
            raise ValueError(f"node {node} has no {GPU_PRODUCT_NODE_LABEL} label")
        result.update_environment(**{f"{role}NodeGpuProduct": product})
        return product
    except Exception as exc:  # noqa: BLE001 - metadata is not a functional assertion
        _record_environment_error(result, "gpuProductCollectionErrors", role, exc)
        return None


def _record_framework_image_digest(
    result: benchmark_result.BenchmarkRecorder,
    pod: object,
) -> None:
    """Records the resolved image digest so baselines survive re-pushed tags."""
    try:
        statuses = getattr(getattr(pod, "status", None), "container_statuses", None) or []
        status = next(item for item in statuses if item.name == frameworks.CONTAINER)
        if not status.image_id:
            raise ValueError(f"container {frameworks.CONTAINER} reports no imageID")
        result.update_environment(frameworkImageDigest=status.image_id)
    except Exception as exc:  # noqa: BLE001 - metadata is not a functional assertion
        _record_environment_error(result, "frameworkImageCollectionErrors", "digest", exc)


def _record_storage_environment(
    result: benchmark_result.BenchmarkRecorder,
    config: k8s.E2EConfig,
) -> None:
    """Records the bound checkpoint PVC and its provisioner best-effort."""
    try:
        pvc = k8s.read_pvc(config.namespace, config.pvc_name)
        storage_class_name = pvc.spec.storage_class_name
        if not storage_class_name:
            raise ValueError(f"PVC {config.namespace}/{config.pvc_name} has no storage class")
        storage_class = k8s.read_storage_class(storage_class_name)
        parameters = benchmark_result.public_storage_parameters(
            storage_class.parameters
        )
        storage_type = next(
            (
                parameters[key]
                for key in ("skuName", "type", "storageType")
                if parameters.get(key)
            ),
            storage_class.provisioner,
        )
        requests = pvc.spec.resources.requests or {}
        capacity = pvc.status.capacity or {}
        result.update_environment(
            storageClass=storage_class_name,
            storage={
                "storageClass": storage_class_name,
                "type": storage_type,
                "provisioner": storage_class.provisioner,
                "parameters": parameters,
                "requestedSize": str(requests.get("storage", "unknown")),
                "capacity": str(capacity.get("storage", "unknown")),
                "accessModes": list(pvc.spec.access_modes or []),
                "volumeMode": pvc.spec.volume_mode,
            },
        )
    except Exception as exc:  # noqa: BLE001 - metadata is not a functional assertion
        message = f"{type(exc).__name__}: {exc}"
        result.update_environment(storageCollectionError=message)
        print(f"benchmark storage metadata unavailable: {message}")


def _start_duration_from_pod_event(
    result: benchmark_result.BenchmarkRecorder,
    measurement: str,
    event_name: str,
    event: object,
) -> None:
    """Uses event emission time, falling back to observation time if absent."""
    try:
        timestamp = snap.pod_event_timestamp(event)
    except ValueError as exc:
        _record_environment_error(result, "timingBoundaryCollectionErrors", event_name, exc)
        result.start_duration(measurement, event=event_name)
    else:
        result.start_duration_at(measurement, timestamp, event=event_name)


def _record_image_pull(
    result: benchmark_result.BenchmarkRecorder,
    *,
    role: str,
    events: list[object],
    pod_uid: str,
) -> None:
    """Records kubelet-reported image pull time, including cache hits."""
    try:
        pull = benchmark_result.parse_image_pull_events(events, pod_uid=pod_uid)
        pulls = dict(result.environment.get("imagePulls", {}))
        pulls[role] = {
            "cacheHit": pull.cache_hit,
            "imageSizeBytes": pull.image_size_bytes,
            "includingWaitSeconds": pull.including_wait_seconds,
        }
        result.update_environment(imagePulls=pulls)
        result.record_measurement(
            *image_pull_duration(role, including_wait=False),
            "seconds",
            pull.duration_seconds,
        )
        result.record_measurement(
            *image_pull_duration(role, including_wait=True),
            "seconds",
            pull.including_wait_seconds,
        )
    except Exception as exc:  # noqa: BLE001 - instrumentation must not fail e2e
        _record_environment_error(result, "imagePullCollectionErrors", role, exc)


def _record_image_pulls(
    result: benchmark_result.BenchmarkRecorder,
    *,
    namespace: str,
    source_pod_uid: str,
    restore_pod_uid: str,
) -> None:
    try:
        events = k8s.list_events(namespace)
    except Exception as exc:  # noqa: BLE001 - instrumentation must not fail e2e
        for role in ("source", "restore"):
            _record_environment_error(result, "imagePullCollectionErrors", role, exc)
        return
    _record_image_pull(result, role="source", events=events, pod_uid=source_pod_uid)
    _record_image_pull(result, role="restore", events=events, pod_uid=restore_pod_uid)


def _record_agent_timings(
    result: benchmark_result.BenchmarkRecorder,
    *,
    config: k8s.E2EConfig,
    source_node: str,
    restore_node: str,
    snapshot_name: str,
    content_name: str,
    content_uid: str,
) -> None:
    """Adds agent-native phase timings without changing functional outcomes."""
    logs_by_node: dict[str, str] = {}

    def agent_logs(node: str) -> str:
        if node not in logs_by_node:
            agent = snap.checkpoint_agent_pod(config, node)
            logs_by_node[node] = k8s.pod_logs(
                config.namespace,
                agent,
                tail_lines=2000,
                container="agent",
            )
        return logs_by_node[node]

    try:
        checkpoint = benchmark_result.parse_agent_timing_summary(
            agent_logs(source_node),
            message="Checkpoint timing summary",
            field="checkpoint",
            matches={"content": content_name},
        )
        _record_agent_summary(result, "checkpoint", checkpoint)
    except Exception as exc:  # noqa: BLE001 - instrumentation must not fail e2e
        _record_environment_error(result, "agentTimingCollectionErrors", "checkpoint", exc)

    restore: benchmark_result.AgentTimingSummary | None = None
    try:
        restore = benchmark_result.parse_agent_timing_summary(
            agent_logs(restore_node),
            message="Restore timing summary",
            field="restore",
            matches={"snapshot": snapshot_name, "content_uid": content_uid},
        )
        _record_agent_summary(result, "restore", restore)
    except Exception as exc:  # noqa: BLE001 - instrumentation must not fail e2e
        _record_environment_error(result, "agentTimingCollectionErrors", "restore", exc)

    if restore is None:
        return
    try:
        gap = (result.event_time("traffic.ready") - restore.completed_at).total_seconds()
        if gap < -1.0:
            raise ValueError(
                "traffic-ready timestamp precedes the agent restore summary by "
                f"{-gap:.3f}s"
            )
        result.record_measurement(
            RESTORE_AGENT_TO_TRAFFIC_DURATION,
            RESTORE_AGENT_TO_TRAFFIC_DISPLAY_NAME,
            "seconds",
            max(0.0, gap),
        )
    except Exception as exc:  # noqa: BLE001 - instrumentation must not fail e2e
        _record_environment_error(
            result,
            "agentTimingCollectionErrors",
            "restore.agent_complete_to_traffic",
            exc,
        )


def _record_agent_summary(
    result: benchmark_result.BenchmarkRecorder,
    operation: str,
    summary: benchmark_result.AgentTimingSummary,
) -> None:
    result.record_measurement(
        *agent_duration(operation),
        "seconds",
        summary.duration_seconds,
    )
    for phase, duration in summary.phases_seconds.items():
        result.record_measurement(
            *agent_duration(operation, phase),
            "seconds",
            duration,
        )


def _record_environment_error(
    result: benchmark_result.BenchmarkRecorder,
    category: str,
    key: str,
    error: Exception,
) -> None:
    errors = dict(result.environment.get(category, {}))
    message = f"{type(error).__name__}: {error}"
    errors[key] = message
    result.update_environment(**{category: errors})
    print(f"benchmark {key} metadata unavailable: {message}")
