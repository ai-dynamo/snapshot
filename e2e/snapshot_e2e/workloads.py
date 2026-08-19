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
SNAPSHOT_COMPLETE = f"{CONTROL_DIR}/snapshot-complete"
RESTORE_DONE = f"{CONTROL_DIR}/restore-complete"
RESTORE_INITIAL_TOKEN = f"{CONTROL_DIR}/initial-restore-token"
STATE_DIR = "/tmp/e2e-state"
FILE_TOKEN = f"{STATE_DIR}/file-token"
OBSERVATIONS = f"{STATE_DIR}/observations.log"
SOURCE_TOKEN_ENV = "SNAPSHOT_E2E_SOURCE_TOKEN"
RESTORE_TOKEN_ENV = "SNAPSHOT_E2E_RESTORE_TOKEN"


@dataclass(frozen=True)
class TestRun:
    suffix: str
    checkpoint_id: str
    snapshot_name: str
    source_pod: str
    restore_pod: str
    image: str
    source_token: str
    restore_token: str

    @classmethod
    def new(cls, prefix: str) -> "TestRun":
        suffix = f"{prefix}-{uuid.uuid4().hex[:6]}"
        return cls(
            suffix=suffix,
            checkpoint_id=suffix,
            snapshot_name=f"{suffix}-snapshot",
            source_pod=f"{suffix}-source",
            restore_pod=f"{suffix}-restore",
            image=workload_image(),
            source_token=f"{suffix}-source-state",
            restore_token=f"{suffix}-restore-state",
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
    include_checkpoint_label: bool = True,
) -> dict[str, Any]:
    labels = {
        **run.labels,
        "nvidia.com/snapshot-is-checkpoint-source": "true",
    }
    if include_checkpoint_label:
        labels["nvidia.com/snapshot-checkpoint-id"] = run.checkpoint_id

    metadata = {
        "name": run.source_pod,
        "namespace": config.namespace,
        "labels": labels,
        "annotations": {
            "nvidia.com/snapshot-storage-type": "pvc",
            "nvidia.com/snapshot-storage-base-path": CHECKPOINT_DIR,
        },
    }
    if include_target_annotation:
        metadata["annotations"]["nvidia.com/snapshot-target-containers"] = CONTAINER
    spec = base_pod_spec(config, run, source_command(run.image, gpu), gpu)
    spec["containers"][0]["env"] = [
        {"name": SOURCE_TOKEN_ENV, "value": run.source_token},
    ]
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": metadata,
        "spec": spec,
    }


def snapshotjob_pod_template(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
    gpu: bool,
) -> dict[str, Any]:
    # No snapshot-* labels/annotations here: unlike source_pod (the plain
    # PodSnapshot flow, which the test must annotate itself), a SnapshotJob's
    # controller derives everything — the checkpoint ID, the target container,
    # storage — from spec.podTemplate and spec.podSnapshotTemplate. That is the
    # feature's whole point, so this template is what a real caller would write.
    spec = base_pod_spec(
        config,
        run,
        snapshotjob_source_command(run.image, gpu),
        gpu,
        control_volume=False,
    )
    spec["containers"][0]["env"] = [
        {"name": SOURCE_TOKEN_ENV, "value": run.source_token},
    ]
    return {"metadata": {"labels": {**run.labels}}, "spec": spec}


