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

Each framework guide ships model-specific manifests under `capture/` and
Pod profiles under `restore/`: `single-gpu.yaml` and `8-gpu.yaml`. Choose the
profile matching the source GPU count, image, and mounts. Model configuration
comes from the checkpoint.
The restore manifest adds the `nvidia.com/restore-from` annotation naming the
`PodSnapshot` to restore from and replaces the container command with an inert
`sleep infinity`.
The same restore manifest works for captures made with or without SnapshotJob.
Download the one for the framework in use:

- vLLM: [single GPU](vllm/restore/single-gpu.yaml), [eight GPUs](vllm/restore/8-gpu.yaml)
- SGLang: [single GPU](sglang/restore/single-gpu.yaml), [eight GPUs](sglang/restore/8-gpu.yaml)
- TensorRT-LLM: [single GPU](tensorrt-llm/restore/single-gpu.yaml), [eight GPUs](tensorrt-llm/restore/8-gpu.yaml)
- [Multi-GPU examples](cuda-shared-memory.md#multi-gpu-models)

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

The container starts as an inert `sleep infinity` placeholder, as the
[restore Pod contract](../reference/restore-pod-contract.md) requires: the agent
restores the checkpointed process into it as a sibling, so running the
application there would only load a second copy of the model. Readiness
therefore waits on the framework's post-restore sentinel — `vllm-restore-ready`
and its equivalents — not the `ready-for-snapshot` file the source pod writes.

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
