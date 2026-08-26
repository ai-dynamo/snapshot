# Usage guides

Using Snapshot is a two-step model:

1. **Build a snapshot-ready image** for your inference server. Snapshot wraps your
   normal runtime image with restore tooling (CRIU, `cuda-checkpoint`, `nsrestore`)
   to produce a *placeholder* image that your replicas run.
2. **Drive checkpoint and restore through Kubernetes.** Checkpoint a replica with a
   `PodSnapshot` or a `SnapshotJob`, and add the `nvidia.com/restore-from`
   annotation to a new pod to restore it. This Kubernetes flow is the same for every
   server — only the replica image differs.

> [!NOTE]
> These guides use `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop — `kubectl` here is just for illustration and
> for trying things out by hand.

## Step 1 — build a snapshot-ready image

- [vLLM](vllm.md)
- [SGLang](sglang.md)
- [TensorRT-LLM](tensorrt-llm.md)

## Step 2 — checkpoint and restore

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
- [Use CUDA CustomStorage](custom-storage.md) — explicitly externalize CUDA checkpoint state through the Snapshot-local NIXL POSIX path.

See [Installation](../operations/install.md) for cluster prerequisites and the
[API reference](../reference/api.md) for full resource detail.
