# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

## Prerequisites

- A ready `PodSnapshot` exists (see [Checkpoint a replica](checkpoint.md)).
- The restored replica reuses the source's snapshot-ready pod spec, provided as a
  manifest under `restore/` for [each framework](#example).

## Example

Restore takes the same steps for single-GPU and multi-GPU snapshots. Only the
Pod profile differs. Each framework guide ships model-specific manifests under
`capture/` and two restore profiles under `restore/`. Choose the profile that
matches the source's GPU count:

| Framework | Single GPU | Eight GPUs |
|-----------|------------|------------|
| vLLM | [single-gpu.yaml](vllm/restore/single-gpu.yaml) | [8-gpu.yaml](vllm/restore/8-gpu.yaml) |
| SGLang | [single-gpu.yaml](sglang/restore/single-gpu.yaml) | [8-gpu.yaml](sglang/restore/8-gpu.yaml) |
| TensorRT-LLM | [single-gpu.yaml](tensorrt-llm/restore/single-gpu.yaml) | [8-gpu.yaml](tensorrt-llm/restore/8-gpu.yaml) |

A profile works for every model of its framework and GPU count, because the
model configuration comes from the checkpoint. It also works for captures made
with or without SnapshotJob. The profile carries the `nvidia.com/restore-from`
annotation naming the `PodSnapshot` to restore from.

Set the namespace where the restored replica will run — the same one holding the
`PodSnapshot`:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"
```

In the manifest, use the same container `image` as the source and set
the `restore-from` annotation to the `PodSnapshot` name, then apply it and watch the
rollout (the Deployment is named `<framework>-restored`). For eight GPUs, use
`restore/8-gpu.yaml` and Deployment `<framework>-8gpu-restored`:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename restore/single-gpu.yaml

kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/<framework>-restored \
  --timeout=30m
```

Snapshot replaces the container's command with its restore proxy, as the
[restore Pod contract](../reference/restore-pod-contract.md) describes, so the
application does not start from scratch. The proxy waits for the restore, then
follows the restored process: the container ends when the restored process
does. Readiness waits on the framework's post-restore sentinel —
`vllm-restore-ready` and its equivalents — not the `ready-for-snapshot` file the
source pod writes.

A Pod gets one restore. The guides use a Deployment, which restarts containers,
so if the restored process exits, the container ends in `CrashLoopBackOff`
instead of starting again. Delete the Pod to restore a new replica.

The node agent adds a `nvidia.com/Restored` condition to the pod once the restore
completes — watch it, along with pod readiness, to confirm. If the restored
workload serves an API, sending a request is a good end-to-end check that it
resumed correctly.

The restored process resumes from the checkpointed state, skipping model loading
and warm-up. In practice, higher-level systems create these restored Deployments
rather than applying them by hand. To generate restore pods programmatically —
from a controller, operator, or serving platform — implement the
[restore Pod contract](../reference/restore-pod-contract.md), which specifies the
required annotations, control volume, and `SNAPSHOT_CONTROL_DIR`, plus the
optional startup gate and seccomp profile a restored pod may carry.
