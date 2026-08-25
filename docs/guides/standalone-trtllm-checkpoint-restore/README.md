<!--
SPDX-FileCopyrightText: 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint and restore TensorRT-LLM with Snapshot

This guide shows how to stop a TensorRT-LLM application at an idle engine
boundary, capture its Kubernetes pod with Snapshot, and verify inference from
the restored process.

Snapshot must already be installed in the cluster. The guide covers a
single-GPU Python application that owns a TensorRT-LLM `LLM` instance. A server
must provide its own request-draining hook before using the same lifecycle.

Companion examples:

- [`snapshot_lifecycle.py`](snapshot_lifecycle.py)
- [Source pod fields](trtllm-source-pod-fields.yaml)
- [`PodSnapshot`](trtllm-snapshot.yaml)
- [Restore pod](trtllm-restore.yaml)

## Prerequisites

- An x86_64 Kubernetes cluster with an NVIDIA Ampere, Ada Lovelace, Hopper, or
  Blackwell GPU node.
- Snapshot installed, including its operator, node agent, and CRDs.
- NVIDIA driver 580 or newer with MIG disabled on the target node.
- A TensorRT-LLM Python application and pod manifest that you can modify.
- A single-GPU text model that the selected TensorRT-LLM image supports.
- The same immutable image and source GPU node available for restore.

Use the namespace from the current context, or set `NAMESPACE` before running
this block. Select the TensorRT-LLM pod; the remaining values are derived:

```bash
NAMESPACE="${NAMESPACE:-$(kubectl config view --minify --output 'jsonpath={..namespace}')}"
NAMESPACE="${NAMESPACE:-default}"
SOURCE_POD="<trtllm-pod-name>"
CONTAINER="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.spec.containers[0].name}')"
RESTORE_POD="${SOURCE_POD}-restore"
SOURCE_NODE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.spec.nodeName}')"
TRTLLM_IMAGE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output "jsonpath={.status.containerStatuses[?(@.name=='${CONTAINER}')].imageID}")"
TRTLLM_IMAGE="${TRTLLM_IMAGE#*://}"
```

## 1. Add the TensorRT-LLM lifecycle

TensorRT-LLM does not need the vLLM sleep and wake sequence for this snapshot
path. The application instead:

1. Disables NCCL symmetric zero-copy before importing TensorRT-LLM.
2. Creates the engine and finishes every in-flight `generate()` call.
3. Stops admitting requests and runs Python garbage collection.
4. Signals that the idle, GPU-resident engine is ready for capture.
5. Waits until the restored pod reports `restore-complete`.
6. Uses the same restored `LLM` object for inference.

`TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` is required because CUDA checkpoint cannot
capture NCCL registered windows. Do not call TensorRT-LLM sleep for this flow:
releasing the allocations would remove the initialized state that Snapshot is
meant to preserve.

Copy [`snapshot_lifecycle.py`](snapshot_lifecycle.py) into the application
source. In the entrypoint, set the environment before any TensorRT-LLM import,
create the engine, and call the two lifecycle phases:

```python
import os

os.environ["TLLM_NCCL_SYMMETRIC_ZERO_COPY"] = "0"

from tensorrt_llm import LLM, SamplingParams

from snapshot_lifecycle import quiesce_for_snapshot, resume_after_restore

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
```

`quiesce_for_snapshot()` blocks. In the source pod, Snapshot terminates the
captured process after the artifact is committed. In the restore pod, the
restored process resumes inside the same function, observes
`restore-complete`, and returns to `resume_after_restore(llm)`.

Rebuild the application image after adding this code.

## 2. Prepare the source pod

Apply the fields from
[`trtllm-source-pod-fields.yaml`](trtllm-source-pod-fields.yaml) to the
TensorRT-LLM pod manifest before deploying it.

The important fields are:

- `TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` before engine creation.
- `UCX_TLS=tcp,self` to avoid RDMA mappings that CRIU cannot restore.
- `/snapshot-control` for lifecycle signals.
- `/dev/net/tun` for restoring network devices created by the
  TensorRT-LLM, OpenMPI, and PyTorch stack.
- A readiness probe that waits for `ready-for-snapshot`.

Deploy the updated image and wait for the lifecycle to reach the capture
boundary:

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

At this point the `LLM` object, model weights, CUDA state, and initialized
TensorRT-LLM runtime remain resident, but the application has no in-flight
generation.

## 3. Capture the pod

Create a `PodSnapshot` to ask Snapshot to capture the selected container:

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

Snapshot creates the cluster-scoped `PodSnapshotContent` that records the
physical artifact. Inspect both resources:

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

## 4. Create the restore pod

The restore pod uses the same immutable image and GPU node. Its
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

## 5. Verify restored inference

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
