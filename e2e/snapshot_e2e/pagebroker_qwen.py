# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Qwen checkpoint and repeated staged restore through the installed Snapshot agent."""

from __future__ import annotations

from dataclasses import dataclass, replace
from datetime import datetime
import math
import os

from kubernetes import client
from kubernetes.client import ApiException
import yaml

from snapshot_e2e import benchmark
from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import inference
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e.workloads import TestRun


RESTORES = 2
OPERATION_TIMEOUT = 600


@dataclass(frozen=True)
class QwenCase:
    name: str
    model: str
    gpu_memory_utilization: float


CASES = (
    QwenCase("qwen3-0.6b", "Qwen/Qwen3-0.6B", 0.30),
    QwenCase("qwen3-8b", "Qwen/Qwen3-8B", 0.80),
)


def gpu_memory_utilization(case: QwenCase) -> float:
    value = float(os.environ.get("SNAPSHOT_E2E_QWEN_GPU_MEMORY_UTILIZATION", case.gpu_memory_utilization))
    if not math.isfinite(value) or not 0 < value <= 1:
        raise ValueError("SNAPSHOT_E2E_QWEN_GPU_MEMORY_UTILIZATION must be in (0, 1]")
    return value


def app_name(run: TestRun) -> str:
    return f"{run.suffix}-app"


def prepare_pod(pod: dict, case: QwenCase, run: TestRun, utilization: float) -> dict:
    main = fw.main_container(pod)
    fw.set_env(main, "SNAPSHOT_MODEL", case.model)
    fw.set_env(main, "VLLM_GPU_MEMORY_UTILIZATION", str(utilization))
    for volume in pod["spec"]["volumes"]:
        if volume.get("configMap", {}).get("name") == "vllm-app":
            volume["configMap"]["name"] = app_name(run)
    return pod


def delete_app(config: k8s.E2EConfig, run: TestRun) -> None:
    try:
        client.CoreV1Api().delete_namespaced_config_map(app_name(run), config.namespace)
    except ApiException as error:
        if error.status != 404:
            raise


def running_containers(pod, names: tuple[str, ...], *, no_restarts: bool = False) -> dict:
    statuses = {status.name: status for status in pod.status.container_statuses or []}
    result = {}
    for name in names:
        status = statuses.get(name)
        assert status and status.ready and status.state.running and status.container_id and status.image_id, (
            f"{name} is not a ready running container with a resolved image"
        )
        assert not no_restarts or status.restart_count == 0, f"{name} restarted during the test"
        result[name] = (status.container_id, status.restart_count, status.image_id)
    assert pod.metadata.uid, "pod UID is missing"
    return {"podUID": str(pod.metadata.uid), "containers": result}


def require_staged_config(text: str, agent_started: datetime) -> dict:
    # The agent loads this file once, so a newer projected file needs a rollout.
    modified, document = text.split("\n", 1)
    assert datetime.fromisoformat(modified) <= agent_started, (
        "agent configuration changed after startup; roll out the agent before testing"
    )
    config = yaml.safe_load(document)
    assert config["storage"]["type"] == "pvc", "Qwen smoke requires filesystem PVC storage"
    broker = config["pageBroker"]
    assert broker["restoreMode"] == "staged", "set pageBroker.restoreMode=staged and roll out the agent"
    assert broker.get("transferEngine", "posix-copy") == "posix-copy", (
        "remove the obsolete filesystem Model Streamer setting and roll out the agent"
    )
    return {"storageType": "pvc", "restoreMode": "staged", "transferEngine": "posix-copy"}


def agent_baseline(config: k8s.E2EConfig, node: str) -> tuple[str, dict, dict]:
    name = snap.checkpoint_agent_pod(config, node)
    pod = k8s.read_pod(config.namespace, name)
    identity = running_containers(pod, ("agent", "pagebroker"))
    broker = next(container for container in pod.spec.containers if container.name == "pagebroker")
    assert not any(arg.startswith("--storage-config") for arg in broker.args or []), (
        "Qwen smoke requires the filesystem PageBroker, not the standalone S3 Job"
    )
    started = next(status.state.running.started_at for status in pod.status.container_statuses if status.name == "agent")
    text = k8s.exec_command(config.namespace, name,
                           "set -eu; stat -Lc '%y' /etc/snapshot/config.yaml; cat /etc/snapshot/config.yaml",
                           container="agent")
    settings = require_staged_config(text, started)
    return name, identity, settings


def assert_agent_unchanged(config: k8s.E2EConfig, name: str, baseline: dict) -> None:
    actual = running_containers(k8s.read_pod(config.namespace, name), ("agent", "pagebroker"))
    assert actual == baseline, "agent or PageBroker was replaced or restarted during the Qwen case"


