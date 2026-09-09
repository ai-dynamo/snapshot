# Usage guides

Using Snapshot is a three-stage flow:

1. **Make the workload snapshot-ready.** The workload must satisfy the
   [workload contract](../reference/workload-contract.md): an entrypoint that
   cooperates with the checkpoint/restore lifecycle, and a pod that carries the
   control volume, seccomp profile, and readiness gate. Building a custom image
   from the framework's runtime image is the reference way to package this — the
   per-framework guides below use it. Deploy the result as a replica; Snapshot's
   agent injects the restore tooling at runtime.
2. **Checkpoint** the running replica — with a `PodSnapshot` or a `SnapshotJob`.
3. **Restore** into new pods — with the `nvidia.com/restore-from` annotation.

Stages 2 and 3 are the same for every framework; only the workload and
deployment in stage 1 differ.

> [!NOTE]
> These guides use `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop — `kubectl` here is just for illustration and
> for trying things out by hand.

## 1. Make the workload snapshot-ready

The reference method — build a snapshot-ready image, then deploy it — per
inference framework:

- [vLLM](vllm.md)
- [SGLang](sglang.md)
- [TensorRT-LLM](tensorrt-llm.md)

## 2. Checkpoint

- [Checkpoint a replica](checkpoint.md)

## 3. Restore

- [Restore a replica](restore.md)

See [Installation](../operations/install.md) for cluster prerequisites and the
[API reference](../reference/api.md) for full resource detail.
