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

When PageBroker is enabled, `pageBroker.restoreMode` selects the restore source
for pods that do not set `nvidia.com/snapshot-pagebroker: "false"`:

- `direct` (default) prepares a PageBroker transaction and mounts the original
  checkpoint directory read-only for CRIU. It does not copy CPU images into staging.
- `staged` copies checkpoint files into PageBroker staging before CRIU runs.

### Shared GPU transfer lanes

Set `pageBroker.transferPooledLanes: N` (or the PageBroker daemon option
`--custom-storage-pooled-lanes N`) to enable a shared pool of
CustomStorage I/O workers. The default, `0`, keeps the per-device transfer rings.
A positive value must not exceed the total staging-slot count. The pinned-memory
budget remains `transferBufferCount * transferChunkBytes * visible GPU count`.
The pool divides those slots among its lanes, so increasing the lane count does
not increase staging memory. A ready rank can use idle capacity from other GPUs.
Lanes assign chunks from the oldest ready extent first. Once its chunks are
assigned, lanes can begin the next extent while earlier I/O and copies finish.
They do not reserve capacity for ranks that are not ready or wait for an entire
rank to finish before assigning the next rank's chunks. CUDA event waits block
the worker rather than spin while a copy is pending.

Pinned buffers, GPU contexts, streams, events, NIXL agents and I/O queues initialize before
PageBroker accepts requests. For direct restores, the pool also prepares the
checkpoint's GPU file handles asynchronously while CPU restore starts. Each lane
gets an independent open file description, avoiding shared NFS open-context
contention. The restore transaction owns the prepared handles until commit or
abort. The CUDA stage checks file identity and size before using them. Process
mappings and destination GPU addresses are prepared after CRIU restores the
processes.

Choose the lane count using repeated last-rank completion measurements at a fixed
staging budget. Available CPU, storage bandwidth and host NUMA placement all
matter. Extra lanes can add contention or hit the container's CPU quota without
improving completion time.

For the tested eight-B200 GLM-5.3-NVFP4 checkpoint on VAST NFS, use
`transferPooledLanes: 16` with `transferBufferCount: 32`,
`transferChunkBytes: 134217728` and a 32-CPU PageBroker limit. This gives 256
credits and 32 GiB of staging across eight GPUs. At fixed storage and read-ahead
settings, four restores per arm reduced median agent time from 28.535 to 25.418
seconds and CUDA time from 20.422 to 17.477 seconds. Treat this as a qualified
starting point for that workload, not a universal optimum. If storage can feed
data faster than one GPU can copy it, concentrating credits may instead limit
throughput. Unequal extents and concurrent restores also need separate validation.

### Diagnose slow GPU restores

The agent's `Restore timing summary` separates CPU restore (`criu_restore`), GPU
restore (`cuda_restore`), and staging. Measure application readiness and a fresh
inference request separately. Framework compilation and autotuning after restore
will show up in application readiness, not in the completed CUDA restore phase.

For a large CustomStorage artifact, compare its GPU payload size with the sustained
read bandwidth available to the restoring node. For example, 520 GB takes at least
21 seconds to read at 25 GB/s, before CPU restore and any GPU setup that does not
overlap the transfer. Increasing pinned-memory resources alone cannot remove a
storage bandwidth limit. Tune `pageBroker.transferBufferCount` and
`pageBroker.transferChunkBytes` together, and account for their product on every
visible GPU.

When comparing runs, keep the model revision, workload image, GPU count, checkpoint
preparation, and storage mount options fixed. Check the mount options actually in
use on the destination node. A different NFS client may not support the same
connection and multipath options. Preserve compatible model, compiler, and
autotuning caches when a new capture is necessary.

PageBroker emits NVTX ranges for CUDA setup, payload transfer, storage waits, and
completion. An NVTX-only trace can distinguish slow native CUDA preparation from
slow storage. CUDA API tracing can interfere with native checkpoint operations,
so do not use it for the performance control. Confirm results with repeated
unprofiled restores and fresh inference.

When testing a build, set `image.agent.tag` for both the agent and PageBroker, and
verify the running containers' image digests. A separate `image.pageBroker.tag` is
rejected because the two images share an internal protocol and ship as a pair.