def snapshotjob_hang_pod_template(
    *,
    config: k8s.E2EConfig,
    run: TestRun,
) -> dict[str, Any]:
    # Never writes ready-for-snapshot, so the SnapshotJob's Running condition
    # never flips True and the source Job runs to its activeDeadlineSeconds —
    # the negative DeadlineExceeded path.
    command = 'set -euo pipefail\necho "[snapshotjob-hang] never signaling ready"\nsleep infinity\n'
    spec = base_pod_spec(config, run, command, gpu=False, control_volume=False)
    return {"metadata": {"labels": {**run.labels}}, "spec": spec}


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
        {"name": "SNAPSHOT_CONTROL_DIR", "value": CONTROL_DIR},
        {"name": RESTORE_TOKEN_ENV, "value": run.restore_token},
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
    *,
    control_volume: bool = True,
) -> dict[str, Any]:
    # control_volume=False for a SnapshotJob's spec.podTemplate: the operator
    # injects the snapshot-control emptyDir, mount, and env var itself
    # (EnsureControlVolume) — a caller-provided one would be redundant with
    # what the feature actually promises callers they do not have to set up.
    container: dict[str, Any] = {
        "name": CONTAINER,
        "image": run.image,
        "imagePullPolicy": "IfNotPresent",
        "command": ["/bin/bash", "-lc", command],
        "volumeMounts": [
            {"name": "checkpoint-storage", "mountPath": CHECKPOINT_DIR},
        ],
    }
    volumes: list[dict[str, Any]] = [
        {
            "name": "checkpoint-storage",
            "persistentVolumeClaim": {"claimName": config.pvc_name},
        },
    ]
    if control_volume:
        container["volumeMounts"].insert(0, {"name": "snapshot-control", "mountPath": CONTROL_DIR})
        volumes.insert(0, {"name": "snapshot-control", "emptyDir": {}})
    spec: dict[str, Any] = {
        "restartPolicy": "Never",
        # These are throwaway test pods; keep cleanup from waiting on the
        # Kubernetes default 30s graceful termination window between tests.
        "terminationGracePeriodSeconds": 1,
        "containers": [container],
        **workload_scheduling(),
        "volumes": volumes,
    }
    if gpu:
        spec["runtimeClassName"] = "nvidia"
        container["resources"] = {"limits": {"nvidia.com/gpu": "1"}}
    return spec


