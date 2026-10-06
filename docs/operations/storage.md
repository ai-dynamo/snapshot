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

### PageBroker restore source

For pods annotated with `nvidia.com/snapshot-pagebroker: "true"`,
`pageBroker.restoreMode` selects the restore source when PageBroker is enabled:

- `staged` (default) copies checkpoint files into PageBroker staging before CRIU runs.
- `direct` prepares a PageBroker transaction and mounts the original checkpoint
  directory read-only for CRIU. It does not copy CPU images into staging.

Both modes attempt to unmount the restore source before Commit or Abort.
Mount cleanup errors are reported through the existing restore cleanup path.
Commit, Abort, and expiry of a direct restore do not delete the checkpoint.
An open source directory is not a lock against external deletion or changes.
Keep checkpoint contents available for the duration of restore.

Source selection is separate from CRIU's image I/O mode. Restore uses
`imageIoMode` from the saved checkpoint manifest. `direct` (also the default
when empty) requests `O_DIRECT`. An explicit `writeback` setting is preserved.
The checkpoint filesystem and CRIU must support the requested I/O mode.
`pageBroker.restoreMode: direct` does not change how checkpoints are captured.
