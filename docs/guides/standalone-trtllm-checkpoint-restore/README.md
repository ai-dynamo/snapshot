<!--
SPDX-FileCopyrightText: 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint and restore TensorRT-LLM with Snapshot

This guide shows how to quiesce a TensorRT-LLM engine, capture its Kubernetes
pod with Snapshot, and resume inference in a restored pod.

Snapshot must already be installed in the cluster. The guide only covers the
TensorRT-LLM workload and the Kubernetes resources needed for capture and
restore.

Companion examples:

- [`snapshot_lifecycle.py`](snapshot_lifecycle.py)
- [Source pod fields](trtllm-source-pod-fields.yaml)
- [`PodSnapshot`](trtllm-snapshot.yaml)
- [Restore pod](trtllm-restore.yaml)

The YAML files are reference templates. The commands below resolve runtime
values such as the source pod UID and node name.

## Prerequisites

- An x86_64 Kubernetes cluster with an NVIDIA Ampere, Ada Lovelace, Hopper, or
  Blackwell GPU node.
- Snapshot installed, including the operator, node agent, and
  `PodSnapshot` and `PodSnapshotContent` CRDs.
- A TensorRT-LLM Python application and pod manifest that you can modify.
- `kubectl` access to create pods and `PodSnapshot` resources.
- A single-GPU text model supported by the TensorRT-LLM image.
- The same immutable workload image for capture and restore.
- The source GPU node available for restore. This guide restores to the same
  node.

The currently tested configuration uses NVIDIA driver 580 or newer, MIG
disabled, and a workload image compatible with the Snapshot restore utilities.

Use the namespace from the current `kubectl` context, or set `NAMESPACE`
beforehand to override it:

```bash
NAMESPACE="${NAMESPACE:-$(kubectl config view --minify --output 'jsonpath={..namespace}')}"
NAMESPACE="${NAMESPACE:-default}"
kubectl get pods --namespace "$NAMESPACE"
```

Select the TensorRT-LLM pod. For a pod with sidecars, set `CONTAINER`
beforehand to the TensorRT-LLM container name; otherwise the first container is
used. All other values are derived:

```bash
SOURCE_POD="<trtllm-pod-name>"
CONTAINER="${CONTAINER:-$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.spec.containers[0].name}')}"
RESTORE_POD="${SOURCE_POD}-restore"
SOURCE_NODE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.spec.nodeName}')"
TRTLLM_IMAGE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output "jsonpath={.status.containerStatuses[?(@.name=='${CONTAINER}')].imageID}")"
TRTLLM_IMAGE="${TRTLLM_IMAGE#*://}"
```

## 1. Configure the TensorRT-LLM lifecycle

This snapshot flow does not call TensorRT-LLM's sleep or wake APIs. A
synchronous `llm.generate()` call returns after that generation finishes. A
server must stop accepting requests and wait for its active generations before
calling the lifecycle below.

