# Usage guides

Using Snapshot is a three-stage flow:

1. **Deploy** a replica for the inference framework. Run the framework's stock
   runtime image unmodified, with a small program mounted into it that
   cooperates with Snapshot's checkpoint/restore lifecycle. Snapshot's agent
   injects the restore tooling at runtime.
2. **Checkpoint** the running replica — with a `PodSnapshot` or a `SnapshotJob`.
3. **Restore** into new pods — with the `nvidia.com/restore-from` annotation.

Stages 2 and 3 are the same for every framework; only the image and deployment in
stage 1 differ.

> [!NOTE]
> These guides use `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop — `kubectl` here is just for illustration and
> for trying things out by hand.

## 1. Deploy

Per inference framework:

- [vLLM](vllm.md)
- [SGLang](sglang.md)
- [TensorRT-LLM](tensorrt-llm.md)

## 2. Checkpoint

- [Checkpoint a replica](checkpoint.md)

## 3. Restore

- [Restore a replica](restore.md)

See [Installation](../operations/install.md) for cluster prerequisites and the
[API reference](../reference/api.md) for full resource detail.
