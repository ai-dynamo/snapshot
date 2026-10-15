# Usage guides

Using Snapshot is a three-stage flow:

1. **Make the workload snapshot-ready** — the per-framework guides below run
   the framework's runtime image unmodified and mount an entrypoint into it
   from a ConfigMap, implementing the
   [workload contract](../reference/workload-contract.md).
2. **Checkpoint** the running replica — with a `PodSnapshot` or a `SnapshotJob`.
3. **Restore** into new pods — with the `nvidia.com/restore-from` annotation.

Stages 2 and 3 are the same for every framework. Each stage also works the same
way for single-GPU and multi-GPU workloads, with these differences:

| Stage | Multi-GPU difference |
|-------|----------------------|
| 1. Snapshot-ready | Engine parallelism settings, such as the tensor-parallel size, and the [multi-GPU requirements](#multi-gpu-requirements) |
| 2. Checkpoint | The capture manifest carries the `nvidia.com/snapshot-multi-gpu: enabled` annotation. See [Single-GPU and multi-GPU capture](checkpoint.md#single-gpu-and-multi-gpu-capture) |
| 3. Restore | The `8-gpu.yaml` restore profile instead of `single-gpu.yaml` |

> [!NOTE]
> These guides use `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop — `kubectl` here is just for illustration and
> for trying things out by hand.

## Multi-GPU requirements

- All GPUs of the workload are on one node. Multi-node is on the roadmap.
- The [driver versions](../../README.md#prerequisites) Snapshot requires.
- A node with the GPU count, CPU, and memory the manifests request. The
  multi-GPU manifests select eight B200 GPUs. Change `nodeSelector` and the
  parallelism settings for other GPUs.
- A ReadWriteMany storage class for the shared [model cache](model-cache-pvc.yaml).

## 1. Make the workload snapshot-ready

Per inference framework, mount an entrypoint into the framework's runtime
image and deploy it:

- [vLLM](vllm.md)
- [SGLang](sglang.md)
- [TensorRT-LLM](tensorrt-llm.md)

## 2. Checkpoint

- [Checkpoint a replica](checkpoint.md)

## 3. Restore

- [Restore a replica](restore.md)

See [Installation](../operations/install.md) for cluster prerequisites and the
[API reference](../reference/api.md) for full resource detail.