Add `TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` to the source pod's container environment
as shown in [step 2](#2-prepare-the-source-pod). Kubernetes sets it before the
application starts, so it is present before TensorRT-LLM is imported or the
engine is created. This disables NCCL registered windows, which CUDA checkpoint
cannot capture.

`snapshot_lifecycle.py` does not create the snapshot. It adapts the
TensorRT-LLM application to Snapshot's file-based lifecycle: mark the engine as
ready to capture, wait for restore, and verify the restored engine.

Create [`snapshot_lifecycle.py`](snapshot_lifecycle.py) next to the application
entrypoint.

### Snapshot phase

```python
import gc
import time
from pathlib import Path

from tensorrt_llm import LLM, SamplingParams

CONTROL_DIR = Path("/snapshot-control")


def quiesce_for_snapshot() -> None:
    gc.collect()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )
    while not CONTROL_DIR.joinpath("restore-complete").exists():
        time.sleep(1)
```

The snapshot phase has no TensorRT-LLM pause call:

- `gc.collect()` releases unreachable Python objects before capture.
- `ready-for-snapshot` tells the pod readiness probe that the application has
  reached the capture point.
- The loop keeps the process at that point. Snapshot terminates the source
  process after capture; the restored process leaves the loop after Snapshot
  writes `restore-complete`.
- `time.sleep(1)` prevents the wait loop from continuously using CPU.

Do not call TensorRT-LLM sleep for this flow. Releasing the engine allocations
would remove the initialized GPU state that Snapshot is intended to preserve.

### Restore phase

Add the restore function to the same file:

```python
def resume_after_restore(llm: LLM) -> str:
    outputs = llm.generate(
        ["Reply with one word: ready"],
        SamplingParams(temperature=0.0, max_tokens=16),
        use_tqdm=False,
    )
    text = outputs[0].outputs[0].text.strip()
    if not text:
        raise RuntimeError("TensorRT-LLM produced empty output after restore")
    CONTROL_DIR.joinpath("trtllm-restore-ready").write_text(
        text + "\n",
        encoding="utf-8",
    )
    return text
```

`llm.generate()` is the TensorRT-LLM call that proves the restored `LLM` object
can still generate text. `trtllm-restore-ready` tells the restored pod's
readiness probe that this validation succeeded.

### Call the lifecycle from the application

The application calls these functions in order:

1. `LLM(...)` creates the TensorRT-LLM engine.
2. `llm.generate(...)` warms the engine and returns after the generation
   finishes.
3. `quiesce_for_snapshot()` runs garbage collection, writes
   `ready-for-snapshot`, and waits at the capture point.
4. The restored `quiesce_for_snapshot()` returns after it sees
   `restore-complete`.
5. `resume_after_restore(llm)` calls `llm.generate(...)` on the restored engine
   and writes `trtllm-restore-ready`.

Import and call the lifecycle in the existing application entrypoint:

```python
from tensorrt_llm import LLM, SamplingParams

from snapshot_lifecycle import quiesce_for_snapshot, resume_after_restore


def main() -> None:
    llm = LLM(
        model="Qwen/Qwen3-0.6B",
        backend="pytorch",
        dtype="float16",
        trust_remote_code=True,
        tensor_parallel_size=1,
        max_num_tokens=1024,
        max_seq_len=512,
        max_batch_size=1,
        enable_chunked_prefill=False,
        kv_cache_config={"free_gpu_memory_fraction": 0.10},
    )

    outputs = llm.generate(
        ["Warm up the engine."],
        SamplingParams(temperature=0.0, max_tokens=16),
        use_tqdm=False,
    )
    if not outputs[0].outputs[0].text.strip():
        raise RuntimeError("TensorRT-LLM warmup produced empty output")

    quiesce_for_snapshot()
    resume_after_restore(llm)


if __name__ == "__main__":
    main()
```

The source process waits in `quiesce_for_snapshot()` and is terminated after
capture. The restored process resumes from that same loop, sees
`restore-complete`, and calls `resume_after_restore(llm)`.

Build a new workload image containing these source changes and use that image
when [preparing the source pod](#2-prepare-the-source-pod). The readiness check
in [step 3](#3-quiesce-tensorrt-llm) confirms when it can be captured.

## 2. Prepare the source pod

Add these fields to the existing TensorRT-LLM pod template:

```yaml
metadata:
  labels:
    nvidia.com/snapshot-is-checkpoint-source: "true"
spec:
  runtimeClassName: nvidia
  securityContext:
    fsGroup: 1000
    fsGroupChangePolicy: OnRootMismatch
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: <trtllm-container-name>
    env:
    - name: SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    - name: TLLM_NCCL_SYMMETRIC_ZERO_COPY
      value: "0"
    - name: UCX_TLS
      value: tcp,self
    readinessProbe:
      exec:
        command:
        - cat
        - /snapshot-control/ready-for-snapshot
      periodSeconds: 1
      failureThreshold: 1800
    volumeMounts:
    - name: snapshot-control
      mountPath: /snapshot-control
    - name: tun
      mountPath: /dev/net/tun
  volumes:
  - name: snapshot-control
    emptyDir: {}
  - name: tun
    hostPath:
      path: /dev/net/tun
      type: CharDevice
```

The label identifies the pod as a prepared capture source.
`TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` is set on the TensorRT-LLM container before
its process starts. `UCX_TLS=tcp,self` avoids RDMA mappings that CRIU cannot
restore. The control volume carries lifecycle files, and `/dev/net/tun` allows
Snapshot to restore network devices created by the TensorRT-LLM, OpenMPI, and
PyTorch stack.

Redeploy the pod with the updated image and fields. Wait until its container is
running:

```bash
kubectl wait \
  --namespace "$NAMESPACE" \
  --for=jsonpath='{.status.phase}'=Running \
  "pod/$SOURCE_POD" \
  --timeout=30m
```

## 3. Quiesce TensorRT-LLM

The application calls `quiesce_for_snapshot()` after engine initialization and
warm-up. No additional TensorRT-LLM command is needed.

Wait for the readiness probe and confirm the lifecycle file:

```bash
kubectl wait \
  --namespace "$NAMESPACE" \
  --for=condition=Ready \
  "pod/$SOURCE_POD" \
  --timeout=30m

kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- test -f /snapshot-control/ready-for-snapshot
```

The pod is now idle and ready to capture. Its model and initialized GPU state
remain in memory, but no generation request is running.

## 4. Capture the pod

A `PodSnapshot` is the capture request. Snapshot creates the corresponding
`PodSnapshotContent`, which records the captured artifact. Do not create the
`PodSnapshotContent` yourself.

Create the capture request using the live pod UID:

```bash
SOURCE_POD_UID="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.metadata.uid}')"

cat <<EOF | kubectl apply -f -
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: ${SOURCE_POD}-snapshot
  namespace: ${NAMESPACE}
spec:
  source:
    podRef:
      name: ${SOURCE_POD}
      uid: ${SOURCE_POD_UID}
      containers:
      - ${CONTAINER}
EOF

kubectl wait \
  --namespace "$NAMESPACE" \
  --for=condition=Ready \
  "podsnapshot/${SOURCE_POD}-snapshot" \
  --timeout=30m
```

Inspect both resources:

```bash
kubectl get podsnapshot "${SOURCE_POD}-snapshot" \
  --namespace "$NAMESPACE" \
  --output yaml

kubectl get podsnapshotcontent \
  "$(kubectl get podsnapshot "${SOURCE_POD}-snapshot" \
    --namespace "$NAMESPACE" \
    --output jsonpath='{.status.boundSnapshotContentName}')" \
  --output yaml
```

The source process is expected to terminate after a successful capture.

## 5. Create the restore pod

The restore pod supplies the target container, GPU, and mounts. Snapshot
replaces its inert process with the captured TensorRT-LLM process.

Use the same image and workload settings as the source pod. The
`nvidia.com/restore-from` annotation references the `PodSnapshot`:

```bash
kubectl delete pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --ignore-not-found \
  --wait=true

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${RESTORE_POD}
  namespace: ${NAMESPACE}
  annotations:
    nvidia.com/restore-from: ${SOURCE_POD}-snapshot
spec:
  restartPolicy: Never
  runtimeClassName: nvidia
  nodeName: ${SOURCE_NODE}
  securityContext:
    fsGroup: 1000
    fsGroupChangePolicy: OnRootMismatch
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: ${CONTAINER}
    image: ${TRTLLM_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["/bin/bash", "-lc", "sleep infinity"]
    resources:
      limits:
        nvidia.com/gpu: "1"
    startupProbe:
      exec:
        command: ["cat", "/snapshot-control/restore-complete"]
      periodSeconds: 1
      failureThreshold: 1800
    readinessProbe:
      exec:
        command: ["cat", "/snapshot-control/trtllm-restore-ready"]
      periodSeconds: 1
      failureThreshold: 1800
    volumeMounts:
    - name: snapshot-control
      mountPath: /snapshot-control
    - name: tun
      mountPath: /dev/net/tun
  volumes:
  - name: snapshot-control
    emptyDir: {}
  - name: tun
    hostPath:
      path: /dev/net/tun
      type: CharDevice
EOF
```

Snapshot starts the placeholder container, restores the captured process tree,
and writes `restore-complete` into the new control volume.

## 6. Resume TensorRT-LLM

The restored lifecycle calls `resume_after_restore(llm)`. No engine
reconstruction or wake call occurs: inference uses the captured `LLM` object.

Wait for its post-restore generation result:

```bash
kubectl wait \
  --namespace "$NAMESPACE" \
  --for=condition=Ready \
  "pod/$RESTORE_POD" \
  --timeout=30m

kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- cat /snapshot-control/trtllm-restore-ready
```

A non-empty response proves that TensorRT-LLM generated text after the
captured process and CUDA state were restored.
