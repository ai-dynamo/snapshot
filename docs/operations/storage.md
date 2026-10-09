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
| `storage.pvc.nfsReadAheadKiB` | Minimum NFS read-ahead in KiB (`0` leaves it unchanged) | `0` |

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

### Buffered CRIU reads on NFS

Set `storage.pvc.nfsReadAheadKiB: 4096` to ensure at least 4 MiB of Linux
read-ahead for the checkpoint mount. A privileged init container resolves the
NFS backing device from `/checkpoints` and raises its `read_ahead_kb` before the
agent and PageBroker start. It leaves larger settings and non-NFS mounts unchanged.
An inaccessible NFS setting fails initialization so the requested tuning is not
silently lost. The default, `0`, does not create this init container.

This helps sequential buffered image reads. It does not change direct I/O for
CPU page images or PageBroker GPU transfers. In the tested eight-GPU GLM
checkpoint, 16 ghost images contain 547.4 MB of saved NCCL `/dev/shm/nccl-*`
shared-memory file contents. Ghost files were unlinked but still referenced by
the processes, so CRIU must recreate their contents. Another 2,239 images total
only 7.1 MB and describe threads, memory mappings, file descriptors, sockets and
other process state. These are separate from the 73.3 GB of direct CPU-page data.

CRIU copies ghost files serially with blocking `sendfile` calls. A small client
read-ahead window limits the NFS requests feeding that copy, even when the
storage system has ample aggregate bandwidth. An isolated copy of the same
16 cold ghost images issued 4,224 READ RPCs at 128 KiB read-ahead, versus 576 at
4 MiB. Mean request payload grew from 126.6 to 928 KiB, and copy time fell from
3.0–5.5 seconds to 0.44–0.51 seconds. Larger read-ahead does not remove the
separate cost of opening and reading thousands of small images.

In full restores, 4 MiB reduced the CRIU stage from 9.5–10.4 seconds to 4.6–4.8
seconds compared with 128 KiB. Both cases started with buffered image contents
absent from the restoring node's page cache. Metadata and storage-server caches
were not cold-controlled. Pre-reading these images would warm the client cache
but move I/O outside the restore timer. See [the experiment](https://github.com/ai-dynamo/snapshot/issues/521)
for controls and limits. Measure repeated end-to-end restores on the intended
filesystem before adopting this setting.

Read-ahead belongs to the Linux backing device, so other mounts sharing the same
NFS filesystem on that node also see the change. This includes other releases
that use that device. Disabling the chart option does not restore the old value.
An administrator must reset it or remount the filesystem to return to its default.
The init container uses `daemonset.initContainer` image and resource settings and
works independently of PageBroker and seccomp profile deployment.

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
