# Checkpoint a replica

Checkpointing saves an initialized replica's state as a checkpoint artifact. There
are two ways to do it, depending on the use case:

| Method | Choose it when… | Implication |
|--------|-----------------|-------------|
| **`PodSnapshot`** | The running replica can be controlled and tracked — for example, by a controller or platform that manages inference pods | The most efficient path — it checkpoints a replica that stays serving. It needs orchestration: bringing the replica up, waiting until it is ready, then triggering the checkpoint. |
| **`SnapshotJob`** | The running pod cannot be tracked directly — for example, in a pipeline that submits the work | Snapshot runs the whole flow: it creates the replica, checkpoints it, and tears it down. Self-contained, but the source is discarded, so every replica (including the first) comes up via [restore](restore.md). |

These examples use `kubectl` to show the flow. In production, an integrating
controller or platform creates and watches these resources through the Kubernetes
API as part of its control loop.

## Prerequisites

- Snapshot is [installed](../operations/install.md) in the cluster.
- The pod to checkpoint is a **snapshot-ready pod**, fully initialized (weights
  loaded, kernels warmed up), that satisfies the
  [workload contract](../reference/workload-contract.md). Packaging the workload
  as a [custom image](README.md) is one way to meet the contract's lifecycle
  protocol. What the pod spec must carry depends on which resource creates it:
  - **`PodSnapshot`** targets a pod that already exists, so that pod's spec must
    carry everything the contract requires on its own:
    - the `/snapshot-control` volume mount, the control directory Snapshot
      signals through;
    - the `securityContext` (seccomp profile) that checkpointing requires;
    - a readiness gate on `/snapshot-control/ready-for-snapshot`, so the pod
      reports Ready only once it is safe to checkpoint.
  - **`SnapshotJob`** creates the source pod itself, so the controller injects
    the control volume and mount, the readiness probe, and the seccomp profile
    into `spec.podTemplate` before creating the source Job. The `podTemplate`
    only needs to supply the workload-specific container spec and lifecycle
    behavior (image, command, how it signals readiness) — it doesn't need to
    duplicate those controller-injected fields.

The framework guides include a complete, working example of a `PodSnapshot`-ready
pod — see the capture manifest referenced from the [vLLM](vllm.md),
[SGLang](sglang.md), and [TensorRT-LLM](tensorrt-llm.md) guides.

Set the namespace where the replica runs — the same one used to deploy it:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"
```

## Option 1 — `PodSnapshot` (checkpoint a running replica)

Point at a replica that is already up and serving. Create a `PodSnapshot` naming its
pod and the container to checkpoint:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: vllm-snapshot
spec:
  source:
    podRef:
      name: vllm-source-<pod-id>
      containers:
        - main
```

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename vllm-snapshot.yaml

kubectl wait \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Ready podsnapshot/vllm-snapshot \
  --timeout=30m
```

The operator binds a cluster-scoped `PodSnapshotContent` and records the artifact.
Because the replica keeps running and serving, this is the faster path — the
trade-off is the orchestration it requires: bringing the replica up, waiting for
readiness, then triggering the checkpoint.

## Option 2 — `SnapshotJob` (checkpoint a throwaway replica)

`SnapshotJob` runs a replica from a pod template, checkpoints it once ready, and
completes from the resulting `PodSnapshot` — removing the source replica. There is
no long-running replica to manage, which fits pipeline use cases.

`spec.podTemplate` only needs the workload's own container spec — image,
command, resources, and any volumes it mounts. The framework recipes do not
need `/dev/net/tun`. CRIU can log a nonfatal TUN capability-probe error when
the device is absent. See [TUN probe errors](../operations/troubleshooting.md#criu-reports-unable-to-create-tun).
The controller injects the `/snapshot-control`
volume and mount, `SNAPSHOT_CONTROL_DIR`, the `ready-for-snapshot` readiness
probe, and the seccomp profile before creating the source pod.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: vllm-snapshot-job
spec:
  podSnapshotTemplate:
    targetContainers:
      - main
  podTemplate:
    spec:
      runtimeClassName: nvidia
      nodeSelector:
        nvidia.com/gpu.present: "true"
      containers:
        - name: main
          image: vllm/vllm-openai:v0.31.0-ubuntu2404@sha256:3fb023cc987cc7c2bca7ca14c1e0aff5ae06bd9431bf07773731ce7e8a682460
          command:
            - python3
            - /snapshot-app/app.py
          args:
            - '{"model": "Qwen/Qwen3-0.6B"}'
          resources:
            limits:
              nvidia.com/gpu: "1"
          volumeMounts:
            - name: app
              mountPath: /snapshot-app
              readOnly: true
      volumes:
        - name: app
          configMap:
            name: vllm-app
```

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename vllm-snapshot-job.yaml

kubectl wait \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Completed snapshotjob/vllm-snapshot-job \
  --timeout=30m

