# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import uuid
from dataclasses import dataclass
from typing import Any

import pytest
from snapshot_e2e import k8s, workloads
from snapshot_e2e import lifecycle as snap

DEFAULT_VLLM_IMAGE = "vllm/vllm-openai:v0.27.1"
DEFAULT_VLLM_MODEL = "Qwen/Qwen3-0.6B"


@dataclass(frozen=True)
class VllmCheckpointRun:
    suffix: str
    checkpoint_id: str
    snapshot_name: str
    source_pod: str
    restore_pod: str
    image: str
    model: str

    @classmethod
    def new(cls) -> VllmCheckpointRun:
        suffix = f"vllm-checkpoint-{uuid.uuid4().hex[:6]}"
        return cls(
            suffix=suffix,
            checkpoint_id=suffix,
            snapshot_name=f"{suffix}-snapshot",
            source_pod=f"{suffix}-source",
            restore_pod=f"{suffix}-restore",
            image=os.environ.get("SNAPSHOT_E2E_VLLM_IMAGE", DEFAULT_VLLM_IMAGE),
            model=os.environ.get("SNAPSHOT_E2E_VLLM_MODEL", DEFAULT_VLLM_MODEL),
        )


@pytest.mark.snapshot_success
@pytest.mark.gpu
@pytest.mark.vllm
def test_vllm_checkpoint_captures_quiesced_engine(
    config: k8s.E2EConfig,
) -> None:
    run = VllmCheckpointRun.new()
    try:
        k8s.create_pod(vllm_source_pod(config, run))
        source = snap.wait_for_pod_ready(config.namespace, run.source_pod, timeout=1200)
        source_node = source.spec.node_name

        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )
        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            run.snapshot_name,
            timeout=1200,
        )

        ready = snap.condition(pod_snapshot, "Ready")
        assert ready and ready.get("status") == "True"
        content_ready = snap.condition(content, "Ready")
        assert content_ready and content_ready.get("reason") == "Captured"
        assert content["spec"]["source"]["podRef"]["name"] == run.source_pod
        assert content["spec"]["source"]["podRef"]["containers"] == [
            workloads.CONTAINER
        ]

        manifest = snap.checkpoint_artifact_manifest(
            config,
            source_node,
            run.checkpoint_id,
        )
        assert "criuDump:" in manifest
        assert "cudaRestore:" in manifest
        assert f"podName: {run.source_pod}" in manifest

        logs = k8s.pod_logs(config.namespace, run.source_pod)
        assert "vLLM engine initialized" in logs
        assert "vLLM generation paused" in logs
        assert "vLLM engine sleeping" in logs
        assert "ready for snapshot" in logs
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        snap.cleanup(config, run)


def vllm_source_pod(
    config: k8s.E2EConfig,
    run: VllmCheckpointRun,
) -> dict[str, Any]:
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": run.source_pod,
            "namespace": config.namespace,
            "labels": {
                "snapshot-e2e-test": run.suffix,
                "nvidia.com/snapshot-is-checkpoint-source": "true",
                "nvidia.com/snapshot-checkpoint-id": run.checkpoint_id,
            },
            "annotations": {
                "nvidia.com/snapshot-target-containers": workloads.CONTAINER,
            },
        },
        "spec": {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 1,
            "runtimeClassName": "nvidia",
            "securityContext": {
                "seccompProfile": {
                    "type": "Localhost",
                    "localhostProfile": "profiles/block-iouring.json",
                },
            },
            **workloads.workload_scheduling(),
            "containers": [
                {
                    "name": workloads.CONTAINER,
                    "image": run.image,
                    "imagePullPolicy": "IfNotPresent",
                    "command": ["/bin/bash", "-lc", vllm_source_command(run)],
                    "env": [
                        {
                            "name": "SNAPSHOT_CONTROL_DIR",
                            "value": workloads.CONTROL_DIR,
                        },
                        {"name": "NCCL_CUMEM_ENABLE", "value": "0"},
                        {"name": "NCCL_NVLS_ENABLE", "value": "0"},
                        {"name": "NCCL_IB_DISABLE", "value": "1"},
                        {"name": "NCCL_RAS_ENABLE", "value": "0"},
                    ],
                    "resources": {"limits": {"nvidia.com/gpu": "1"}},
                    "readinessProbe": {
                        "exec": {
                            "command": ["cat", workloads.SOURCE_READY],
                        },
                        "periodSeconds": 1,
                        "failureThreshold": 1200,
                    },
                    "volumeMounts": [
                        {
                            "name": "snapshot-control",
                            "mountPath": workloads.CONTROL_DIR,
                        },
                    ],
                },
            ],
            "volumes": [
                {
                    "name": "snapshot-control",
                    "emptyDir": {},
                },
            ],
        },
    }


def vllm_source_command(run: VllmCheckpointRun) -> str:
    return f"""set -euo pipefail
cat >/tmp/vllm_checkpoint.py <<'PY'
import asyncio
import os
from pathlib import Path

from vllm.engine.arg_utils import AsyncEngineArgs
from vllm.usage.usage_lib import UsageContext
from vllm.v1.engine.async_llm import AsyncLLM

control_dir = Path(os.environ["SNAPSHOT_CONTROL_DIR"])

async def main():
    engine_args = AsyncEngineArgs(
        model={run.model!r},
        dtype="half",
        max_model_len=512,
        gpu_memory_utilization=0.35,
        enforce_eager=True,
        enable_sleep_mode=True,
        disable_log_stats=True,
    )
    engine = AsyncLLM.from_engine_args(
        engine_args,
        usage_context=UsageContext.LLM_CLASS,
    )
    print("vLLM engine initialized", flush=True)
    await engine.pause_generation()
    print("vLLM generation paused", flush=True)
    await engine.sleep()
    print("vLLM engine sleeping", flush=True)
    (control_dir / "ready-for-snapshot").write_text("ready\\n", encoding="utf-8")
    print("ready for snapshot", flush=True)
    while not (control_dir / "snapshot-complete").exists():
        await asyncio.sleep(1)

if __name__ == "__main__":
    asyncio.run(main())
    os._exit(0)
PY
python3 /tmp/vllm_checkpoint.py
"""
