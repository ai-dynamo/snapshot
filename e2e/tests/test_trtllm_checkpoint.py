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

DEFAULT_TRTLLM_MODEL = "Qwen/Qwen3-0.6B"
TRTLLM_RESTORE_READY = f"{workloads.CONTROL_DIR}/trtllm-restore-ready"


@dataclass(frozen=True)
class TrtllmCheckpointRun:
    suffix: str
    snapshot_name: str
    source_pod: str
    restore_pod: str
    image: str
    model: str

    @classmethod
    def new(cls) -> TrtllmCheckpointRun:
        suffix = f"trtllm-checkpoint-{uuid.uuid4().hex[:6]}"
        image = os.environ.get("SNAPSHOT_E2E_TRTLLM_IMAGE")
        if not image:
            pytest.skip("SNAPSHOT_E2E_TRTLLM_IMAGE is required")
        return cls(
            suffix=suffix,
            snapshot_name=f"{suffix}-snapshot",
            source_pod=f"{suffix}-source",
            restore_pod=f"{suffix}-restore",
            image=image,
            model=os.environ.get("SNAPSHOT_E2E_TRTLLM_MODEL", DEFAULT_TRTLLM_MODEL),
        )


@pytest.mark.snapshot_success
@pytest.mark.gpu
@pytest.mark.trtllm
def test_trtllm_checkpoint_restores_idle_engine(
    config: k8s.E2EConfig,
) -> None:
    run = TrtllmCheckpointRun.new()
    try:
        k8s.create_pod(trtllm_source_pod(config, run))
        source = snap.wait_for_pod_ready(
            config.namespace,
            run.source_pod,
            timeout=1800,
        )
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
            timeout=1800,
        )

        ready = snap.condition(pod_snapshot, "Ready")
        assert ready and ready.get("status") == "True"
        content_ready = snap.condition(content, "Ready")
        assert content_ready and content_ready.get("reason") == "Captured"
        assert content["spec"]["source"]["podRef"]["name"] == run.source_pod
        assert content["spec"]["source"]["podRef"]["containers"] == [
            workloads.CONTAINER
        ]

        content_uid = content["metadata"]["uid"]
        manifest = snap.checkpoint_artifact_manifest(
            config,
            source_node,
            content_uid,
        )
        assert f"contentUID: {content_uid}" in manifest
        assert "containerName: main" in manifest
        assert "criuDump:" in manifest
        assert "cudaRestore:" in manifest
        assert f"podName: {run.source_pod}" in manifest

        logs = k8s.pod_logs(config.namespace, run.source_pod)
        assert "TensorRT-LLM engine initialized" in logs
        assert logs.count("TensorRT-LLM pre-snapshot output=") == 2
        assert "TensorRT-LLM engine idle" in logs
        assert "ready for snapshot" in logs

        k8s.delete_pod(config.namespace, run.source_pod)
        snap.wait_for_pod_deleted(config.namespace, run.source_pod)
        k8s.create_pod(trtllm_restore_pod(config, run, source_node))
        snap.wait_for_restored_condition(
            config.namespace,
            run.restore_pod,
            "True",
            "RestoreSucceeded",
            timeout=1800,
        )
        snap.wait_for_pod_ready(
            config.namespace,
            run.restore_pod,
            timeout=1800,
        )
        snap.wait_for_file(
            config.namespace,
            run.restore_pod,
            TRTLLM_RESTORE_READY,
            timeout=1800,
        )

        restored_output = k8s.exec_command(
            config.namespace,
            run.restore_pod,
            f"cat {TRTLLM_RESTORE_READY}",
        )
        assert restored_output.strip()
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        snap.cleanup(config, run)


