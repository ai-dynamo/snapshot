# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Pod specs used by Snapshot functional e2e tests."""

from __future__ import annotations

import os
import uuid
from dataclasses import dataclass
from typing import Any

from snapshot_e2e import k8s


CONTAINER = "main"
CONTROL_DIR = "/snapshot-control"
CHECKPOINT_DIR = "/checkpoints"
SOURCE_READY = f"{CONTROL_DIR}/ready-for-snapshot"
RESTORE_DONE = f"{CONTROL_DIR}/restore-complete"


@dataclass(frozen=True)
class TestRun:
    suffix: str
    checkpoint_id: str
    snapshot_name: str
    source_pod: str
    restore_pod: str
    image: str

    @classmethod
    def new(cls, prefix: str) -> "TestRun":
        suffix = f"{prefix}-{uuid.uuid4().hex[:6]}"
        tag = os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.0.0-g71827d8e")
        return cls(
            suffix=suffix,
            checkpoint_id=suffix,
            snapshot_name=f"{suffix}-snapshot",
            source_pod=f"{suffix}-source",
            restore_pod=f"{suffix}-restore",
            image=os.environ.get(
                "SNAPSHOT_E2E_WORKLOAD_IMAGE",
                f"ghcr.io/ai-dynamo/snapshot/agent:{tag}",
            ),
        )

    @property
    def labels(self) -> dict[str, str]:
        return {"snapshot-e2e-test": self.suffix}


def source_pod(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    gpu: bool,
    include_target_annotation: bool = True,
) -> dict[str, Any]:
    metadata = {
        "name": run.source_pod,
        "namespace": config.namespace,
        "labels": {
            **run.labels,
            "nvidia.com/snapshot-checkpoint-id": run.checkpoint_id,
            "nvidia.com/snapshot-is-checkpoint-source": "true",
        },
        "annotations": {
            "nvidia.com/snapshot-storage-type": "pvc",
            "nvidia.com/snapshot-storage-base-path": CHECKPOINT_DIR,
        },
    }
    if include_target_annotation:
        metadata["annotations"]["nvidia.com/snapshot-target-containers"] = CONTAINER
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": metadata,
        "spec": base_pod_spec(config, run, source_command(run.image, gpu), gpu),
    }


def restore_pod(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    gpu: bool,
    source_node: str | None = None,
) -> dict[str, Any]:
    spec = base_pod_spec(config, run, restore_command(run.image, gpu), gpu)
    spec["securityContext"] = {
        "seccompProfile": {
            "type": "Localhost",
            "localhostProfile": "profiles/block-iouring.json",
        }
    }
    spec["containers"][0]["env"] = [
        {"name": "DYN_SNAPSHOT_RESTORE_STANDBY", "value": "1"},
        {"name": "DYN_SNAPSHOT_CONTROL_DIR", "value": CONTROL_DIR},
    ]
    spec["containers"][0]["startupProbe"] = {
        "exec": {"command": ["/bin/bash", "-lc", f"test -f {RESTORE_DONE}"]},
        "periodSeconds": 1,
        "failureThreshold": 1200,
    }
    if source_node:
        spec["affinity"] = same_node_affinity(source_node)
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": run.restore_pod,
            "namespace": config.namespace,
            "labels": {
                **run.labels,
                "nvidia.com/snapshot-checkpoint-id": run.checkpoint_id,
                "nvidia.com/snapshot-is-restore-target": "true",
            },
            "annotations": {
                "nvidia.com/snapshot-target-containers": CONTAINER,
                "nvidia.com/snapshot-artifact-version": "1",
                "nvidia.com/snapshot-storage-type": "pvc",
                "nvidia.com/snapshot-storage-base-path": CHECKPOINT_DIR,
            },
        },
        "spec": spec,
    }


def base_pod_spec(
    config: k8s.E2EConfig,
    run: TestRun,
    command: str,
    gpu: bool,
) -> dict[str, Any]:
    container: dict[str, Any] = {
        "name": CONTAINER,
        "image": run.image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["/bin/bash", "-lc", command],
        "volumeMounts": [
            {"name": "snapshot-control", "mountPath": CONTROL_DIR},
            {"name": "checkpoint-storage", "mountPath": CHECKPOINT_DIR},
        ],
    }
    spec: dict[str, Any] = {
        "restartPolicy": "Never",
        "containers": [container],
        "volumes": [
            {"name": "snapshot-control", "emptyDir": {}},
            {
                "name": "checkpoint-storage",
                "persistentVolumeClaim": {"claimName": config.pvc_name},
            },
        ],
    }
    if gpu:
        spec.update(gpu_scheduling())
        container["resources"] = {"limits": {"nvidia.com/gpu": "1"}}
    return spec


