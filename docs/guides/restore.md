# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

## Prerequisites

- A ready `PodSnapshot` exists (see [Checkpoint a replica](checkpoint.md)).
- The restored replica reuses the source's snapshot-ready pod spec, provided as a
  ready-to-apply manifest under `restore/` for [each framework](#example).

## Example

Each engine has matching model filenames under `capture/` and `restore/`.
The restore manifest keeps the workload mounts and image, with an
`nvidia.com/restore-from` annotation added naming the `PodSnapshot` to restore
from, and the container command replaced with an inert `sleep infinity`.
The restore agent supplies the CUDA shared-memory libraries, so the placeholder
does not run the source's installer or launcher. Download the one for the
framework in use:

- [vLLM](vllm/restore/qwen3-0.6b.yaml)
- [SGLang](sglang/restore/qwen3-0.6b.yaml)
- [TensorRT-LLM](tensorrt-llm/restore/qwen3-0.6b.yaml)

Set the namespace where the restored replica will run — the same one holding the
`PodSnapshot`:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"
```

These links select the single-GPU Qwen3 example. For a multi-GPU model, use the
matching file from the [model table](cuda-shared-memory.md#multi-gpu-models).
Keep the same immutable engine image and GPU count as the source, and set the
`restore-from` annotation to the `PodSnapshot` name. Apply the manifest and watch
the rollout (the single-GPU Deployment is named `<framework>-restored`):

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename restore/qwen3-0.6b.yaml

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
