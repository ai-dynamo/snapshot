# Storage

Snapshot keeps every checkpoint in a single shared volume that all node agents
mount. Each agent's PageBroker sidecar publishes checkpoint artifacts there and
reads them back for restore; workload pods never mount checkpoint storage.

## The checkpoint volume

Today the checkpoint store is a Kubernetes PersistentVolumeClaim (PVC). Because
agents on multiple GPU nodes mount it concurrently, it must support
`ReadWriteMany` (RWX). The chart provisions one PVC per cluster by default and
mounts it at `/checkpoints` in every agent.

## Configuration

The chart's `storage.pvc` values control the PVC:

| Value                      | Purpose                                             | Default        |
|----------------------------|-----------------------------------------------------|----------------|
| `storage.pvc.create`       | Create the PVC (set `false` to use an existing one) | `true`         |
| `storage.pvc.name`         | Shared RWX PVC mounted by every agent               | `snapshot-pvc` |
| `storage.pvc.size`         | Requested size                                      | `1Ti`          |
| `storage.pvc.storageClass` | Storage class (empty = cluster default)             | `""`           |
| `storage.pvc.basePath`     | Mount path inside the agent                         | `/checkpoints` |

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

## PageBroker staging

Every agent pod also runs a PageBroker sidecar, from its own image at the
agent's tag, which owns the movement of checkpoint data between the PVC and the
node. Both containers mount the same PVC
at `/checkpoints`, so artifacts land in the same place regardless of which
container wrote them. The PVC layout is unchanged.

PageBroker adds one memory-backed volume per agent pod, shared by the agent and
the sidecar:

- **Checkpoint.** CRIU dumps the image into a staging directory on that volume.
  When the dump succeeds, PageBroker publishes it to the PVC and replaces any
  previous artifact for the same checkpoint atomically. A failed dump is aborted
  and never touches the PVC.
- **Restore.** PageBroker copies the artifact from the PVC into staging first.
  CRIU then restores from memory instead of reading the PVC directly.

Because the staging volume is RAM, size the agent pod for it:

| Value                                | Purpose                                                                                 | Default     |
|--------------------------------------|-----------------------------------------------------------------------------------------|-------------|
| `pageBroker.staging.sizeLimit`       | Cap on the shared staging volume. PageBroker refuses a restore that does not fit        | `64Gi`      |
| `daemonset.resources.limits.memory`  | Bounds the largest checkpoint image, since CRIU writes staging from the agent container | `64Gi`      |
| `pageBroker.resources.limits.memory` | Bounds restore prefetch, since PageBroker fills staging on restore                      | `256Gi`     |
| `pageBroker.resources.requests`      | Idle footprint of the daemon; added to the agent's requests when scheduling             | 1 CPU / 2Gi |

Pages written to the staging volume are charged to the container that wrote
them, not to the volume. Keep `sizeLimit` at or below the smaller of the two
memory limits so that an oversized transfer fails PageBroker's capacity check
with `INSUFFICIENT_STORAGE` instead of OOM-killing the writer. Leaving
`sizeLimit` empty sizes the tmpfs from node allocatable memory, which is larger
than either limit and defeats that check.

To checkpoint workloads whose CPU memory exceeds the defaults, raise all three
together, for example for images up to 200Gi:

```bash
helm upgrade snapshot ... \
  --set daemonset.resources.limits.memory=200Gi \
  --set pageBroker.resources.limits.memory=200Gi \
  --set pageBroker.staging.sizeLimit=200Gi
```

Each agent pod requests 3 CPU and 3Gi of memory in total by default, so it
schedules on any GPU node the agent alone would have fit on. Limits are not
checked at scheduling time.

When upgrading from a chart that shipped without PageBroker, do not use
`helm upgrade --reuse-values`: it keeps the previous chart's defaults, including
the old 4Gi agent memory limit. Use `--reset-then-reuse-values` or pass your
overrides explicitly.

## Retention

Chart-created PVCs are retained when the Helm release is removed, so checkpoints
survive an uninstall.

## Other backends

`storage.type` currently supports `pvc`. Object-storage backends (`s3`, `oci`) are
reserved in the chart for future use and are not supported today.