def gpu_scheduling() -> dict[str, Any]:
    return {
        "runtimeClassName": "nvidia",
        "nodeSelector": {
            "nvidia.com/gpu.present": "true",
            "nvidia.com/mig.config": "all-disabled",
        },
        "tolerations": [
            {"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
            {"key": "dedicated", "operator": "Exists", "effect": "NoSchedule"},
        ],
    }


def same_node_affinity(node: str) -> dict[str, Any]:
    return {
        "nodeAffinity": {
            "requiredDuringSchedulingIgnoredDuringExecution": {
                "nodeSelectorTerms": [
                    {
                        "matchFields": [
                            {
                                "key": "metadata.name",
                                "operator": "In",
                                "values": [node],
                            }
                        ]
                    }
                ]
            }
        }
    }


def source_command(image: str, gpu: bool) -> str:
    state_loop = CUDA_SOURCE if gpu else CPU_SOURCE
    return f"""set -euo pipefail
echo "[source] image={image}"
echo "fs-marker-before-snapshot" > /tmp/snapshot-fs-marker
{state_loop}
"""


def restore_command(image: str, gpu: bool) -> str:
    gpu_validation = "test -s /tmp/gpu-marker" if gpu else "true"
    return f"""set -euo pipefail
echo "[restore] image={image}"
echo "[restore] waiting for restore-complete"
while [ ! -f {RESTORE_DONE} ]; do sleep 1; done
echo "[restore] restore-complete"
test -f /tmp/snapshot-fs-marker
test -s /tmp/tick.log
{gpu_validation}
cat /tmp/tick.log
sleep infinity
"""


CPU_SOURCE = f"""
echo cpu-state > /tmp/cpu-marker
echo ready > {SOURCE_READY}
i=0
while true; do
  echo "tick $i" >> /tmp/tick.log
  i=$((i + 1))
  sleep 5
done
"""


CUDA_SOURCE = f"""
cat >/tmp/cuda_hold.c <<'C_EOF'
#include <dlfcn.h>
#include <stdio.h>
#include <unistd.h>
typedef int CUdevice;
typedef void *CUcontext;
typedef void *CUdeviceptr;
typedef int CUresult;
int main(void) {{
  void *cuda = dlopen("libcuda.so.1", RTLD_NOW);
  if (!cuda) {{ fprintf(stderr, "dlopen libcuda.so.1 failed: %s\\n", dlerror()); return 1; }}
  CUresult (*cuInit)(unsigned int) = dlsym(cuda, "cuInit");
  CUresult (*cuDeviceGet)(CUdevice *, int) = dlsym(cuda, "cuDeviceGet");
  CUresult (*cuCtxCreate)(CUcontext *, unsigned int, CUdevice) = dlsym(cuda, "cuCtxCreate_v2");
  CUresult (*cuMemAlloc)(CUdeviceptr *, size_t) = dlsym(cuda, "cuMemAlloc_v2");
  if (!cuInit || !cuDeviceGet || !cuCtxCreate || !cuMemAlloc) {{
    fprintf(stderr, "missing CUDA driver symbol\\n");
    return 1;
  }}
  CUdevice device = 0;
  CUcontext context = NULL;
  CUdeviceptr ptr = NULL;
  if (cuInit(0) != 0 || cuDeviceGet(&device, 0) != 0 ||
      cuCtxCreate(&context, 0, device) != 0 || cuMemAlloc(&ptr, 4096) != 0) {{
    fprintf(stderr, "CUDA setup failed\\n");
    return 1;
  }}
  FILE *gpu = fopen("/tmp/gpu-marker", "w");
  if (gpu) {{ fprintf(gpu, "cuda-allocation-ready\\n"); fclose(gpu); }}
  FILE *ready = fopen("{SOURCE_READY}", "w");
  if (ready) {{ fprintf(ready, "ready\\n"); fclose(ready); }}
  int tick = 0;
  while (1) {{
    FILE *log = fopen("/tmp/tick.log", "a");
    if (log) {{ fprintf(log, "tick %d\\n", tick++); fclose(log); }}
    sleep(5);
  }}
}}
C_EOF
cc /tmp/cuda_hold.c -ldl -o /tmp/cuda_hold
exec /tmp/cuda_hold
"""
