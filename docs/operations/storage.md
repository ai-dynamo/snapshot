# Storage

Snapshot keeps every checkpoint in a single shared volume that all node agents
mount. Each agent's PageBroker sidecar publishes checkpoint artifacts there and
reads them back for restore; workload pods never mount checkpoint storage.

The checkpoint store must prevent unauthorized modification of artifacts through
filesystem permissions or the storage server's access controls. PageBroker requests
owner-only permissions for new GPU files and directories, but does not reject
directory modes that allow group or other users to write. Network filesystems such
as SMB can report these modes from mount settings rather than per-directory permissions.

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

The claim is always in the Helm release namespace. The chart resolves these
values into `storage.yaml` in the existing configuration ConfigMap, mounted at
`/etc/snapshot/storage.yaml` in PageBroker and the operator manager. The file
carries one backend block and that populated block selects the backend; Helm's
`storage.type` only chooses which block to render. The agent
receives only its existing `config.yaml`; its configuration has no PVC identity
block. Provisioning settings (`create`, `size`, `storageClass`) do not identify
the artifact store.

The PVC store ID hashes the backend, claim namespace/name and normalized path
inside the claim. The current installation uses the entire claim, so that path
is `/`, independent of the container-local `/checkpoints` mount. Neither a
caller-supplied store ID nor an additional PVC namespace setting is required.
`storage.pvc.basePath` retains its existing mount-path meaning.

The operator validates this resolved identity at startup and uses it to check
artifact cleanup against each content's recorded store. New captures use the
legacy filesystem flow by default. Artifact addressing requires a compatible
PageBroker image and the separate activation steps below. S3 remains unsupported.

### Enable artifact addressing

The operator Deployment and agent DaemonSet roll independently. Keep new content
unbound while upgrading agents: an older agent cannot record a bound publication
descriptor. Use two Helm upgrades when enabling this contract on an existing
installation:

1. Select matching agent/operator/PageBroker images that implement the artifact
   contract, including the PageBroker PVC backend. Set
   `pageBroker.artifactAddressing=true` and keep
   `operator.bindNewContents=false`. This configures compatible brokers while
   allowing legacy captures throughout the rollout. Wait for the agent DaemonSet
   rollout to complete on every node before continuing.
2. Set `operator.bindNewContents=true`, keeping
   `pageBroker.artifactAddressing=true`. This changes only the operator's capture
   activation; it does not roll the agents again. New contents now record the
   configured store and require publication descriptors.

Both settings default to `false`, so the chart adds no artifact-backend arguments
until explicitly enabled. Agent and PageBroker images must still match. A fresh installation with compatible images may enable
both settings together, provided no captures are submitted until all agents are
ready. Existing unbound contents remain readable through the legacy path.

Once bound content exists, keep a compatible broker with artifact addressing
enabled for its reads and restores. Turning off `operator.bindNewContents` stops
new bound captures while preserving support for existing ones; it does not
convert their storage format or permit downgrading the agents to a legacy build.

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
container wrote them.

PageBroker runs as privileged root so its GPU engine can operate on workload
CUDA processes and device mappings, and it can write artifacts on the shared PVC.

PageBroker adds one memory-backed volume per agent pod, shared by the agent and
the sidecar:

- **Checkpoint.** By default, CRIU dumps the image into a staging directory on
  that volume. When the dump succeeds, PageBroker publishes it to the PVC: it
  renames any previous artifact for the same checkpoint aside, then renames
  the new one into place. The destination path is briefly absent between those
  two renames. A failed dump is aborted and never touches the PVC. GPU
  CustomStorage capture instead writes directly to a temporary directory
  beside the destination on the PVC; a failed capture removes that temporary
  directory without promoting it, so it still never publishes a completed
  checkpoint, but it does touch the PVC along the way.
- **Restore.** By default (`pageBroker.restoreMode: direct`), CRIU restores
  directly from the artifact already on the PVC; nothing is copied into
  staging. Setting `pageBroker.restoreMode: staged` has PageBroker copy the
  artifact into staging first, and CRIU restores from memory instead.