def exercise(config: k8s.E2EConfig, run: TestRun, case: QwenCase,
             result: benchmark.BenchmarkRecorder) -> None:
    spec = replace(frameworks.FRAMEWORKS["vllm"], model=case.model, restore_timeout_seconds=OPERATION_TIMEOUT)
    image = frameworks.framework_image(spec)
    utilization = gpu_memory_utilization(case)
    source_node = None
    result.update_environment(model=case.model, frameworkImage=image, gpuMemoryUtilization=utilization,
                              expectedRestores=RESTORES, completedRestores=0)
    result.define_duration("checkpoint.duration", "Checkpoint (API to Ready)")
    for number in range(1, RESTORES + 1):
        result.define_duration(f"restore.{number}.duration", f"Restore {number} (Pod creation to inference)")
    try:
        cache = frameworks.SharedModelCache.from_env()
        result.update_environment(modelCacheMode="shared-nfs" if cache else "framework-default")
        if cache:
            pv, pvc = fw.shared_model_cache_volume(config=config, cache=cache)
            snap.ensure_pv(pv)
            snap.ensure_pvc(pvc)
        app = fw.app_configmap(config=config, spec=spec)
        app["metadata"]["name"] = app_name(run)
        app["metadata"]["labels"] = run.labels
        k8s.apply_configmap(config.namespace, app)
        source_manifest = prepare_pod(fw.source_pod(config=config, run=run, spec=spec,
                                     image=image, model_cache=cache), case, run, utilization)
        k8s.create_pod(source_manifest)
        source = snap.wait_for_pod_ready(config.namespace, run.source_pod,
                                        timeout=frameworks.SOURCE_READY_TIMEOUT_SECONDS)
        source_node = source.spec.node_name
        assert source_node, "source pod has no assigned node"
        result.redact(source_node, "source-node")
        source_identity = running_containers(source, ("main",), no_restarts=True)
        source_image = source_identity["containers"]["main"][2]
        result.update_environment(frameworkImageDigest=source_image)
        agent, baseline, settings = agent_baseline(config, source_node)
        result.redact(agent, "agent-pod")
        result.update_environment(**settings, agentImage=baseline["containers"]["agent"][2],
                                  pagebrokerImage=baseline["containers"]["pagebroker"][2])
        assert "vLLM pre-checkpoint output=" in k8s.pod_logs(config.namespace, run.source_pod, tail_lines=2000), (
            "source did not report inference before checkpoint"
        )
        result.mark_event("source.ready")
        result.start_duration("checkpoint.duration", event="checkpoint.requested")
        snap.create_podsnapshot(config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid)
        snapshot, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name,
                                                         timeout=OPERATION_TIMEOUT)
        assert snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert content["spec"]["source"]["nodeName"] == source_node
        result.finish_duration("checkpoint.duration", event="checkpoint.ready")
        assert_agent_unchanged(config, agent, baseline)
        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod,
                                 timeout=frameworks.POD_DELETE_TIMEOUT_SECONDS)
        restored_uids = set()
        for number in range(1, RESTORES + 1):
            prefix = f"restore.{number}"
            restore = prepare_pod(fw.restore_pod(config=config, run=run, spec=spec, source_node=source_node,
                                  image=image, model_cache=cache), case, run, utilization)
            result.start_duration(f"{prefix}.duration", event=f"{prefix}.requested")
            created = k8s.create_pod(restore)
            assert created.metadata.uid and created.metadata.uid not in restored_uids | {source.metadata.uid}, (
                "restore must create a fresh pod"
            )
            restored_uids.add(created.metadata.uid)
            restored, text = snap.wait_for_restore_traffic_ready(
                config.namespace, run.restore_pod, ready_file=spec.restore_ready_file,
                error_file=spec.restore_error_file, timeout=spec.restore_timeout_seconds)
            assert restored.metadata.uid == created.metadata.uid, "restore pod was replaced"
            restored = snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=60)
            assert restored.metadata.uid == created.metadata.uid, "restore pod was replaced"
            assert restored.spec.node_name == source_node, "restore ran on a different node"
            identity = running_containers(restored, ("main",), no_restarts=True)
            assert identity["containers"]["main"][2] == source_image, "restore used a different workload image"
            assert text.strip(), "restored process produced an empty readiness result"
            for prompt in (frameworks.PROMPT, "Reply with one short word: ready"):
                answer = inference.request_generate(config.namespace, run.restore_pod, prompt)
                assert isinstance(answer, str) and answer.strip(), "restored inference returned empty text"
                print(f"[{case.name} restore {number}] /generate: {answer!r}")
            logs = k8s.pod_logs(config.namespace, run.restore_pod, tail_lines=2000)
            assert "pre-checkpoint output=" not in logs, "restore placeholder cold-started the model"
            assert not logs.startswith("<logs unavailable:"), "cannot verify restore placeholder logs"
            assert running_containers(k8s.read_pod(config.namespace, run.restore_pod), ("main",),
                                      no_restarts=True) == identity, "restored workload changed during inference"
            assert_agent_unchanged(config, agent, baseline)
            result.finish_duration(f"{prefix}.duration", event=f"{prefix}.inference_verified")
            result.update_environment(completedRestores=number)
            k8s.delete_pod(config.namespace, run.restore_pod)
            snap.wait_for_pod_deleted(config.namespace, run.restore_pod,
                                     timeout=frameworks.POD_DELETE_TIMEOUT_SECONDS)
        result.record_measurement("restores.completed", "Verified restores", "count", len(restored_uids))
        result.finish_test()
    except Exception:
        result.finish_test()
        try:
            snap.debug_dump_framework(config, run, source_node=source_node, image=image)
        except Exception as error:
            print(f"Qwen failure diagnostics unavailable: {error}")
        raise
