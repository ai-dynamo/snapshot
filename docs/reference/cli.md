# `snapshotctl` CLI

`snapshotctl` is a lower-level utility for checkpointing and restoring a pod
directly from a pod manifest. It is not the primary path — most users drive
Snapshot through the [Kubernetes resources](../guides/README.md) — but it is handy
for validation and debugging, and it is a quick way to try checkpoint/restore by
hand.

## Requirements

- The Snapshot Helm chart is installed in the target namespace, with the
  `snapshot-agent` DaemonSet running and the checkpoint PVC mounted.
- `checkpoint` requires the operator (it resolves the `PodSnapshot` into a
  checkpoint). `restore` is handled by the agent directly from pod annotations.

## Checkpoint

`snapshotctl checkpoint` creates a `PodSnapshot` from a pod manifest and waits for
the agent to checkpoint it:

```bash
snapshotctl checkpoint \
  --manifest ./vllm-replica-pod.yaml \
  --snapshot vllm-snapshot \
  --container main \
  --namespace my-inference
```

The manifest must be a `Pod` (not a Deployment or Job) using a
[snapshot-ready image](../guides/README.md).

## Restore

`snapshotctl restore` creates a new pod from a manifest and restores it from a
named `PodSnapshot`:

```bash
snapshotctl restore \
  --manifest ./vllm-replica-pod.yaml \
  --snapshot vllm-snapshot \
  --namespace my-inference
```

The restore manifest must contain a container with the same name checkpointed by that
`PodSnapshot`. `snapshotctl` returns once the restore is submitted — watch the
pod's `nvidia.com/Restored` status condition, readiness, and events for progress.

The source README for the tool lives at
[`operator/cmd/snapshotctl/README.md`](../../operator/cmd/snapshotctl/README.md).
