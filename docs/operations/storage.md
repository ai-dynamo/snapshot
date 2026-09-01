# Storage

Snapshot keeps every checkpoint in a single shared volume that all node agents
mount. Each agent reads and writes checkpoint artifacts there; workload pods never
mount checkpoint storage.

## The checkpoint volume

Today the checkpoint store is a Kubernetes PersistentVolumeClaim (PVC). Because
agents on multiple GPU nodes mount it concurrently, it must support
`ReadWriteMany` (RWX). The chart provisions one PVC per cluster by default and
mounts it at `/checkpoints` in every agent.

## Configuration

The chart's `storage.pvc` values control the PVC:

| Value | Purpose | Default |
|-------|---------|---------|
| `storage.pvc.create` | Create the PVC (set `false` to use an existing one) | `true` |
| `storage.pvc.name` | Shared RWX PVC mounted by every agent | `snapshot-pvc` |
| `storage.pvc.size` | Requested size | `1Ti` |
| `storage.pvc.storageClass` | Storage class (empty = cluster default) | `""` |
| `storage.pvc.basePath` | Mount path inside the agent | `/checkpoints` |

If the cluster has no default storage class that can provision RWX, set one:

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --namespace snapshot --create-namespace \
  --set storage.pvc.storageClass=<rwx-storage-class>
```

### Use an existing PVC

Point the chart at an existing RWX claim instead of creating one:

```bash
helm install snapshot ... \
  --set storage.pvc.create=false \
  --set storage.pvc.name=<existing-rwx-pvc>
```

The named claim must support `ReadWriteMany`. Access modes are immutable, so a
`ReadWriteOnce` claim cannot be converted in place — create a new RWX claim and, if
the existing checkpoints are needed, copy them over once.

## Retention

Chart-created PVCs are retained when the Helm release is removed, so checkpoints
survive an uninstall.

## Other backends

`storage.type` currently supports `pvc`. Object-storage backends (`s3`, `oci`) are
reserved in the chart for future use and are not supported today.