def trtllm_source_pod(
    config: k8s.E2EConfig,
    run: TrtllmCheckpointRun,
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
            },
        },
        "spec": {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 1,
            "runtimeClassName": "nvidia",
            "securityContext": {
                "fsGroup": 1000,
                "fsGroupChangePolicy": "OnRootMismatch",
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
                    "command": ["/bin/bash", "-lc", trtllm_source_command(run)],
                    "env": [
                        {
                            "name": "SNAPSHOT_CONTROL_DIR",
                            "value": workloads.CONTROL_DIR,
                        },
                        {"name": "HF_HUB_DISABLE_XET", "value": "1"},
                        {"name": "TLLM_NCCL_SYMMETRIC_ZERO_COPY", "value": "0"},
                        {"name": "UCX_TLS", "value": "tcp,self"},
                    ],
                    "resources": {"limits": {"nvidia.com/gpu": "1"}},
                    "readinessProbe": {
                        "exec": {"command": ["cat", workloads.SOURCE_READY]},
                        "periodSeconds": 1,
                        "failureThreshold": 1800,
                    },
                    "volumeMounts": [
                        {
                            "name": "snapshot-control",
                            "mountPath": workloads.CONTROL_DIR,
                        },
                        {"name": "tun", "mountPath": "/dev/net/tun"},
                    ],
                },
            ],
            "volumes": [
                {"name": "snapshot-control", "emptyDir": {}},
                {
                    "name": "tun",
                    "hostPath": {
                        "path": "/dev/net/tun",
                        "type": "CharDevice",
                    },
                },
            ],
        },
    }


def trtllm_restore_pod(
    config: k8s.E2EConfig,
    run: TrtllmCheckpointRun,
    source_node: str,
) -> dict[str, Any]:
    pod = trtllm_source_pod(config, run)
    pod["metadata"] = {
        "name": run.restore_pod,
        "namespace": config.namespace,
        "labels": {"snapshot-e2e-test": run.suffix},
        "annotations": {"nvidia.com/restore-from": run.snapshot_name},
    }
    pod["spec"]["affinity"] = workloads.same_node_affinity(source_node)
    container = pod["spec"]["containers"][0]
    container["command"] = ["/bin/bash", "-lc", "sleep infinity"]
    container["startupProbe"] = {
        "exec": {"command": ["cat", workloads.RESTORE_DONE]},
        "periodSeconds": 1,
        "failureThreshold": 1800,
    }
    container["readinessProbe"] = {
        "exec": {"command": ["cat", TRTLLM_RESTORE_READY]},
        "periodSeconds": 1,
        "failureThreshold": 1800,
    }
    return pod


def trtllm_source_command(run: TrtllmCheckpointRun) -> str:
    return f"""set -euo pipefail
cat >/tmp/trtllm_checkpoint.py <<'PY'
import gc
import os
import time
from pathlib import Path

from tensorrt_llm import LLM, SamplingParams

control_dir = Path(os.environ["SNAPSHOT_CONTROL_DIR"])

def generate_text(llm, prompts):
    outputs = llm.generate(
        prompts,
        SamplingParams(temperature=0.0, max_tokens=16),
        use_tqdm=False,
    )
    texts = []
    for output in outputs:
        if not output.outputs:
            raise RuntimeError("TensorRT-LLM produced no output")
        text = output.outputs[0].text.strip()
        if not text:
            raise RuntimeError("TensorRT-LLM produced empty output")
        texts.append(text)
    return texts

def main():
    llm = LLM(
        model={run.model!r},
        backend="pytorch",
        dtype="float16",
        trust_remote_code=True,
        tensor_parallel_size=1,
        max_num_tokens=1024,
        max_seq_len=512,
        max_batch_size=1,
        enable_chunked_prefill=False,
        kv_cache_config={{"free_gpu_memory_fraction": 0.10}},
    )
    print("TensorRT-LLM engine initialized", flush=True)
    prompts = [
        "Summarize why checkpoint and restore testing matters.",
        "Continue this sequence with four numbers: 1, 2, 3, 4,",
    ]
    for text in generate_text(llm, prompts):
        print(f"TensorRT-LLM pre-snapshot output={{text!r}}", flush=True)
    gc.collect()
    print("TensorRT-LLM engine idle", flush=True)
    (control_dir / "ready-for-snapshot").write_text("ready\\n", encoding="utf-8")
    print("ready for snapshot", flush=True)
    while True:
        if (control_dir / "snapshot-complete").exists():
            os._exit(0)
        if (control_dir / "restore-complete").exists():
            text = generate_text(llm, ["Reply with one word: ready"])[0]
            (control_dir / "trtllm-restore-ready").write_text(
                text + "\\n",
                encoding="utf-8",
            )
            print(f"TensorRT-LLM restored output={{text!r}}", flush=True)
            while True:
                time.sleep(3600)
        time.sleep(1)

if __name__ == "__main__":
    main()
PY
python3 /tmp/trtllm_checkpoint.py
"""
