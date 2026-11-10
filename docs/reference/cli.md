# `snapshotctl` CLI

`snapshotctl` is a command-line tool that checkpoints and restores a pod
directly from a pod manifest. Most users do not need it, because they use the
[Kubernetes resources](../using-snapshot/README.md) instead. Use it to validate a
setup, debug, or try a checkpoint and restore by hand.

This page covers what each command needs, what the pod manifest must contain,
and how to run `snapshotctl checkpoint` and `snapshotctl restore`.

## Requirements

### Checkpoint

- The snapshot Helm chart must already be installed in the target namespace
- A `snapshot-agent` DaemonSet must be running in that namespace
- The namespace must already have the snapshot volume mounted by the agent
- The snapshot operator (`PodSnapshotReconciler`) must be installed in the cluster

`snapshotctl checkpoint` creates a `PodSnapshot` CR, which the operator's
`PodSnapshotReconciler` resolves into a `PodSnapshotContent` work order. The
node agent then performs the CRIU dump. Both the operator and the agent are
required; neither can be skipped.

The caller must have the following RBAC permissions in the target namespace:

- `create`, `get`, `list`, `watch` on `podsnapshots` (nvidia.com)
- `get`, `list` on `pods`

### Restore

- The snapshot Helm chart must already be installed in the target namespace
- A `snapshot-agent` DaemonSet must be running in that namespace
- The namespace must already have the snapshot volume mounted by the agent

`snapshotctl restore` does not require the operator. The agent handles restore
directly from pod annotations.

## Manifest Requirements

`snapshotctl checkpoint --manifest ...` and `snapshotctl restore --manifest ...`
accept a Kubernetes `Pod` manifest, not a Deployment or Job manifest.

That pod manifest must:

- describe the worker pod you want to checkpoint or restore
- use the workload's regular image; the node agent supplies the restore tooling
- match the runtime-relevant worker settings you care about preserving

`snapshotctl restore` applies Snapshot's generic
[Restore Pod Contract](../reference/restore-pod-contract.md), including the
annotations, control volume, environment, startup gate, and seccomp profile. It
keeps the container commands: the node agent replaces each restore
destination's command with its restore proxy when the container is created.

In practice, start from the real worker pod spec you would normally run, then
keep only the pod-level fields needed to recreate that worker accurately.

## Checkpoint

`snapshotctl checkpoint` creates a `PodSnapshot` from a pod manifest and waits for
the agent to checkpoint it:

```bash
snapshotctl checkpoint \
  --manifest ./vllm-replica-pod.yaml \
  --snapshot vllm-qwen3-0-6b-snapshot \
  --container main \
  --namespace my-inference
```

The manifest must be a `Pod` (not a Deployment or Job) using a
[snapshot-ready workload](../using-snapshot/README.md). `snapshotctl` rejects any
other `kind`. The supported frameworks ship capture Deployments. Wrap the selected manifest's
`spec.template.spec` in a Pod manifest to use it here:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vllm-replica-pod
spec:
  # The capture Deployment's spec.template.spec, unchanged
  ...
```

Checkpoint targets are part of the `PodSnapshot` request, not pod annotations.
`snapshotctl checkpoint` requires `--container <name>` and records that one
container in `PodSnapshot.spec.source.podRef.containers`.

`snapshotctl checkpoint` leaves the `PodSnapshot` CR in place as the capture
record after the checkpoint completes. It is not deleted automatically. A
`--cleanup` flag to remove it after a successful capture is planned as future
work.

## Restore

`snapshotctl restore` creates a new pod from a manifest and restores it from a
named `PodSnapshot`:

```bash
snapshotctl restore \
  --manifest ./vllm-replica-pod.yaml \
  --snapshot vllm-qwen3-0-6b-snapshot \
  --namespace my-inference
```

The restore manifest must contain a container with the same name checkpointed by that
`PodSnapshot`. `snapshotctl` returns once the restore is submitted. Watch the
pod's `nvidia.com/Restored` status condition, readiness, and events for progress.

To clone the same checkpoint into multiple containers, add a flat
source-to-destination map:

```yaml
metadata:
  annotations:
    nvidia.com/restore-from: worker-snapshot
    nvidia.com/restore-container-map: main=engine-0,main=engine-1
```

Every mapping source must match the one container captured by the
`PodSnapshot`, and every destination must exist in the restore manifest.
`snapshotctl` validates the full map before creating the Pod. Without the map,
same-name single-container restore remains unchanged. Unrelated platform
annotations are preserved.

## Notes

- observe restore progress through the pod's `Restored` status condition,
  readiness, events, and agent logs
- `RestoreSucceeded` means every destination restored, `RestorePartiallySucceeded`
  means only some restored, and `RestoreFailed` means none restored
- partial and failed outcomes are terminal; retry them with a new restore Pod
- `snapshotctl` is useful for debugging and lower-level validation, but it does
  not replace the operator-managed checkpoint flow