With `pageBroker.artifactAddressing=true`, the sidecar receives the resolved
store configuration (`--storage-config /etc/snapshot/storage.yaml`) and derives
the store identity from it at startup. Checkpoints bound to that store are addressed by
identity rather than by path: the agent names the store, the content UID and
the container, and PageBroker publishes the image at
`artifacts/<contentUID>/containers/<name>/`, the same layout as before, with a
`publication.json` next to `manifest.yaml` recording the store ID, the
artifact handle and the deterministic commit ID. Commit returns that handle
as the publication descriptor; restores and metadata reads name the
descriptor, and PageBroker refuses a descriptor from another store
(`STORE_MISMATCH`), a publication that is missing (`ARTIFACT_NOT_FOUND`) or
whose evidence does not match (`ARTIFACT_CORRUPT`). A transaction that
outlives PageBroker's fixed 2h5m lifetime fails Commit with
`TRANSACTION_EXPIRED`. PageBroker begins expiry cleanup, but active GPU work
or a filesystem cleanup error can defer removal until a later sweep. Legacy
path-addressed requests are unchanged, and content without a store binding
keeps using them.

Because the staging volume is RAM, size the agent pod for it:

| Value                                | Purpose                                                                                        | Default      |
|---------------------------------------|--------------------------------------------------------------------------------------------------|--------------|
| `pageBroker.staging.sizeLimit`       | Cap on the shared staging volume, used for checkpoint and staged restore                        | `64Gi`       |
| `daemonset.resources.limits.memory`  | Bounds the largest checkpoint image, since CRIU writes staging from the agent container         | `64Gi`       |
| `pageBroker.resources.limits.memory` | Bounds staged-restore prefetch, since PageBroker fills staging only in staged restore mode       | `256Gi`      |
| `pageBroker.resources.requests`      | GPU transfer rings and process memory; added to the agent's requests when scheduling            | 8 CPU / 32Gi |

Pages written to the staging volume are charged to the container that wrote
them (the agent during checkpoint, PageBroker during staged restore), not to
the volume. By default `sizeLimit` and `daemonset.resources.limits.memory` are
both `64Gi`, leaving no headroom for the agent's own process and CRIU memory
while staging a checkpoint near that size. Leave that headroom — either lower
`sizeLimit` or raise the agent memory limit — when sizing for your workloads.
The staging-space check (`INSUFFICIENT_STORAGE`) only rejects a transfer that
would exceed `sizeLimit`; it does not guarantee protection from an OOM kill,
and it has no way to know the size of a checkpoint before CRIU writes it.
Leaving `sizeLimit` missing, null, or empty falls back to the chart's `64Gi`
default, not the node's allocatable memory.

To checkpoint workloads whose CPU memory exceeds the defaults, raise the
staging cap and the agent memory limit together, leaving headroom above the
cap for agent process memory — for example, a 200Gi staging budget with 24Gi
of headroom:

```bash
helm upgrade snapshot ... \
  --set daemonset.resources.limits.memory=224Gi \
  --set pageBroker.staging.sizeLimit=200Gi
```

Leave `pageBroker.resources.limits.memory` at its chart default (`256Gi`)
here: PageBroker needs headroom above the staging cap too, on top of the
default GPU transfer buffers (4 GiB per visible GPU, `32Gi` on an eight-GPU
node by default). The chart default already covers both; raise it further
only if your node has more GPUs or a larger `pageBroker.transferBufferCount`.

Each agent pod requests 10 CPU and 33Gi of memory in total by default, including
the GPU engine transfer rings. Nodes must have that allocatable capacity.
Limits are not checked at scheduling time.

When upgrading from a chart that shipped without PageBroker, do not use
`helm upgrade --reuse-values`: it keeps the previous chart's defaults, including
the old 4Gi agent memory limit. Use `--reset-then-reuse-values` instead, which
resets to the new chart's defaults but still carries forward any value you
explicitly overrode before — including an old memory limit or staging size, if
you had set one. Review `daemonset.resources.limits.memory`,
`pageBroker.resources.limits.memory`, and `pageBroker.staging.sizeLimit`
together after upgrading, or pass all three explicitly.

## Retention

Chart-created PVCs are retained when the Helm release is removed, so checkpoints
survive an uninstall.

## Other backends

`storage.type` currently supports `pvc`. Object-storage backends (`s3`, `oci`) are
reserved in the chart for future use and are not supported today.

### PageBroker restore source

`pageBroker.restoreMode` selects the restore source for every restore:

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
