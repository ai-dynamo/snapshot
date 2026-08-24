<!--
SPDX-FileCopyrightText: 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint and restore vLLM

This guide shows how to checkpoint and restore a standalone vLLM process with
Snapshot. Dynamo is not installed or used.

The flow uses `PodSnapshot`, the Snapshot operator, and the node agent directly.
It does not use `SnapshotJob` or `snapshotctl`.

Snapshot is under active development and is not production-ready. Pin immutable
Snapshot and workload image versions, and validate the complete flow on the
target GPU and driver combination.

The standalone vLLM end-to-end test in
[PR #104](https://github.com/ai-dynamo/snapshot/pull/104) is an executable
example of this flow.

## How the flow works

1. The source pod initializes vLLM and handles test generation requests.
2. The workload calls `pause_generation()` and `sleep()`.
3. The workload writes `/snapshot-control/ready-for-snapshot`.
4. A `PodSnapshot` requests capture of the source container.
5. The Snapshot operator creates a `PodSnapshotContent`, and the node agent
   captures the process, CPU memory, GPU state, and root filesystem changes.
6. The node agent writes `/snapshot-control/snapshot-complete`.
7. A new inert pod starts with the same workload image and references the
   checkpoint ID.
8. The node agent restores the captured process into that pod and writes
   `/snapshot-control/restore-complete`.
9. The restored process calls `wake_up()` and `resume_generation()`, then
   verifies inference.

The application owns the vLLM-specific lifecycle. Snapshot owns the Kubernetes,
CRIU, CUDA checkpoint, artifact, and restore mechanics.

## Requirements

- An x86_64 Kubernetes cluster with NVIDIA GPU nodes.
- NVIDIA driver 580 or newer.
- GPU Operator 26.3.0 or newer and `RuntimeClass/nvidia`.
- containerd or CRI-O.
- Permission to run the privileged Snapshot agent DaemonSet.
- A storage class that supports `ReadWriteMany`.
- `kubectl`, Helm, Docker Buildx, Go, and access to a container registry that
  the GPU nodes can pull from.
- MIG disabled on the GPU node used by this example.

Use the same workload image digest for capture and restore. The target node must
also be compatible with the source node's GPU model, driver, kernel, mounts, and
other captured runtime properties.

## 1. Set the environment

Set these values once. All later commands derive from them.

```bash
export SNAPSHOT_VERSION=<immutable-published-snapshot-tag>
export NAMESPACE=snapshot-vllm
export STORAGE_CLASS=<rwx-storage-class>
export VLLM_IMAGE=<registry>/snapshot-vllm:v0.27.1
export MODEL=Qwen/Qwen3-0.6B
export CHECKPOINT_ID=vllm-qwen3-06b
```

Create the namespace:

```bash
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml |
  kubectl apply -f -
```

## 2. Install Snapshot

Install the chart, operator, agent, CRDs, seccomp profile, and checkpoint PVC
from the same immutable Snapshot version:

```bash
helm show chart oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version "${SNAPSHOT_VERSION#v}"

helm upgrade --install snapshot \
  oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version "${SNAPSHOT_VERSION#v}" \
  --namespace "$NAMESPACE" \
  --set "image.operator.tag=${SNAPSHOT_VERSION}" \
  --set image.operator.pullPolicy=Always \
  --set "image.agent.tag=${SNAPSHOT_VERSION}" \
  --set image.agent.pullPolicy=Always \
  --set storage.pvc.create=true \
  --set "storage.pvc.storageClass=${STORAGE_CLASS}" \
  --set storage.pvc.size=64Gi \
  --set config.criu.mntnsCompatMode=true \
  --wait \
  --timeout=15m
```

Verify the installation:

```bash
kubectl rollout status deployment/snapshot-operator \
  --namespace "$NAMESPACE" \
  --timeout=10m
kubectl rollout status daemonset/snapshot-agent \
  --namespace "$NAMESPACE" \
  --timeout=10m
kubectl get crd podsnapshots.nvidia.com podsnapshotcontents.nvidia.com
kubectl get pvc snapshot-pvc --namespace "$NAMESPACE"
```

## 3. Make vLLM snapshot-aware

The workload must quiesce vLLM before it reports readiness and resume vLLM only
after restore completes.

Create `snapshot_vllm.py`:

```python
import asyncio
import os
from pathlib import Path

from vllm import SamplingParams
from vllm.engine.arg_utils import AsyncEngineArgs
from vllm.usage.usage_lib import UsageContext
from vllm.v1.engine.async_llm import AsyncLLM


control_dir = Path(os.environ["SNAPSHOT_CONTROL_DIR"])


async def generate_text(engine, prompt, request_id):
    result = None
    async for output in engine.generate(
        prompt,
        SamplingParams(temperature=0.0, max_tokens=8),
        request_id,
    ):
        result = output
    if result is None or not result.outputs:
        raise RuntimeError(f"vLLM produced no output for {request_id}")
    text = result.outputs[0].text.strip()
    if not text:
        raise RuntimeError(f"vLLM produced empty output for {request_id}")
    return text


async def main():
    engine = AsyncLLM.from_engine_args(
        AsyncEngineArgs(
            model=os.environ["MODEL"],
            dtype="half",
            max_model_len=512,
            gpu_memory_utilization=0.35,
            enforce_eager=True,
            enable_sleep_mode=True,
            disable_log_stats=True,
        ),
        usage_context=UsageContext.LLM_CLASS,
    )

    await generate_text(
        engine,
        "Summarize why checkpoint and restore testing matters.",
        "snapshot-preflight",
    )
    await engine.pause_generation()
    await engine.sleep()
    (control_dir / "ready-for-snapshot").write_text("ready\n", encoding="utf-8")

    while True:
        if (control_dir / "snapshot-complete").exists():
            return
        if (control_dir / "restore-complete").exists():
            await engine.wake_up()
            await engine.resume_generation()
            await engine.check_health()
            text = await generate_text(
                engine,
                "Reply with one word: ready",
                "snapshot-restore-smoke",
            )
            (control_dir / "vllm-restore-ready").write_text(
                text + "\n",
                encoding="utf-8",
            )
            while True:
                await asyncio.sleep(3600)
        await asyncio.sleep(1)


if __name__ == "__main__":
    asyncio.run(main())
    os._exit(0)
```

Level 1 sleep offloads model weights and discards the KV cache. The pre-capture
request proves that the engine handled real work before quiescing; it does not
preserve that request's KV cache through restore.

## 4. Build the vLLM image

The Snapshot restore bundle is built on Ubuntu 24.04. The upstream
`vllm/vllm-openai:v0.27.1` image uses Ubuntu 22.04 and cannot load that bundle
without a compatible glibc. The following image is suitable for this test:

```dockerfile
ARG BASE_IMAGE=vllm/vllm-openai:v0.27.1
FROM ${BASE_IMAGE}

USER root

RUN set -eux; \
    printf 'deb http://archive.ubuntu.com/ubuntu noble main universe\n' \
      >/etc/apt/sources.list.d/snapshot-noble.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends -t noble libc6 libc-bin; \
    rm -f /etc/apt/sources.list.d/snapshot-noble.list; \
    rm -rf /var/lib/apt/lists/*

COPY snapshot_vllm.py /opt/snapshot_vllm.py
```

Save it as `Dockerfile.snapshot-vllm`, then build and push:

```bash
docker buildx build \
  --platform linux/amd64 \
  --file Dockerfile.snapshot-vllm \
  --tag "$VLLM_IMAGE" \
  --push \
  .
```

For a production image, build vLLM on an Ubuntu 24.04 base instead of upgrading
glibc across Ubuntu releases.

## 5. Create the pod manifests

Create `/tmp/vllm-source.yaml`:

```bash
cat >/tmp/vllm-source.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: vllm-source
  namespace: ${NAMESPACE}
  labels:
    nvidia.com/snapshot-is-checkpoint-source: "true"
    nvidia.com/snapshot-checkpoint-id: ${CHECKPOINT_ID}
  annotations:
    nvidia.com/snapshot-target-containers: main
spec:
  restartPolicy: Never
  runtimeClassName: nvidia
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: main
    image: ${VLLM_IMAGE}
    imagePullPolicy: Always
    command: ["python3", "/opt/snapshot_vllm.py"]
    env:
    - name: SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    - name: MODEL
      value: ${MODEL}
    - name: HF_HUB_DISABLE_XET
      value: "1"
    - name: NCCL_CUMEM_ENABLE
      value: "0"
    - name: NCCL_NVLS_ENABLE
      value: "0"
    - name: NCCL_IB_DISABLE
      value: "1"
    - name: NCCL_RAS_ENABLE
      value: "0"
    resources:
      limits:
        nvidia.com/gpu: "1"
    readinessProbe:
      exec:
        command:
        - cat
        - /snapshot-control/ready-for-snapshot
      periodSeconds: 1
      failureThreshold: 1200
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

## 6. Capture the vLLM process

Start the source pod and wait until vLLM has quiesced:

```bash
kubectl apply -f /tmp/vllm-source.yaml
kubectl wait \
  --for=condition=Ready \
  pod/vllm-source \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

Create `/tmp/vllm-snapshot.yaml`, pinning the source pod UID:

```bash
cat >/tmp/vllm-snapshot.yaml <<EOF
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: vllm-source-snapshot
  namespace: ${NAMESPACE}
  labels:
    nvidia.com/snapshot-checkpoint-id: ${CHECKPOINT_ID}
spec:
  source:
    podRef:
      name: vllm-source
      uid: $(kubectl get pod vllm-source \
        --namespace "$NAMESPACE" \
        --output jsonpath='{.metadata.uid}')
      containers:
      - main
EOF

kubectl apply -f /tmp/vllm-snapshot.yaml
kubectl wait \
  --for=condition=Ready \
  podsnapshot/vllm-source-snapshot \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

Inspect the API objects:

```bash
kubectl get podsnapshot vllm-source-snapshot \
  --namespace "$NAMESPACE" \
  -o wide
kubectl get podsnapshotcontent
```

The `PodSnapshot` must report `Ready=True`. Its
`status.boundSnapshotContentName` identifies the system-created
`PodSnapshotContent`. Do not create `PodSnapshotContent` yourself.

## 7. Restore the checkpoint

Create `/tmp/vllm-restore.yaml` while the source pod still records its node:

```bash
cat >/tmp/vllm-restore.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: vllm-restore
  namespace: ${NAMESPACE}
  labels:
    nvidia.com/snapshot-checkpoint-id: ${CHECKPOINT_ID}
    nvidia.com/snapshot-is-restore-target: "true"
  annotations:
    nvidia.com/snapshot-target-containers: main
    nvidia.com/snapshot-artifact-version: "1"
spec:
  restartPolicy: Never
  runtimeClassName: nvidia
  nodeName: $(kubectl get pod vllm-source \
    --namespace "$NAMESPACE" \
    --output jsonpath='{.spec.nodeName}')
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: main
    image: ${VLLM_IMAGE}
    imagePullPolicy: Always
    command: ["/bin/bash", "-lc", "sleep infinity"]
    env:
    - name: SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    - name: MODEL
      value: ${MODEL}
    - name: HF_HUB_DISABLE_XET
      value: "1"
    - name: NCCL_CUMEM_ENABLE
      value: "0"
    - name: NCCL_NVLS_ENABLE
      value: "0"
    - name: NCCL_IB_DISABLE
      value: "1"
    - name: NCCL_RAS_ENABLE
      value: "0"
    resources:
      limits:
        nvidia.com/gpu: "1"
    startupProbe:
      exec:
        command:
        - cat
        - /snapshot-control/restore-complete
      periodSeconds: 1
      failureThreshold: 1200
    readinessProbe:
      exec:
        command:
        - /bin/bash
        - -lc
        - test -s /snapshot-control/vllm-restore-ready
      periodSeconds: 1
      failureThreshold: 1200
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

The restore container must remain inert until the node agent replaces it with
the restored process tree. Starting a second vLLM engine in the restore pod can
consume the GPU and race the restore.

Delete the source pod to release the GPU, then create the restore target:

```bash
kubectl delete pod vllm-source \
  --namespace "$NAMESPACE" \
  --wait=true
kubectl apply -f /tmp/vllm-restore.yaml
```

Wait for restore and post-restore inference:

```bash
kubectl wait \
  --for=condition=Ready \
  pod/vllm-restore \
  --namespace "$NAMESPACE" \
  --timeout=20m

kubectl get pod vllm-restore \
  --namespace "$NAMESPACE" \
  -o json |
  jq -r '.metadata.annotations["nvidia.com/snapshot-restore-status.main"]'

kubectl exec vllm-restore \
  --namespace "$NAMESPACE" \
  -- awk '{print}' /snapshot-control/vllm-restore-ready
```

The restore status must be `completed`, and the result file must contain the
post-restore model response.

## 8. Inspect the artifact

The agent stores version 1 under
`/checkpoints/<checkpoint-id>/versions/1` on the Snapshot PVC.

```bash
kubectl exec \
  --namespace "$NAMESPACE" \
  "$(kubectl get pod \
    --namespace "$NAMESPACE" \
    --selector app.kubernetes.io/component=snapshot-agent \
    --output jsonpath='{.items[0].metadata.name}')" \
  -- ls -lah "/checkpoints/${CHECKPOINT_ID}/versions/1"
```

`manifest.yaml` records the source pod, image digest, node, GPU, driver,
checkpoint settings, and restore inputs. The remaining files contain CRIU
images, process memory, CUDA state, and captured root filesystem changes.

## 9. Clean up

```bash
kubectl delete pod vllm-source \
  --namespace "$NAMESPACE" \
  --ignore-not-found
kubectl delete pod vllm-restore \
  --namespace "$NAMESPACE" \
  --ignore-not-found
kubectl delete podsnapshot vllm-source-snapshot \
  --namespace "$NAMESPACE" \
  --ignore-not-found
```

Deleting the `PodSnapshot` removes its API record. The chart retains checkpoint
PVC data. Delete the PVC only when all retained artifacts can be discarded:

```bash
helm uninstall snapshot --namespace "$NAMESPACE"
kubectl delete pvc snapshot-pvc \
  --namespace "$NAMESPACE" \
  --ignore-not-found
```

## Troubleshooting

- `GLIBC_2.38 not found`: the workload image is older than the Snapshot restore
  bundle. Use an Ubuntu 24.04-compatible image.
- `tun: Unable to create tun`: mount the host `/dev/net/tun` character device in
  both source and restore manifests.
- A missing Hugging Face Xet log blocks CRIU restore: set
  `HF_HUB_DISABLE_XET=1`.
- `mnt-v2` errors for NVIDIA devices: install Snapshot with
  `config.criu.mntnsCompatMode=true`.
- `CUDA_ERROR_UNKNOWN` during capture: upgrade the NVIDIA driver to 580 or
  newer.
- A restore pod starts a second vLLM process: override its command with an inert
  process such as `sleep infinity`.
- The source and restore image digests differ: rebuild or retag once, then use
  the same immutable digest in both manifests.
- Restore completes but inference does not: verify the restored application
  observes `restore-complete`, then calls `wake_up()` before
  `resume_generation()`.
