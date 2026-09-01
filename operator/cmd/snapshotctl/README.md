# `snapshotctl`

`snapshotctl` is a lower-level snapshot utility for developers and operators.
It is not the primary user workflow. The normal user-facing path is:

```text
PodSnapshot CR -> operator -> snapshot-agent
```

Use `snapshotctl` when you want to exercise checkpoint or restore behavior
directly from a worker pod manifest.

## Requirements

### checkpoint

- the snapshot Helm chart must already be installed in the target namespace
- a `snapshot-agent` DaemonSet must be running in that namespace
- the namespace must already have the checkpoint PVC mounted by the agent
- the snapshot operator (`PodSnapshotReconciler`) must be installed in the cluster

`snapshotctl checkpoint` creates a `PodSnapshot` CR, which the operator's
`PodSnapshotReconciler` resolves into a `PodSnapshotContent` work order. The
node agent then performs the CRIU capture. Both the operator and the agent are
required; neither can be skipped.

The caller must have the following RBAC permissions in the target namespace:

- `create`, `get`, `list`, `watch` on `podsnapshots` (nvidia.com)
- `get`, `list` on `pods`

### restore

- the snapshot Helm chart must already be installed in the target namespace
- a `snapshot-agent` DaemonSet must be running in that namespace
- the namespace must already have the checkpoint PVC mounted by the agent

`snapshotctl restore` does not require the operator. The agent handles restore
directly from pod annotations.

## PodSnapshot lifecycle

`snapshotctl checkpoint` leaves the `PodSnapshot` CR in place as the capture
record after the checkpoint completes. It is not deleted automatically. A
`--cleanup` flag to remove it after a successful capture is planned as future
work.

## Manifest requirements

`snapshotctl checkpoint --manifest ...` and `snapshotctl restore --manifest ...`
accept a Kubernetes `Pod` manifest, not a Deployment or Job manifest.

That pod manifest must:

- describe the worker pod you want to checkpoint or restore
- use the placeholder image for checkpoint-aware flows
- match the runtime-relevant worker settings you care about preserving
- provide an inert, long-running entrypoint for each restore destination

`snapshotctl restore` applies Snapshot's generic
[restore Pod contract](../../../docs/restore-pod-contract.md), including the
annotations, control volume, environment, startup gate, and seccomp profile. It
does not replace container commands or inject a workload-specific standby
environment variable. A platform-specific entrypoint convention remains the
platform's responsibility.

In practice, start from the real worker pod spec you would normally run, then
keep only the pod-level fields needed to recreate that worker accurately.

## Target containers

Checkpoint targets are part of the `PodSnapshot` request, not pod annotations.
`snapshotctl checkpoint` requires `--container <name>` and records that one
container in `PodSnapshot.spec.source.podRef.containers`.

Restore uses the single container captured by that `PodSnapshot`. By default,
the restore manifest must contain a container with the same name. To clone the
same checkpoint into multiple containers, add a flat source-to-destination map:

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

## Commands

Checkpoint from a manifest:

```bash
snapshotctl checkpoint \
  --manifest ./worker-pod.yaml \
  --snapshot worker-snapshot \
  --container main \
  --namespace ${NAMESPACE}
```

Restore by creating a new pod from a manifest:

```bash
snapshotctl restore \
  --manifest ./worker-pod.yaml \
  --namespace ${NAMESPACE} \
  --snapshot worker-snapshot
```

## Notes

- `restore --manifest` creates a new restore target pod from the manifest you provide
- `restore` returns after the restore request is submitted; it does not wait for completion
- observe restore progress through the pod's `Restored` status condition,
  readiness, events, and agent logs
- `RestoreSucceeded` means every destination restored, `RestorePartiallySucceeded`
  means only some restored, and `RestoreFailed` means none restored
- partial and failed outcomes are terminal; retry them with a new restore Pod
- `snapshotctl` is useful for debugging and lower-level validation, but it does
  not replace the operator-managed checkpoint flow
