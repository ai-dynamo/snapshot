# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

## Prerequisites

- A ready `PodSnapshot` exists (see [Checkpoint a replica](checkpoint.md)).
- The restored replica reuses the source's snapshot-ready pod spec, provided as a
  ready-to-apply `restore-deployment.yaml` for [each framework](#example).

## Example

Each build-and-deploy guide ships a ready-to-apply `restore-deployment.yaml` next
to its `deployment.yaml`: the same manifest with the
`nvidia.com/snapshot-is-checkpoint-source` label removed and an
`nvidia.com/restore-from` annotation added, naming the `PodSnapshot` to restore
from. Download the one for the framework in use:

- [vLLM `restore-deployment.yaml`](vllm/restore-deployment.yaml)
- [SGLang `restore-deployment.yaml`](sglang/restore-deployment.yaml)
- [TensorRT-LLM `restore-deployment.yaml`](tensorrt-llm/restore-deployment.yaml)

In the manifest, set the container `image` to the one built for the source and set
the `restore-from` annotation to the `PodSnapshot` name, then apply it and watch the
rollout (the Deployment is named `<framework>-restored`):

```bash
kubectl apply -f restore-deployment.yaml
kubectl rollout status deployment/<framework>-restored -n my-inference --timeout=30m
```

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
