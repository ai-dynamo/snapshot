<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-237: S3-compatible object storage for checkpoints

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Overview and flows](#overview-and-flows)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
    - [Kubernetes API and operator](#kubernetes-api-and-operator)
    - [Maintenance](#maintenance)
    - [Snapshot agent](#snapshot-agent)
    - [PageBroker: internal API](#pagebroker-internal-api)
  - [Security](#security)
  - [Configuration](#configuration)
  - [Storage and lifecycle](#storage-and-lifecycle)
    - [Publication and artifact format](#publication-and-artifact-format)
  - [Performance and Scalability](#performance-and-scalability)
  - [Monitoring](#monitoring)
  - [Dependencies](#dependencies)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Allow Snapshot to save checkpoint artifacts to S3-compatible object storage and
restore workloads from them.
PVC remains the default. Stage 1 uses one store per installation, selected at
Helm install/upgrade, and deletes artifacts immediately on both backends. Stage 2
adds explicit coordination between active storage operations and maintenance.

## Motivation

S3 provides checkpoint storage without a shared checkpoint PVC. It is the first
additional backend; stable artifact references and separate backend adapters let
future stores be added with minimal changes to workload APIs.

### Goals

- Add S3 checkpoint/restore through PageBroker and cleanup through maintenance.
- Use the same in-process maintenance workqueue model for PVC, S3 and future backends.
- Preserve artifact identity and verify publication, restore and deletion across
  retries, restarts and storage configuration changes.
- Reject new operations after deletion begins. Stage 1 deletes immediately and
  lets the sweep remove anything that lands late; Stage 2 adds active-operation
  fencing.
- Keep storage credentials outside workload APIs and the agent process. The
  operator manager is the one exception: since maintenance runs in-process, it
  carries storage credentials, scoped to maintenance operations only.

### Non-Goals

- Storage classes, simultaneous stores, automatic fallback or artifact migration.
- Direct (unstaged) restore of bound content on any backend, new feature-discovery APIs or
  workload-identity implementation.
- Moving CRIU/process orchestration into PageBroker or redesigning CUDA execution.
- Cross-component operation leases and generation fencing in Stage 1; those are
  deferred to Stage 2 after the conservative deletion contract is qualified.

## Proposal

PageBroker already handles checkpoint/restore through local staging; this design
adds its S3 backend. Content is bound to its store before capture and becomes
Ready only after confirmed publication. Restore validates compatibility and stages
the complete bundle before CRIU/CUDA execution.

The operator runs maintenance in-process: a bounded worker pool handles content
deletion and periodic sweeps through per-backend adapters.
Workers access PVC or S3 directly and never through PageBroker.

### Overview and flows

```mermaid
flowchart TB
    API["Kubernetes API<br/>Content / status"]
    Secret["Credential Secret<br/>Projected by kubelet"]
    S3["S3-compatible storage"]

    subgraph AgentPod["Agent Pod"]
        Agent["Snapshot agent"]
        NodePB["Node PageBroker"]
        Files["Shared local staging<br/>Manifest and checkpoint files"]
        Agent <-->|"Prepare / GetArtifactMetadata / Stage<br/>Commit / Abort · Unix socket RPC"| NodePB
        Agent <-->|"Read/write local files"| Files
        NodePB <-->|"Stage/verify files"| Files
    end

    subgraph OperatorPod["Operator Pod"]
        Operator["Snapshot operator<br/>reconcilers"]
        Queue["Rate-limiting workqueue"]
        Workers["Maintenance workers<br/>operator/internal/maintenance"]
        Operator -->|"Enqueue delete-content / sweep"| Queue
        Queue -->|"Dequeue key"| Workers
        Workers -.->|"Requeue with backoff on error"| Queue
    end

    Agent <-->|"Watch / update status"| API
    Operator <-->|"Watch content / finalize"| API
    Workers -->|"Read ownership / reconcile metadata"| API
    NodePB <-->|"Authenticated upload/download"| S3
    Workers <-->|"Inspect / list / delete objects"| S3
    Secret -.->|"Mounted credential file"| NodePB
    Secret -.->|"Mounted credential file"| OperatorPod
```

The sequence diagrams below show each flow's happy path and its main failure
branch. Details follow in [Design Details](#design-details).

**Checkpoint flow**

```mermaid
sequenceDiagram
    autonumber
    participant Agent as Snapshot agent
    participant PB as Node PageBroker
    participant S3 as S3-compatible store
    participant API as Kubernetes API
    Agent->>Agent: Read content from informer cache (store binding, not deleting)
    Agent->>PB: Prepare(storeID, artifactUID, container)
    PB->>S3: Preflight destination access
    PB-->>Agent: Transaction opened
    Agent->>Agent: CUDA then CRIU capture into shared staging
    Agent->>PB: Commit
    PB->>S3: Upload payload objects
    PB->>S3: PUT index.json last under containers/name/
    PB-->>Agent: PublishedArtifact (store, handle)
    Agent->>API: Add descriptor to status.storage.artifacts, Ready when all containers confirmed
    alt Commit reply lost, agent still running
        Agent->>PB: Commit (same transaction)
        PB-->>Agent: Same PublishedArtifact, publication path is idempotent
    end
    alt Agent or PageBroker restarted before status was written
        Agent->>PB: GetArtifactMetadata(storeID, artifactUID, container)
        PB-->>Agent: Resolved PublishedArtifact and manifest, or ARTIFACT_NOT_FOUND
    end
    alt Transaction expired before index PUT
        PB-->>Agent: TRANSACTION_EXPIRED, unconfirmed objects left for sweep
    end
```

**Restore flow**

```mermaid
sequenceDiagram
    autonumber
    participant Agent as Snapshot agent
    participant PB as Node PageBroker
    participant S3 as S3-compatible store
    Agent->>PB: GetArtifactMetadata(handle, storeID)
    PB->>S3: Fetch and verify index, download manifest
    PB-->>Agent: manifest_directory (shared volume)
    Agent->>Agent: Snapshot compatibility checks
    Agent->>PB: Abort (release metadata directory)
    Agent->>PB: StagedRestore(handle)
    PB->>S3: Download all objects, verify digests
    PB-->>Agent: Staged bundle path
    Agent->>Agent: Mount staging, CRIU/CUDA restore, unmount
    Agent->>PB: Commit (release staging)
    alt Store mismatch, missing or corrupt artifact
        PB-->>Agent: STORE_MISMATCH, ARTIFACT_NOT_FOUND or ARTIFACT_CORRUPT, restore fails before CRIU
    end
    alt Unmount fails
        Agent->>Agent: Report restore result unchanged, skip Commit
        PB->>PB: Transaction expiry reclaims staging
    end
```

**Cleanup flow**

```mermaid
sequenceDiagram
    autonumber
    participant Rec as Content reconciler
    participant W as Maintenance worker
    participant API as Kubernetes API
    participant S3 as S3-compatible store
    Rec->>API: Content has deletion timestamp
    Rec->>W: Enqueue delete-content(storeID, artifactUID)
    W->>API: Reread content, still deleting, same UID
    W->>S3: List and delete artifacts/artifactUID/ under the bound store
    W->>S3: Verify nothing remains
    W->>API: Remove finalizer
    alt Too early, partial, denied or unknown result
        W->>W: Requeue with backoff, finalizer stays
    end
    alt Publication landed after cleanup
        Rec->>W: Periodic sweep
        W->>S3: List artifacts, artifactUID has no live content
        W->>S3: Delete as orphan
    end
```

**Checkpoint:** agent → prepare/validate destination → CUDA then CRIU/filesystem
capture into staging → PageBroker uploads and publishes the index at a fixed path →
agent persists the descriptor and Ready. The path is derived from the content UID
and the container name, so a retried `Commit` writes to the same place and is a
no-op if the index is already there. The index records the store, content UID and
container it was published for, and every reader refuses it on a mismatch; see
[Publication and artifact format](#publication-and-artifact-format).

**Restore:** agent → GetArtifactMetadata → Snapshot compatibility gates → StagedRestore
downloads/verifies the full bundle → CRIU/CUDA restore → unmount → Commit cleanup.
Once fully staged, restore needs no further S3 reads. The later executor
compatibility gate still runs before process restore.

**Cleanup:** the existing maintenance flows, with one change each in what the
backend does. `delete-content` deletes the objects under `artifacts/<artifactUID>/`
in the bound store instead of removing a directory. `sweep` lists artifact UIDs
under the store prefix instead of directories. Deletion timestamp handling,
ownership recheck, finalizer and requeue are unchanged; see
[Maintenance](#maintenance).

**Agent loss after publication:** if the agent or PageBroker restarts after
PageBroker published but before status was written, the retained transaction is
gone, so the restarted agent cannot retry `Commit`. It calls `GetArtifactMetadata`
with the store ID, content UID and container name instead. PageBroker resolves the
single publication for that identity and returns its descriptor with the manifest;
the agent checks that the manifest names this store, content and container,
persists the descriptor and marks the content Ready. `ARTIFACT_NOT_FOUND` means the
capture failed and a new capture gets a new content. The agent never derives a
handle itself.

### Limitations, Risks, and Mitigations

| Risk | What goes wrong | How the design handles it |
| --- | --- | --- |
| Changed store | The installation is switched to another store, or back again. Content bound to the old store cannot be reached through the new one. If the old store is gone, its content can never be cleaned up. | The content keeps `storeID` and `artifactHandle`; the bucket layout is keyed by content UID. Switching S3 → PVC → the same S3 configuration hashes to the same `storeID`, so old checkpoints restore again with no migration. Every operation refuses a mismatched binding before any I/O. While the original store is inactive, deletion cannot be confirmed: the content gets a `StoreInactive` condition and an event and keeps its finalizer; the sweep retries. An administrator can set the annotation `snapshot.nvidia.com/release-artifacts=true` to drop the finalizer; the artifacts left behind are recorded in an event. |
| Credentials or S3 outage | The key is revoked or the store is unreachable during a checkpoint, a restore or a cleanup. | Check destination access before capture and source access before restore. Bound retries and timeouts. Preflight cannot guarantee later access, so every operation handles denial mid-way. Access denial never proves that an artifact is missing, so it never triggers deletion. |
| Uncertain publication | PageBroker uploaded the checkpoint but the `Commit` reply to the agent was lost, or one side restarted. Nobody knows whether the publication exists. | The publication has one fixed path per content and container. A retried `Commit` checks that path and is a no-op if a matching index is already there. Uploads resume only from complete staging; CUDA/CRIU are never replayed. If the agent itself is lost before status is written, it looks the publication up by identity through `GetArtifactMetadata` on restart and finishes or fails the capture from what it finds. |
| Cleanup races | On S3, deleting an artifact while a restore is still downloading it fails the restore, and an upload that lands after cleanup leaves an orphan. On PVC neither happens: an unlinked file stays readable to a process that has it open. | Deletion is immediate on both backends, as today. A restore interrupted by deletion fails and reports it; the content was deleted under it. A publication that lands after cleanup is an orphan the next sweep removes. Stage 2 adds explicit active-operation tracking so cleanup can wait for in-flight operations. |
| Metadata loss | The `PodSnapshotContent` objects are lost, for example a cluster rebuilt from scratch. The artifacts are still in the bucket but nothing owns them, and the next sweep would delete them as orphans. | Back up the content objects separately. While they are being restored from backup, pause sweeps and new operations until an administrator resumes. |
| Operator process risk | Maintenance already runs inside the operator process for PVC, so a panic or leak there affects reconciliation, and a leadership change drops in-flight work. S3 widens this: the process now holds store credentials and performs network transfers, not only local deletes. | Existing mitigations stay: panics isolated per task, bounded worker pool and transfer concurrency, pending work rebuilt from cluster state after any restart or leadership change. Credentials are scoped to maintenance operations only. |

## Design Details

### API

#### Kubernetes API and operator

Extend the existing `PodSnapshotContent`; proposed fields below are additions only:

```yaml
spec:
  storage:                               # Store binding fixed before capture
    storeID: store-v1-<digest>            # Prevent access through another configured store
status:
  storage:                               # Results confirmed by checkpoint Commit
    artifacts:                           # One descriptor per captured container
      - containerName: main              # Associate the publication with its container
        artifactHandle: <opaque-handle>   # Locate the publication inside its bound store
```

```go
// CheckpointStorageBinding is spec.storage; immutable with the rest of spec.
type CheckpointStorageBinding struct {
    // +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
    StoreID string `json:"storeID"`
}

// CheckpointStorageStatus is status.storage; one descriptor per confirmed container.
type CheckpointStorageStatus struct {
    // +listType=map
    // +listMapKey=containerName
    Artifacts []PublishedContainerArtifact `json:"artifacts"`
}

type PublishedContainerArtifact struct {
    ContainerName  string `json:"containerName"`
    ArtifactHandle string `json:"artifactHandle"`
}
```

Both `spec.storage` and `status.storage` are optional pointers so legacy content
without a binding stays valid. `artifactHandle` is where PageBroker put the
publication inside the store: a directory on PVC, the index object key on S3. The
agent stores it and hands it back for restore and metadata reads, so restore never
reconstructs the path from naming conventions and the layout can change without an
API change. The layout version lives in the index itself, not in the API: for S3,
PageBroker reads the version from the index before touching any data. PVC has no
index: the backend alone selects the existing directory reader, so artifacts
captured before this proposal restore unchanged. CRIU and CUDA formats are unchanged
and are validated by the existing compatibility checks.

The operator sets `spec.storage.storeID` when it creates the `PodSnapshotContent`.
The whole spec is immutable, so the binding can never be added or changed
afterwards. A `PodSnapshotContent` represents one capture attempt, so its immutable
UID already distinguishes a new attempt from a retry of the same attempt. Each
container's S3 index records `storeID`, `artifactUID` and `containerName`; no
further Kubernetes API field is needed. A worked example is under
[Publication and artifact format](#publication-and-artifact-format).

Two CRD validation rules tie the status block to the binding: `status.storage`
requires `spec.storage`, and every `status.storage.artifacts[].containerName` must
be one of `spec.source.podRef.containers`. The list is a map keyed by
`containerName`, so a container has at most one descriptor.

A store is the single storage configuration of the installation, defined in Helm
and rendered into `storage.yaml` in the configuration ConfigMap described under
[Configuration](#configuration). It is not a Kubernetes resource in Stage 1.

Compute `storeID` as `store-v1-` + SHA-256 of a versioned, fixed-field canonical
storage identity. For S3, encode backend, resolved endpoint, region, bucket and
normalized prefix. Normalize the endpoint to lowercase scheme
and DNS host, omit the scheme's default port, retain a non-default port and remove
only a root trailing slash. Reject user information, query strings, fragments and
non-root endpoint paths. Resolve an empty/default endpoint to an explicit provider
endpoint before hashing. Normalize the prefix by removing leading/trailing slashes.
For PVC, encode backend, namespace, claim and normalized base path. Access settings
are excluded: addressing mode, TLS CA, checksum mode and credentials describe how a
client talks to the bucket, not where the bucket is. Changing them never changes
the binding or the artifact location, so existing checkpoints stay reachable.
Canonical encoding, shared by Go and C++: write each field as a 4-byte big-endian
length followed by its UTF-8 bytes, version tag first; `storeID` is `store-v1-`
followed by the lowercase hex SHA-256 of those bytes. The shared Go/C++ vectors live
in `api/storage/testdata/identity.json`.

Worked S3 example. For the configuration shown under
[Configuration](#configuration), the empty endpoint resolves to
`https://s3.us-east-1.amazonaws.com` and the preimage is, in order:

```text
"snapshot.store/v1", "s3", "https://s3.us-east-1.amazonaws.com", "us-east-1", "checkpoint-bucket", "snapshots"
storeID = store-v1-fa5448e4870e1058af946de52383baa7dacfd92dfe74f70c821d87a734b349d9
```

`storeID` is the complete identity of the physical location, not only a guard
against configuration changes. The store prefix in the bucket is exactly the
configured `prefix`; nothing outside the hashed fields contributes to an object key,
so two configurations with the same `storeID` address the same objects. It is
recorded in every S3 index, and it scopes maintenance so a worker can never delete
under a different store. A reference to a configuration object cannot replace it:
the object can be edited to point elsewhere, and the digest is what detects that.
When a storage CRD introduces multiple stores, content gains a `storeRef` beside
`storeID`; the reference says where to resolve, the digest says what was resolved.
An installation that needs its own namespace in a shared bucket uses its own
`prefix`.

Operator, PageBroker and maintenance share one Go/C++ fixture set covering default
and non-default ports, rejected paths, regions, addressing modes and prefixes, and
must produce identical digests. Only PVC vectors exist until the S3 backend lands;
the example above is the first S3 vector.

`PodSnapshot` and `SnapshotJob` retain their current workload request APIs. Later
storage-class selection can bind the same content fields without changing restore
references. Legacy content without a binding is accepted only in PVC mode.

#### Maintenance

The operator owns scheduling and retention. `operator/internal/maintenance/` is the
existing implementation of this design for PVC; S3 adds a backend and the changes
listed below.

**Roles.** The content reconciler decides *when*: a deletion timestamp enqueues
`delete-content` and a periodic tick enqueues `sweep`. A bounded pool of workers
decides *whether*: it rereads ownership, takes the artifact lock and then acts. Backends know *how*: they list, inspect and delete
objects under the store prefix for PVC or S3. Maintenance never calls PageBroker.

**Eligibility.** Per-content deletion is scoped by the content's bound `storeID` and
its artifact UID, which together name `artifacts/<artifactUID>/` in exactly one
store; every container and unfinished attempt under it is covered. A sweep lists
artifact UIDs under the store prefix and keeps only those with no live content.

**Immediate deletion.** Deletion is immediate on both backends, as today. On PVC an
unlinked file stays readable to a process that already has it open. On S3 a deleted
object disappears under a running download, so a restore that is still downloading
fails and reports the failure; the content was deleted under it. An upload accepted
before transaction expiry can land after cleanup; it is an orphan the next sweep
removes. The agent checks the deletion timestamp once, as today; nothing is reread.
Stage 2 adds active-operation tracking so cleanup can wait for in-flight operations.

**Safety.** Any result that is too early, partial, denied or unknown keeps the finalizer and
requeues with backoff. Nothing is ever selected by object timestamp; a conflicting
or duplicate publication fails closed and needs explicit repair. A publication that
lands after cleanup is an orphan the next sweep removes. On operator start or new
leadership, pending work is rebuilt from cluster state rather than trusted to have
survived in memory.

**Changes to the current implementation.** The store ID on the work-item key; a shared lock on `(storeID, artifactUID)` held across
ownership recheck, storage operation and status or finalizer update; no drain on
leadership loss, where today `Queue.Start` drains on every shutdown; and failed
sweeps requeue with backoff, where today they wait for the next scan interval.

**Implementation notes.** Queue, dedup, retry and worker mechanics are those of
the existing `operator/internal/maintenance/` package and are not restated here.
What S3 adds to the operator process is store credentials and an S3 SDK client;
backend calls stay idempotent because a request issued before cancellation may
still finish.

Maintenance and PageBroker share the store-ID, artifact format and Stage 1
transaction contracts, with Go/C++ compatibility fixtures. Stage 2 adds
the cross-component active-operation and generation-fencing contract.

#### Snapshot agent

What changes for S3, and why:

- **Deletion check from the informer cache.** The agent reads the content once from
  its PodSnapshotContent informer, checks the store binding and the deletion
  timestamp, and then starts its PageBroker transaction; there is no reread and no
  direct API read. A deletion that the cache has not yet seen is caught after capture
  by the finalizer and the sweep, at the cost of one wasted capture.
- **Metadata through PageBroker.** The agent has no credentials and no bucket
  access, so it obtains the manifest through `GetArtifactMetadata` on the shared
  volume and then runs the existing compatibility checks unchanged. Storage checks
  stay mandatory even when compatibility is bypassed.
- **Publication descriptors in status.** The descriptor returned by `Commit` is
  how every later operation finds the publication, so the agent persists it per
  container and sets Ready only when every required container is confirmed. The
  identity lookup in `GetArtifactMetadata` exists only to recover that descriptor
  after a lost reply or a restart; restores always use the persisted descriptor.
- **Staged restore only.** Every artifact-addressed restore is staged, on PVC as
  well as S3: the protocol has no direct restore by descriptor, and direct restore
  would need the agent to mount the store itself. `pageBroker.restoreMode` applies
  only to legacy content restored through the filesystem selector. Direct restore
  of bound PVC content is a separate decision after the PVC qualification gate.
- **Cleanup separate from success.** Unmount before `Commit`; if unmount fails,
  report the restore result unchanged, skip `Commit` and let transaction expiry
  reclaim staging. Never rerun a restore to retry cleanup.

Snapshot retains Kubernetes authorization, CRIU, canonical manifest creation and
process orchestration.

#### PageBroker: internal API

Extend [pagebroker.proto](../../../agent/pagebroker/v1/pagebroker.proto), using the
existing local Unix socket. Storage extensions use field numbers and failure codes
from 20 upward in `Request.command`, `Response.result` and `Failure.Code`; 12-19
stay reserved for the separate CUDA integration. Only new fields are shown.

```protobuf
// Identity before an artifact has been published.
message ArtifactIdentity {
  string artifact_uid = 1;      // Stable artifact UID for the owning PodSnapshotContent.
  string container_name = 2;    // Captured container within that content.
}

// Binds a logical artifact to its store before a publication handle is known.
message ArtifactTarget {
  string store_id = 1;           // Require the configured destination store.
  ArtifactIdentity artifact = 2; // Logical checkpoint destination.
}

// Confirmed publication, persisted by Snapshot for later operations.
message PublishedArtifact {
  string store_id = 1;                 // Require the original configured store.
  string artifact_handle = 2;          // Opaque locator within that store.
}

message PrepareStagedCheckpointRequest {
  ArtifactTarget target = 3; // Logical destination instead of a caller's path.
}

message StagedRestoreRequest {
  PublishedArtifact artifact = 3; // Exact committed source to download and verify.
}

message CommitComplete {
  PublishedArtifact published_artifact = 1; // Checkpoint result; absent for restore Commit.
}

// Exactly one of artifact or target is required.
message GetArtifactMetadataRequest {
  PublishedArtifact artifact = 1; // Exact committed publication whose metadata is needed.
  ArtifactTarget target = 2;      // Identity lookup when no descriptor was received.
}

// Success result; errors use the existing Failure message.
message GetArtifactMetadataComplete {
  string manifest_directory = 1;  // Shared local directory with verified manifest.yaml.
  PublishedArtifact artifact = 2; // Publication read; the resolved one for a target.
}
```

PageBroker resolves the backend from configuration. Every artifact request rejects
a mixed or missing selector with `INVALID_REQUEST` before any I/O: artifact and
legacy filesystem selectors never combine, and the metadata request carries exactly
one of `artifact` or `target`. Existing wire numbers and legacy PVC reads are
retained. The I/O engine is optional on artifact requests: absent selects the
configured engine, an explicit engine must support the bound store, and it never
selects a store.

**One new operation**, `GetArtifactMetadata`, extends `Request.command` /
`Response.result`. Its logical signature is:

```text
GetArtifactMetadata(
  request_id: string,       // Existing envelope: correlate the reply.
  transaction_id: string,   // Existing envelope: own the temporary metadata directory.
  artifact: PublishedArtifact | target: ArtifactTarget   // Exactly one.
) -> GetArtifactMetadataComplete | Failure
```

PageBroker verifies the store binding, committed publication, supported format,
metadata access and integrity, then returns a temporary local directory containing
`manifest.yaml` for the exact requested publication, together with that
publication's descriptor. With a target, PageBroker resolves the single publication
for that store, artifact UID and container and returns its descriptor, or fails with
`ARTIFACT_NOT_FOUND`; the lookup reads only and never creates, completes or
substitutes a publication. A mismatch returns `Failure`; PageBroker never
substitutes another publication. Snapshot decides how to use this metadata; restore
compatibility checks and recovery after a lost `Commit` reply are the two caller
flows. Full payload verification belongs to `StagedRestore`.

The directory remains valid until the transaction is closed with `Abort` or expires.
The caller releases it after reading, on every exit path. Cleanup removes temporary
metadata, never the publication.

Artifact deletion and orphan listing belong to maintenance backends, not the
PageBroker RPC. PageBroker still releases its own transaction resources through
`Abort` and restore `Commit`; these never delete retained checkpoints.

Extend `Failure.code` with `STORE_MISMATCH`, `ACCESS_DENIED`, `STORAGE_UNAVAILABLE`,
`ARTIFACT_NOT_FOUND`, `ARTIFACT_CORRUPT`, `UNSUPPORTED_ARTIFACT`,
`TRANSACTION_EXPIRED` and `OUTCOME_UNKNOWN`. `TRANSACTION_EXPIRED` changes the
current contract, which returns `TRANSACTION_NOT_FOUND` for expired transactions;
PageBroker returns it for an expired transaction it still remembers, keeps
`TRANSACTION_NOT_FOUND` for unknown IDs, and the agent client adds it to its
terminal-failure set. The agent treats any failure code it does not recognize as
terminal, so during a rolling upgrade an old agent meeting the new code and a new
agent meeting the old code both stop the same way; no version gate is needed. An
access error never proves absence.
Use existing conditions with storage-specific reasons. Keep errors bounded and
credential-free: a storage-code `Failure.message` is at most 1024 UTF-8 bytes and
never carries credentials or provider response bodies; the agent replaces a longer
or invalid message with a fixed note and keeps the code. Legacy codes keep their
existing diagnostics.

Reuse request/transaction IDs and the 64 KiB message limit. A transaction's maximum
lifetime starts at successful Prepare and retries never extend it. PageBroker checks
expiry before every new storage operation and immediately before publishing the
checkpoint index; expiry returns `TRANSACTION_EXPIRED` and leaves unconfirmed data
for sweeping. Every SDK request uses a deadline no later than the transaction expiry,
so no new request starts after it. Expiry bounds request issue, not provider
completion: a request the provider accepted before expiry may finish after it, and
the next sweep removes what it leaves behind. Bound individual
transfers through broker timeouts and SDK retries. Extend `Abort` to stop transfers
before cleanup; it cannot undo committed data or CUDA/CRIU. Restore `Commit` cleans
local staging.

### Security

Helm values contain Secret references only. PageBroker and maintenance may share
one credential Secret or use separate ones; the required permissions differ:

| Component | S3 actions on the configured bucket and prefix |
| --- | --- |
| Node PageBroker | `GetObject`, `PutObject`, `HeadObject`, `ListBucket` (prefix-scoped), `AbortMultipartUpload` |
| Operator maintenance | `ListBucket` (prefix-scoped), `HeadObject`, `DeleteObject` |

These permissions come from the provider's policy attached to the credentials.
Kubernetes service-account RBAC governs only Kubernetes API access and grants no
storage rights. Kubelet projects each component's credential Secret to a fixed path, `/etc/snapshot/s3-credentials/credentials`, in both node
PageBroker and the operator manager container, using the provider SDK's native
credentials-file format (e.g. the AWS shared-credentials-file syntax). This
path and format are part of the wire contract, not implementation-defined per
component. The agent receives no credential file; workload requests and RPCs
carry references.

The operator manager is an S3-credentialed component, not just a Kubernetes-API
client: a process compromise exposes storage credentials, and a maintenance-path
bug (an unbounded retry loop or a memory leak in a storage SDK) can affect the
reconcile loop that owns every other content object. Mitigate with panic recovery
per work item, resource limits sized for the combined workload, and keeping the
storage SDKs on the same audited dependency set PageBroker uses.

An external manager or administrator rotates the Secret. Use a projection that
supports updates, without a fixed `subPath`. Both PageBroker and the operator
manager use refreshable credential providers. If an SDK cannot reload the projected
file, the component observes the projection directory and atomically replaces its
storage client after parsing a valid update. New operations use the replacement;
in-flight operations retain the old client only for their bounded lifetime. An
invalid update fails new operations closed and surfaces a credential condition.
Neither component requires Kubernetes Secret-read permission or a Pod restart.

Use verified TLS and an optional custom CA. Scope storage permissions to the
configured store, restrict local sockets/staging, and exclude secrets from logs
and status. Maintenance checks artifact UID, store binding and fresh ownership
before deletion; protect against arbitrary paths and symlinks. The operator's
service account gets no GPU or host-process access; its storage rights come only
from the maintenance credential Secret.

### Configuration

PVC remains the default. `storage.type` already selects the maintenance backend;
the `s3` block below replaces the reserved `s3.uri` placeholder in the chart.

**Supported stores.** `s3` means the S3 API, not one vendor. The backend works
against AWS S3 or any S3-compatible store, selected by `endpoint`; an empty
endpoint means AWS. A store must provide Signature V4 authentication with a static
access key and secret, `ListObjectsV2`, multipart upload, and read-after-write
consistency for `PUT` followed by `GET` and `LIST`. Stage 2 fencing additionally
needs conditional `PUT` with `If-None-Match`. Stores that do not speak the S3 API,
and provider-native identity models, are separate backends behind the same
adapter interface.

```yaml
storage:
  type: s3                       # AWS S3 or any S3-compatible store, selected by endpoint
  s3:
    bucket: checkpoint-bucket
    prefix: snapshots
    region: us-east-1
    endpoint: ""                 # Empty = AWS; HTTPS endpoint of any S3-compatible store
    addressingMode: ""           # path | virtual-host; default virtual-host when endpoint is empty, else path
    auth:
      mode: secret              # Stage 1; workloadIdentity may be added later
      credentialsSecretRef:      # Shared by PageBroker and maintenance unless overridden
        name: snapshot-s3-credentials
        key: credentials
      pageBroker:
        credentialsSecretRef: {} # Optional override for node PageBroker
      maintenance:
        credentialsSecretRef: {} # Optional override for operator maintenance
    tls:
      caBundleConfigMapRef: ""   # Optional custom CA
```

The chart renders the identity part into `storage.yaml` in the configuration
ConfigMap, which the operator and PageBroker decode into the shared `api/storage`
configuration. The populated block selects the backend; two blocks or none is a
configuration error, and there is no discriminator key. The chart's existing
`storage.type` key only picks which block to render:

```go
type Config struct {
    PVC *PVC `json:"pvc,omitempty"` // Exactly one of PVC or S3 is set
    S3  *S3  `json:"s3,omitempty"`
}

// S3 is the S3 API configuration. Endpoint selects the store; empty means AWS.
type S3 struct {
    Bucket         string `json:"bucket"`
    Prefix         string `json:"prefix"`
    Region         string `json:"region"`
    Endpoint       string `json:"endpoint"`
    AddressingMode string `json:"addressingMode"` // path | virtual-host; see default below
}
```

`addressingMode` is a client setting, not part of `storeID`. When omitted it
defaults to `virtual-host` if `endpoint` is empty and `path` otherwise; the chart
schema and both components apply the same rule. Nothing else goes in this file:
no derived `storeID`, no credentials, no CA path, no limits.

Settings reach PageBroker and the operator manager through three channels, by
kind:

| Kind | Channel | PageBroker | Operator manager |
| --- | --- | --- | --- |
| Store identity | `storage.yaml`, mounted read-only at `/etc/snapshot/storage.yaml` | `--storage-config` | `--snapshot-storage-config` |
| Credentials | Secret projected without `subPath` at `/etc/snapshot/s3-credentials/credentials` | `--s3-credentials-file` | same path, maintenance client |
| Custom CA | ConfigMap from `tls.caBundleConfigMapRef` projected at `/etc/snapshot/s3-ca/ca.pem` | `--s3-ca-file`; absent means system trust | same |
| Resource and transfer limits | Command-line flags rendered from `pageBroker.*` Helm values, like the existing `--max-concurrent-requests` | `--s3-upload-part-bytes`, `--s3-upload-buffer-bytes`, `--staging-bytes`, `--transaction-lifetime` | worker pool size, per-task timeout, retry backoff |

The chart owns the defaults; each component validates ranges and refuses to start
on an invalid value. Plain HTTP is not configurable. No environment variable is part
of the contract: if an SDK needs one internally, the component sets it from the
flag. `--transaction-lifetime` keeps the semantics under
[PageBroker: internal API](#pagebroker-internal-api): it starts at successful
Prepare, retries never extend it, and every SDK request deadline is bounded by it.

- In S3 mode, omit checkpoint PVC mounts on the agent; preserve node staging. In
  PVC mode, the operator manager mounts the shared claim directly.
- Match Snapshot/PageBroker image versions. PageBroker reads mounted configuration
  and has no Kubernetes client.

### Storage and lifecycle

#### Publication and artifact format

Store the existing per-container bundle under
`<prefix>/artifacts/<artifactUID>/containers/<name>/`, where `<prefix>` is the
configured S3 prefix and nothing else. Upload immutable payloads under `data/`, then
publish the index last. For the example configuration and content UID
`7f3a9c2e-1d4b-4e8f-9a6c-2b5d8e1f0a37`, container `main`, the keys in
`checkpoint-bucket` are:

```text
snapshots/artifacts/7f3a9c2e-1d4b-4e8f-9a6c-2b5d8e1f0a37/containers/main/index.json
snapshots/artifacts/7f3a9c2e-1d4b-4e8f-9a6c-2b5d8e1f0a37/containers/main/data/<relative-path>
```

The index is a contract between PageBroker, which writes and reads it, and
maintenance, which inspects it. One object per publication:

```json
{
  "format": "snapshot.s3-checkpoint/v1",
  "storeID": "store-v1-fa5448e4870e1058af946de52383baa7dacfd92dfe74f70c821d87a734b349d9",
  "artifactUID": "7f3a9c2e-1d4b-4e8f-9a6c-2b5d8e1f0a37",
  "containerName": "main",
  "rootMode": 448,
  "directories": [
    { "path": "criu", "mode": 448 }
  ],
  "files": [
    { "path": "criu/pages-1.img", "mode": 384, "size": 1073741824, "sha256": "<64 lowercase hex>" },
    { "path": "manifest.yaml", "mode": 384, "size": 2048, "sha256": "<64 lowercase hex>" }
  ]
}
```

- Exactly these top-level fields. An unknown or missing field, or an unknown
  `format`, is `UNSUPPORTED_ARTIFACT`.
- `storeID`, `artifactUID` and `containerName` must equal the caller's binding and
  the key the index was read from; a mismatch is `STORE_MISMATCH` or
  `ARTIFACT_CORRUPT` before any payload download.
- `path` is relative to the publication root and names the object at
  `data/<path>`; it is safe (no absolute, `..` or empty components), valid UTF-8,
  at most 512 bytes, unique, sorted, and its parent directory is listed first.
  `mode` is the nine permission bits as a decimal integer, at most `0777`.
- Limits: index at most 4 MiB, at most 10 000 entries, total `size` at most 1 TiB,
  `manifest.yaml` present with 1 byte to 1 MiB.
- Maintenance reads only `format`, the three identity fields and `files[].path`.

Reject unsafe paths, unsupported formats, missing files and checksum mismatches.

The index lives at a fixed key, `containers/<name>/index.json`, so there is exactly
one publication per content and container. `Commit` is idempotent for that key: if
a matching index is already there, it returns the same `PublishedArtifact` without
writing. Since a new capture attempt receives a new `artifactUID`, retries and
restarts of one attempt land on the same key while a new attempt cannot adopt an
old publication.

Each S3 index records the `storeID`, `artifactUID` and container name it was
published for. A retried `Commit` and every restore compare those three fields with
the caller's binding and refuse the index on any mismatch, which catches a bundle
copied between stores or prefixes and any mismatch between the key and the body.
Every component already holds those values, so no new API field is needed.
`storeID` is the only derived identity; its encoding is defined under
[Kubernetes API and operator](#kubernetes-api-and-operator).

A PVC publication has no index: the directory is the publication and its relative
path is the handle. The fixed path still holds exactly one publication per content
and container, so a target that is already published or still being published is
refused at Prepare and again at Commit with `TRANSACTION_CONFLICT` and left
untouched, and a retried `Commit` on the same transaction returns the same
`PublishedArtifact` without publishing again. The store binding is checked against
the configured `storeID` on every PVC request; there is no per-publication identity
to compare beyond the path.

Worked example. Content `psc-7f3a` is created bound to store `store-v1-9c1e…`; its
UID is the artifact UID and the pod has one container, `main`.

1. The agent sends `prepare_staged_checkpoint` with the target. PageBroker reserves
   `artifacts/<uid>/containers/main/`.
2. Capture finishes and the agent sends `commit`. PageBroker uploads the data
   objects under that path, then writes `index.json` last; the index records
   `store-v1-9c1e…`, the UID and `main`.
3. The reply is lost. The agent cannot tell whether step 2 finished.
4. The agent sends `commit` again on the same transaction. PageBroker checks
   `containers/main/index.json`. Absent: finish the upload to that path. Present
   with matching identity fields and passing verification: return the same
   `PublishedArtifact` without writing. Present with different identity fields or
   failed verification: conflict, fail closed and leave it for repair. There is
   never a second publication.

On restore, PageBroker compares the index's identity fields with the bound store,
the content UID and the container name and refuses it on a mismatch, before any
data is downloaded.

A retried capture is a new content object with a new UID, so its path differs and
it can neither adopt nor overwrite the old publication.

Example publication descriptors; digest values are placeholders:

```protobuf
// PVC
store_id: "store-v1-<pvc-digest>"
artifact_handle: "artifacts/<artifactUID>/containers/main"
```

```protobuf
// S3
store_id: "store-v1-<s3-digest>"
artifact_handle: "artifacts/<artifactUID>/containers/main/index.json"
```

Reader selection uses the **backend, plus the index version on S3**. PVC keeps its
directory representation and its existing reader, with no index and no migration;
S3 reconstructs the same logical bundle from index-listed objects at
`<publication>/data/<relative-file-path>`. CRIU/CUDA retain their producer formats.

### Performance and Scalability

Bound transfers separately from CUDA admission. Size memory-backed staging against
both containers' limits; reject insufficient capacity before capture. Cap the
maintenance worker pool size and sweep batch size explicitly — worker concurrency
adds directly to the operator process's own resource footprint. Measure throughput,
peak memory and latency before setting defaults.

### Monitoring

Use existing conditions with storage-specific reasons and rate-limited updates.
Measure phase duration, transfer bytes/retries, credential failures, staging
pressure and cleanup backlog. Expose workqueue depth, per-key retry counts and
worker utilization as operator metrics. Count transaction expiry, credential reload
success/failure and cleanup deferrals.
Correlate logs with content, transaction and work-item key. Report cleanup separately
and preserve historical success.

### Dependencies

- Always-running PageBroker, shared local staging and its separate image.
- Separate CUDA CustomStorage integration and existing Snapshot compatibility checks.
- S3-compatible service plus C++ PageBroker and Go maintenance SDKs/adapters.
- Shared store-ID, artifact-format and publication/read/delete contracts.

Protobuf tags are settled in
[pagebroker.proto](../../../agent/pagebroker/v1/pagebroker.proto). Before coding the
backends, finalize the credentials-file format and SDKs, canonical store-ID encoding
and numeric resource/time limits. Finalize the active-operation and
generation-fencing protocol before enabling Stage 2 concurrent deletion.

### Test Plan

Tracking and test issues are not yet assigned. Cover unit/configuration checks,
backend integration with a disposable S3-compatible service, and GPU end-to-end tests.

- **API/configuration:** immutable store bindings, unique container descriptors,
  index refusal on identity mismatch, identical Go/C++
  store-ID results for endpoint/region/prefix variants, flipping `addressingMode`
  leaves `storeID` unchanged and old content restorable, and legacy PVC.
- **Restore mode:** every bound restore takes the staged path on both backends,
  whatever `restoreMode` is configured; legacy unbound PVC restore keeps the
  configured mode.
- **Helm/security:** S3 installation without checkpoint PVC; credentials only in
  PageBroker/operator manager; matching images and an operator service account
  without GPU/host mounts. Rotate and revoke credentials while already-running Go
  and C++ clients prove that subsequent operations use the replacement without a
  Pod restart.
- **PVC-first maintenance:** per-content deletion and scheduled sweeps through the
  in-process worker pool; legacy layout, artifact UID checks, symlink refusal and
  finalizer completion.
- **Workqueue lifecycle:** duplicate enqueues coalescing to one key, operator
  restart/leadership change rebuilding pending work, retry backoff and exhaustion
  (cap then surface a condition), graceful `ShutDownWithDrain` under SIGTERM,
  immediate cancellation on leadership loss, and every maintenance mode using the
  same `(storeID, artifactUID)` lock, including candidates resolved by a sweep.
- **Storage/RPC:** disposable S3-compatible service; upload/index publication,
  format/path/digest rejection, fixed transaction expiry across retries, rejection
  immediately before a late index publication, bounded messages, maintenance
  pagination, Abort and inspection expiry.
- **Failure:** invalid credentials before capture, outage during transfer, lost
  replies/restarts, idempotent `Commit` retry, conflicting index fails closed, agent
  loss after publication recovers the descriptor through the identity lookup or
  leaves the content not Ready on `ARTIFACT_NOT_FOUND`, and no CUDA/CRIU replay.
- **Lifecycle/concurrency:** inspection cleanup on success/refusal/requeue/cancellation;
  deletion checks served from the informer cache; immediate deletion on both backends,
  including S3 deletion under a running download; expiration during upload/download; an index `PUT`
  accepted before expiry that completes after cleanup is removed by the following
  sweep; stale publishers, incomplete attempts and capture-node loss. Stage 2 adds concurrent
  delete/read/publish races, operation expiry and stale-generation rejection.
- **End to end:** cross-node capture/restore/delete; recovered workload state and
  inference; restore after S3 loss following staging; PVC↔S3-A↔S3-B mismatch
  refusal; store switch round trip S3-A → PVC → S3-A restores content captured
  before the switch; cleanup blocked by an inactive store reports the condition
  and keeps the finalizer, and the release annotation drops it.
- **Operations:** denied decryption/deletion, supported provider versioning/retention
  and PVC cleanup parity.

Qualify the real CUDA path, multi-rank and partial-destination failures, and vLLM,
SGLang and TensorRT-LLM workloads. Run the same lifecycle on an external S3 endpoint;
an emulator or mocked CUDA path alone does not qualify provider/GPU support.

### Graduation Criteria

- **Alpha (Stage 1):** demonstrate S3 capture/restore without a checkpoint PVC and
  immediate deletion with PVC deletion unchanged; prove transaction expiry,
  idempotent `Commit` retry, PVC compatibility, credential rotation and core
  failure paths.
- **Beta (Stage 2):** qualify advertised providers and GPU workloads under sustained
  load; add explicit active-operation tracking and generation fencing, then prove
  concurrent cleanup/read/publication safety across component and leader restarts.
- **GA:** document support and upgrades/rollback; demonstrate artifact/API
  compatibility and supported versioning/retention behavior. Multi-store support is
  not required.

## Alternatives

- **Maintenance Jobs (`batch/v1`), one per task:** isolates storage credentials
  and failures from the operator manager, and reuses Kubernetes for scheduling,
  retries (`backoffLimit`) and completion tracking (Job `Complete`/TTL). Rejected
  as the primary design for Stage 1 because per-task Pod startup adds latency and
  scheduling load at sweep scale, and because it needs a second image/binary
  (`operator/maintenance/main.go`) and its own RBAC surface. A variant keeps the
  existing workqueue for scheduling and dispatches a Job per task; it keeps the
  Pod overhead and adds the dispatch protocol, so it is not chosen either. Revisit
  if holding S3 credentials and transfer failures inside the operator process
  proves too costly.
- **Maintenance sidecar:** isolates credentials from the manager container without
  per-task Pod startup, but needs its own dispatch protocol and still shares the
  operator Pod's lifecycle. Not chosen for Stage 1.
- **Stage 2 fencing authority:** Kubernetes operation leases let the operator and
  agents coordinate through the existing API but require PageBroker cancellation
  to be authoritative; storage-resident epochs let every PageBroker enforce a fence
  directly but require conditional-write and consistency guarantees from every
  supported provider. Stage 2 selects one protocol after Stage 1 concurrency and
  failure testing; Stage 1 does not claim either mechanism.
- **Storage CRD reference as the binding:** a future CRD for multiple stores could
  carry the binding as an object reference instead of a digest. Rejected for
  Stage 1 because a reference names a mutable object and cannot prove which
  location an artifact was written to; the digest can. The two compose later as
  `storeRef` plus `storeID`, so adopting the digest now does not conflict with
  that design.
- **Streaming restore or storage classes now:** useful later, but expand Stage 1
  lifetimes and selection semantics; retain full staging and one bound store.