# the resulting PodSnapshot to restore from:
kubectl get snapshotjob vllm-snapshot-job \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --output jsonpath='{.status.podSnapshotName}'
```

Because the source replica is deleted, every serving replica — including the
first — is brought up via [restore](restore.md).

## Single-GPU and multi-GPU capture

Both options capture single-GPU and multi-GPU workloads on one node. The
framework guides ship a capture manifest for each combination:

| Framework | Model | GPUs | `PodSnapshot` source (Deployment) | `SnapshotJob` |
|-----------|-------|------|-----------------------------------|---------------|
| vLLM | Qwen3 0.6B | 1 | [qwen3-0.6b.yaml](vllm/capture/qwen3-0.6b.yaml) | [qwen3-0.6b-snapshotjob.yaml](vllm/capture/qwen3-0.6b-snapshotjob.yaml) |
| vLLM | GLM 5.3 NVFP4 | 8 | [glm-5.3.yaml](vllm/capture/glm-5.3.yaml) | [glm-5.3-snapshotjob.yaml](vllm/capture/glm-5.3-snapshotjob.yaml) |
| vLLM | DeepSeek V4.1 Flash | 8 | [deepseek-v4.1-flash.yaml](vllm/capture/deepseek-v4.1-flash.yaml) | [deepseek-v4.1-flash-snapshotjob.yaml](vllm/capture/deepseek-v4.1-flash-snapshotjob.yaml) |
| SGLang | Qwen3 0.6B | 1 | [qwen3-0.6b.yaml](sglang/capture/qwen3-0.6b.yaml) | [qwen3-0.6b-snapshotjob.yaml](sglang/capture/qwen3-0.6b-snapshotjob.yaml) |
| SGLang | GLM 5.3 NVFP4 | 8 | [glm-5.3.yaml](sglang/capture/glm-5.3.yaml) | [glm-5.3-snapshotjob.yaml](sglang/capture/glm-5.3-snapshotjob.yaml) |
| SGLang | DeepSeek V4.1 Flash | 8 | [deepseek-v4.1-flash.yaml](sglang/capture/deepseek-v4.1-flash.yaml) | [deepseek-v4.1-flash-snapshotjob.yaml](sglang/capture/deepseek-v4.1-flash-snapshotjob.yaml) |
| TensorRT-LLM | Qwen3 0.6B | 1 | [qwen3-0.6b.yaml](tensorrt-llm/capture/qwen3-0.6b.yaml) | [qwen3-0.6b-snapshotjob.yaml](tensorrt-llm/capture/qwen3-0.6b-snapshotjob.yaml) |
| TensorRT-LLM | GLM 5.3 NVFP4 | 8 | [glm-5.3.yaml](tensorrt-llm/capture/glm-5.3.yaml) | [glm-5.3-snapshotjob.yaml](tensorrt-llm/capture/glm-5.3-snapshotjob.yaml) |

A multi-GPU capture enables multi-GPU support with the
`nvidia.com/snapshot-multi-gpu: enabled` annotation on the source Pod. The
eight-GPU manifests also mount the shared [model cache](model-cache-pvc.yaml)
that the framework guides create.

### With a `SnapshotJob`

Add the annotation to `spec.podTemplate`. The controller prepares the source
Pod for multi-GPU capture:

<table>
<tr><th>Single GPU</th><th>Multi-GPU</th></tr>
<tr><td>

```yaml
spec:
  podTemplate:
    spec:
      containers:
        - name: main
          command:
            - python3
            - /snapshot-app/app.py
          resources:
            limits:
              nvidia.com/gpu: "1"
```

</td><td>

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/snapshot-multi-gpu: enabled
    spec:
      containers:
        - name: main
          command:
            - python3
            - /snapshot-app/app.py
          resources:
            limits:
              nvidia.com/gpu: "8"
```

</td></tr>
</table>

### With a Deployment and `PodSnapshot`

A Pod that Snapshot does not create must also prepare itself. It copies the
multi-GPU libraries from the Snapshot agent image with an init container, and
starts the workload through the launcher those libraries include:

<table>
<tr><th>Single GPU</th><th>Multi-GPU</th></tr>
<tr><td>

```yaml
template:
  spec:
    containers:
      - name: main
        command:
          - python3
          - /snapshot-app/app.py
        resources:
          limits:
            nvidia.com/gpu: "1"
```

</td><td>

```yaml
template:
  metadata:
    annotations:
      nvidia.com/snapshot-multi-gpu: enabled
  spec:
    initContainers:
      - name: snapshot-cuda-install
        image: ${SNAPSHOT_AGENT_IMAGE}
        command:
          - /bin/cp
        args:
          - --preserve=mode
          - --
          - /usr/local/lib/snapshot/libcuinterpose.so
          - /usr/local/lib/snapshot/libcuinterpose_core.so
          - /usr/local/bin/cuinterpose-launch
          - /tmp/snapshot-cuda/
        volumeMounts:
          - name: snapshot-cuda
            mountPath: /tmp/snapshot-cuda
    containers:
      - name: main
        command:
          - /tmp/snapshot-cuda/cuinterpose-launch
          - --library
          - /tmp/snapshot-cuda/libcuinterpose.so
          - --
          - python3
          - /snapshot-app/app.py
        resources:
          limits:
            nvidia.com/gpu: "8"
        volumeMounts:
          - name: snapshot-cuda
            mountPath: /tmp/snapshot-cuda
            readOnly: true
    volumes:
      - name: snapshot-cuda
        emptyDir: {}
```

</td></tr>
</table>

Set `SNAPSHOT_AGENT_IMAGE` to the image of the installed Snapshot agent, and
substitute it when applying the manifest:

```bash
export SNAPSHOT_AGENT_IMAGE=<registry>/snapshot/agent@sha256:<digest>

envsubst '${SNAPSHOT_AGENT_IMAGE}' < capture/glm-5.3.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

`envsubst` comes with `gettext-base` on Debian and Ubuntu. Then create the
`PodSnapshot` for the ready source Pod as in
[Option 1](#option-1--podsnapshot-checkpoint-a-running-replica).