def workload_scheduling() -> dict[str, Any]:
    # Keep all workload pods on GPU nodes so the shared RWO checkpoint PVC binds in
    # a zone where both source and restore pods can schedule.
    node_selector = {
        "nvidia.com/gpu.present": "true",
    }
    if os.environ.get("GITHUB_ACTIONS") == "true":
        # Dynamo CI has MIG-capable and non-MIG GPU pools. Match preflight there,
        # while allowing local clusters that do not expose this label.
        node_selector["nvidia.com/mig.config"] = "all-disabled"

    return {
        "nodeSelector": node_selector,
        "tolerations": [
            {"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
            {"key": "dedicated", "operator": "Exists", "effect": "NoSchedule"},
            {
                "key": "CriticalAddonsOnly",
                "operator": "Exists",
                "effect": "NoSchedule",
            },
        ],
    }


def workload_image() -> str:
    image = os.environ.get("SNAPSHOT_E2E_WORKLOAD_IMAGE")
    if image:
        return image

    tag = os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG")
    if not tag:
        raise RuntimeError(
            "SNAPSHOT_E2E_SNAPSHOT_TAG or SNAPSHOT_E2E_WORKLOAD_IMAGE is required"
        )
    return f"ghcr.io/ai-dynamo/snapshot/agent:{tag}"


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
mkdir -p {STATE_DIR}
{state_loop}
"""


def snapshotjob_source_command(image: str, gpu: bool) -> str:
    state_loop = SNAPSHOTJOB_CUDA_SOURCE if gpu else SNAPSHOTJOB_CPU_SOURCE
    return f"""set -euo pipefail
echo "[snapshotjob-source] image={image}"
mkdir -p {STATE_DIR}
{state_loop}
"""


def restore_command(image: str, gpu: bool) -> str:
    # Restore starts from a live placeholder container. The agent nsenters into
    # it later, so this command may run briefly; record the initial restore token
    # and then wait for restore-complete. The agent may start restore soon after
    # the placeholder starts, so use an init container if this pre-restore write
    # ever needs to be guaranteed.
    return f"""set -euo pipefail
echo "[restore] image={image}"
restore_token="${{{RESTORE_TOKEN_ENV}}}"
echo "[restore] initial_restore_token=$restore_token"
printf '%s\\n' "$restore_token" > {RESTORE_INITIAL_TOKEN}
echo "[restore] waiting for restore-complete"
while [ ! -f {RESTORE_DONE} ]; do sleep 1; done
echo "[restore] restore-complete"
test -f {FILE_TOKEN}
test -s {OBSERVATIONS}
cat {OBSERVATIONS}
sleep infinity
"""


# CPU_SOURCE stores one token in two places: a shell variable, which is process
# memory, and {FILE_TOKEN}, which is filesystem state. The loop only provides
# post-restore liveness: every observation must keep reporting the source token.
CPU_SOURCE = f"""
cpu_token="${{{SOURCE_TOKEN_ENV}}}"
unset {SOURCE_TOKEN_ENV}
printf '%s\\n' "$cpu_token" > {FILE_TOKEN}
echo ready > {SOURCE_READY}
seq=0
while true; do
  file_token="$(cat {FILE_TOKEN} 2>/dev/null || true)"
  printf 'observation seq=%s cpu=%s file=%s gpu=disabled\\n' "$seq" "$cpu_token" "$file_token" >> {OBSERVATIONS}
  seq=$((seq + 1))
  sleep 5
done
"""


# SNAPSHOTJOB_CPU_SOURCE is CPU_SOURCE's SnapshotJob counterpart: it loops until
# the agent writes {SNAPSHOT_COMPLETE} (leaveRunning keeps the process alive
# through the dump so it can observe this), then exits 0 — the container exiting
# cleanly is what lets the source Job reach Complete=True and the SnapshotJob
# reach Completed=True.
#
# The observation interval is 1s, not the plain-PodSnapshot workload's 5s:
# freeze+dump+resume typically completes in a few seconds, faster than a 5s
# poll, so a caller waiting for >=2 pre-dump observations would otherwise
# never see a second one land before the sentinel appears on resume.
SNAPSHOTJOB_CPU_SOURCE = f"""
cpu_token="${{{SOURCE_TOKEN_ENV}}}"
unset {SOURCE_TOKEN_ENV}
printf '%s\\n' "$cpu_token" > {FILE_TOKEN}
echo ready > {SOURCE_READY}
seq=0
while [ ! -f {SNAPSHOT_COMPLETE} ]; do
  file_token="$(cat {FILE_TOKEN} 2>/dev/null || true)"
  printf 'observation seq=%s cpu=%s file=%s gpu=disabled\\n' "$seq" "$cpu_token" "$file_token" >> {OBSERVATIONS}
  seq=$((seq + 1))
  sleep 1
done
printf 'observation seq=%s cpu=%s file=%s gpu=disabled\\n' "$seq" "$cpu_token" "$(cat {FILE_TOKEN})" >> {OBSERVATIONS}
echo "[snapshotjob-source] snapshot-complete observed, exiting 0"
"""


# CUDA_SOURCE stores the source token in CPU memory, {FILE_TOKEN}, and a CUDA
# device allocation. On each loop it reads GPU memory back, compares it with CPU
# memory, then logs all three values. After restore, seeing the source token here
# proves the target pod's different restore token was replaced by snapshot state.
CUDA_SOURCE = f"""
cat >/tmp/cuda_hold.c <<'C_EOF'
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#define TOKEN_SIZE 256
typedef int CUdevice;
typedef void *CUcontext;
typedef void *CUdeviceptr;
typedef int CUresult;

static void read_file_token(char *buffer, size_t size) {{
  FILE *file = fopen("{FILE_TOKEN}", "r");
  if (!file) {{ buffer[0] = '\\0'; return; }}
  if (!fgets(buffer, size, file)) {{ buffer[0] = '\\0'; }}
  buffer[strcspn(buffer, "\\n")] = '\\0';
  fclose(file);
}}

int main(void) {{
  /* Copy the source token out of the environment into ordinary process memory,
     then remove the env var so observations depend on restored memory state. */
  const char *initial_token = getenv("{SOURCE_TOKEN_ENV}");
  if (!initial_token || initial_token[0] == '\\0') {{
    fprintf(stderr, "{SOURCE_TOKEN_ENV} is required\\n");
    return 1;
  }}
  char cpu_token[TOKEN_SIZE];
  memset(cpu_token, 0, sizeof(cpu_token));
  strncpy(cpu_token, initial_token, sizeof(cpu_token) - 1);
  unsetenv("{SOURCE_TOKEN_ENV}");

  /* Store the same token in the container filesystem; rootfs diff restore
     should bring this file into the restore pod. */
  FILE *file = fopen("{FILE_TOKEN}", "w");
  if (!file) {{ perror("open file token"); return 1; }}
  fprintf(file, "%s\\n", cpu_token);
  fclose(file);

  /* Resolve CUDA driver symbols dynamically so the workload does not need to
     link against CUDA at build time. */
  void *cuda = dlopen("libcuda.so.1", RTLD_NOW);
  if (!cuda) {{ fprintf(stderr, "dlopen libcuda.so.1 failed: %s\\n", dlerror()); return 1; }}
  CUresult (*cuInit)(unsigned int) = dlsym(cuda, "cuInit");
  CUresult (*cuDeviceGet)(CUdevice *, int) = dlsym(cuda, "cuDeviceGet");
  CUresult (*cuCtxCreate)(CUcontext *, unsigned int, CUdevice) = dlsym(cuda, "cuCtxCreate_v2");
  CUresult (*cuMemAlloc)(CUdeviceptr *, size_t) = dlsym(cuda, "cuMemAlloc_v2");
  CUresult (*cuMemcpyHtoD)(CUdeviceptr, const void *, size_t) = dlsym(cuda, "cuMemcpyHtoD_v2");
  CUresult (*cuMemcpyDtoH)(void *, CUdeviceptr, size_t) = dlsym(cuda, "cuMemcpyDtoH_v2");
  if (!cuInit || !cuDeviceGet || !cuCtxCreate || !cuMemAlloc || !cuMemcpyHtoD || !cuMemcpyDtoH) {{
    fprintf(stderr, "missing CUDA driver symbol\\n");
    return 1;
  }}
  CUdevice device = 0;
  CUcontext context = NULL;
  CUdeviceptr ptr = NULL;
  /* Allocate a tiny GPU buffer and copy the source token into device memory.
     The loop below reads it back after restore to prove GPU memory survived. */
  if (cuInit(0) != 0 || cuDeviceGet(&device, 0) != 0 ||
      cuCtxCreate(&context, 0, device) != 0 ||
      cuMemAlloc(&ptr, sizeof(cpu_token)) != 0) {{
    fprintf(stderr, "CUDA setup failed\\n");
    return 1;
  }}
  if (cuMemcpyHtoD(ptr, cpu_token, sizeof(cpu_token)) != 0) {{
    fprintf(stderr, "initial CUDA token copy failed\\n");
    return 1;
  }}
  FILE *ready = fopen("{SOURCE_READY}", "w");
  if (ready) {{ fprintf(ready, "ready\\n"); fclose(ready); }}
  int seq = 0;
  while (1) {{
    char gpu_token[TOKEN_SIZE];
    char file_token[TOKEN_SIZE];
    memset(gpu_token, 0, sizeof(gpu_token));
    memset(file_token, 0, sizeof(file_token));
    if (cuMemcpyDtoH(gpu_token, ptr, sizeof(gpu_token)) != 0) {{
      fprintf(stderr, "CUDA token read failed\\n");
      return 2;
    }}
    gpu_token[TOKEN_SIZE - 1] = '\\0';
    read_file_token(file_token, sizeof(file_token));
    if (strcmp(gpu_token, cpu_token) != 0) {{
      fprintf(stderr, "CUDA token mismatch: cpu=%s gpu=%s\\n", cpu_token, gpu_token);
      return 2;
    }}
    FILE *log = fopen("{OBSERVATIONS}", "a");
    if (log) {{
      fprintf(log, "observation seq=%d cpu=%s file=%s gpu=%s\\n", seq, cpu_token, file_token, gpu_token);
      fclose(log);
    }}
    seq++;
    sleep(5);
  }}
}}
C_EOF
cc /tmp/cuda_hold.c -ldl -o /tmp/cuda_hold
exec /tmp/cuda_hold
"""


# SNAPSHOTJOB_CUDA_SOURCE is CUDA_SOURCE's SnapshotJob counterpart: same
# cpu/file/GPU token triad, but the observation loop exits once it sees
# {SNAPSHOT_COMPLETE} (written by the agent post-dump; leaveRunning keeps this
# process alive to observe it) instead of looping forever, so the process exits
# 0 and the source Job can reach Complete=True.
#
# The observation interval is 1s, not the plain-PodSnapshot workload's 5s:
# freeze+dump+resume typically completes in a few seconds, faster than a 5s
# poll, so a caller waiting for >=2 pre-dump observations would otherwise
# never see a second one land before the sentinel appears on resume.
SNAPSHOTJOB_CUDA_SOURCE = f"""
cat >/tmp/cuda_hold.c <<'C_EOF'
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#define TOKEN_SIZE 256
typedef int CUdevice;
typedef void *CUcontext;
typedef void *CUdeviceptr;
typedef int CUresult;

static void read_file_token(char *buffer, size_t size) {{
  FILE *file = fopen("{FILE_TOKEN}", "r");
  if (!file) {{ buffer[0] = '\\0'; return; }}
  if (!fgets(buffer, size, file)) {{ buffer[0] = '\\0'; }}
  buffer[strcspn(buffer, "\\n")] = '\\0';
  fclose(file);
}}

int main(void) {{
  const char *initial_token = getenv("{SOURCE_TOKEN_ENV}");
  if (!initial_token || initial_token[0] == '\\0') {{
    fprintf(stderr, "{SOURCE_TOKEN_ENV} is required\\n");
    return 1;
  }}
  char cpu_token[TOKEN_SIZE];
  memset(cpu_token, 0, sizeof(cpu_token));
  strncpy(cpu_token, initial_token, sizeof(cpu_token) - 1);
  unsetenv("{SOURCE_TOKEN_ENV}");

  FILE *file = fopen("{FILE_TOKEN}", "w");
  if (!file) {{ perror("open file token"); return 1; }}
  fprintf(file, "%s\\n", cpu_token);
  fclose(file);

  void *cuda = dlopen("libcuda.so.1", RTLD_NOW);
  if (!cuda) {{ fprintf(stderr, "dlopen libcuda.so.1 failed: %s\\n", dlerror()); return 1; }}
  CUresult (*cuInit)(unsigned int) = dlsym(cuda, "cuInit");
  CUresult (*cuDeviceGet)(CUdevice *, int) = dlsym(cuda, "cuDeviceGet");
  CUresult (*cuCtxCreate)(CUcontext *, unsigned int, CUdevice) = dlsym(cuda, "cuCtxCreate_v2");
  CUresult (*cuMemAlloc)(CUdeviceptr *, size_t) = dlsym(cuda, "cuMemAlloc_v2");
  CUresult (*cuMemcpyHtoD)(CUdeviceptr, const void *, size_t) = dlsym(cuda, "cuMemcpyHtoD_v2");
  CUresult (*cuMemcpyDtoH)(void *, CUdeviceptr, size_t) = dlsym(cuda, "cuMemcpyDtoH_v2");
  if (!cuInit || !cuDeviceGet || !cuCtxCreate || !cuMemAlloc || !cuMemcpyHtoD || !cuMemcpyDtoH) {{
    fprintf(stderr, "missing CUDA driver symbol\\n");
    return 1;
  }}
  CUdevice device = 0;
  CUcontext context = NULL;
  CUdeviceptr ptr = NULL;
  if (cuInit(0) != 0 || cuDeviceGet(&device, 0) != 0 ||
      cuCtxCreate(&context, 0, device) != 0 ||
      cuMemAlloc(&ptr, sizeof(cpu_token)) != 0) {{
    fprintf(stderr, "CUDA setup failed\\n");
    return 1;
  }}
  if (cuMemcpyHtoD(ptr, cpu_token, sizeof(cpu_token)) != 0) {{
    fprintf(stderr, "initial CUDA token copy failed\\n");
    return 1;
  }}
  FILE *ready = fopen("{SOURCE_READY}", "w");
  if (ready) {{ fprintf(ready, "ready\\n"); fclose(ready); }}
  int seq = 0;
  while (access("{SNAPSHOT_COMPLETE}", F_OK) != 0) {{
    char gpu_token[TOKEN_SIZE];
    char file_token[TOKEN_SIZE];
    memset(gpu_token, 0, sizeof(gpu_token));
    memset(file_token, 0, sizeof(file_token));
    if (cuMemcpyDtoH(gpu_token, ptr, sizeof(gpu_token)) != 0) {{
      fprintf(stderr, "CUDA token read failed\\n");
      return 2;
    }}
    gpu_token[TOKEN_SIZE - 1] = '\\0';
    read_file_token(file_token, sizeof(file_token));
    if (strcmp(gpu_token, cpu_token) != 0) {{
      fprintf(stderr, "CUDA token mismatch: cpu=%s gpu=%s\\n", cpu_token, gpu_token);
      return 2;
    }}
    FILE *log = fopen("{OBSERVATIONS}", "a");
    if (log) {{
      fprintf(log, "observation seq=%d cpu=%s file=%s gpu=%s\\n", seq, cpu_token, file_token, gpu_token);
      fclose(log);
    }}
    seq++;
    sleep(1);
  }}
  fprintf(stderr, "snapshot-complete observed, exiting 0\\n");
  return 0;
}}
C_EOF
cc /tmp/cuda_hold.c -ldl -o /tmp/cuda_hold
exec /tmp/cuda_hold
"""
