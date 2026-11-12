<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-248: SnapshotStorageClass

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
    - [SnapshotStorageClass](#snapshotstorageclass)
    - [Request and content fields](#request-and-content-fields)
    - [Resolution](#resolution)
    - [Default class](#default-class)
    - [Store uniqueness](#store-uniqueness)
    - [Agent and PageBroker](#agent-and-pagebroker)
    - [Credential delivery](#credential-delivery)
    - [Maintenance](#maintenance)
  - [Security](#security)
  - [Configuration](#configuration)
    - [Upgrade from a single-store installation](#upgrade-from-a-single-store-installation)
  - [Monitoring](#monitoring)
  - [Dependencies](#dependencies)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Snapshot keeps every snapshot in one store chosen at Helm install time. This
proposal lets a cluster hold several stores at once and lets each snapshot
request pick one. An administrator registers a store as a cluster-scoped
`SnapshotStorageClass`; a `PodSnapshot` or `SnapshotJob` names a class in
`spec.storageClassName`, or omits it to use the default. Classes are
added and removed while the cluster runs. The chart renders today's `storage.*`
values as the default class, so existing installations need no changes.

## Motivation

Teams already own storage and prefer it over the cluster-wide default: it is
faster, closer to the source Pod, or inside their own security boundary. Today
the only way to use it is a separate Snapshot installation. SNEP-237 left room
for this: content records a `storeID` digest, and "a storage CRD" was to add a
reference beside it. This proposal is that CRD.

### Goals

- Register more than one snapshot store in a cluster, PVC or S3-compatible.
  An S3 store is added without restarting Snapshot; a PVC store needs its claim
  mounted on the operator and agents first, a Helm upgrade and a restart.
- Let a `PodSnapshot` or `SnapshotJob` select a store by name, defaulting to
  the installation's store.
- Bind each `PodSnapshotContent` to one store at creation, and refuse two
  classes that describe the same location.
- Protect a store while content references it, and let an administrator decide
  whether deleting content also deletes its snapshots.

### Non-Goals

- Provisioning storage. A class points at a PVC or bucket that already exists.
- Mounting a PVC the chart did not mount. In this iteration a PVC class must
  name a claim already mounted on the operator and every agent.
- Moving snapshots between stores, or falling back to another store when the
  chosen one is unavailable.
- Namespaced classes, per-namespace quotas, or RBAC on who may name a class.
- Changing SNEP-237's artifact layout, `storeID` encoding or publications.

## Proposal

A `SnapshotStorageClass` is to a snapshot store what `StorageClass` plus
`PersistentVolume` are to a disk: it names a prepared location, carries the
settings to reach it, and protects it while something still depends on it.

**Registering a store.** The administrator creates the PVC or bucket, puts any
credentials in a Secret in the Snapshot namespace, and creates a
`SnapshotStorageClass`. The operator computes the `storeID`, refuses a location
another class already holds, and sets `Ready` once a marker write succeeds; that
write is the credential check. A class that is not Ready re-validates when its
Secret or ConfigMap changes, and on a backoff.

**Selecting a store.** A `PodSnapshot`, or the `SnapshotJob` template, names a
class in `spec.storageClassName`, or omits it for the default. The operator
resolves it once and binds the content to that store for its whole life.

**Retiring a store.** A protection finalizer holds the class while content
references it. A terminating class takes no new snapshots but still restores
and cleans up. `reclaimPolicy` decides whether deleting content also deletes
its snapshots: `Delete` removes them, `Retain` keeps them. The policy is copied
onto the content at binding and does not change afterwards.

**The default.** The chart renders its `storage.*` values as a class named
`default` with `isDefault: true`, so an upgraded installation needs no manual
step. The default may move to another class; bound content keeps its binding.
At most one class is the default; see [Default class](#default-class).

### User Stories

- A team has a nearby high-bandwidth S3-compatible store. The administrator
  creates a `fast-s3` class; the team's `SnapshotJob` names it; others default.

### Limitations, Risks, and Mitigations

| Risk | What goes wrong | How the design handles it |
| --- | --- | --- |
| PVC not mounted | A PVC class names a claim the agents cannot see; captures would fail on every node. | The operator marks the class `Ready=False`, reason `ClaimNotMounted`, by checking the claim against the mounted set the chart renders. Adding a claim is a Helm upgrade (`storage.pvc.additionalClaims`). Documented limitation for this iteration. |
| Agent without the claim | The class is `Ready`, because the operator reads the chart's mounted set, but a particular agent does not have the claim: it still runs the previous pod spec during a rolling upgrade, or the claim is missing on its node. | The agent maps `claimName` to a mount path from its own `config.yaml` and checks the path exists before any I/O. A claim it cannot map fails the operation with `StorageClassUnavailable`, and nothing is retried against another store. The existing retry succeeds once the node's agent has the mount. |
| Class edited after binding | The location fields change; content bound to the old `storeID` would read or delete in the wrong place. Or `reclaimPolicy` flips and existing content loses data it was meant to keep. | Location fields are immutable through CEL, and the policy is frozen on the content at binding. Credentials and TLS settings stay editable because they are not identity inputs. The agent and maintenance compare the class's `status.storeID` with the content's binding before any I/O and refuse a mismatch. |
| Class deleted under content | Content loses the only description of where its snapshots are. | Protection finalizer; a terminating class still resolves for existing content. Force-removing the finalizer leaves content with the existing `StoreInactive` handling from SNEP-237. |
| Secret missing or revoked | An S3 class's Secret is deleted or rotated to an invalid key. | The class goes `Ready=False`, reason `CredentialsUnavailable`, and new bindings stop. In-flight operations fail with the existing credential errors; nothing is deleted on an access error. |
| Agent holds credentials | Today only PageBroker and the operator see S3 credentials. The agent now reads the class's Secret and writes it to a local file for PageBroker. | The Secret must live in the Snapshot namespace; the agent's RBAC is a Role there, not a ClusterRole. The file is written to the memory-backed volume the two containers already share, mode `0600`, under a per-transaction directory removed on Commit or Abort. Accepted trade-off against giving PageBroker a Kubernetes client; see Alternatives. |
| Duplicate or overlapping location | Two classes describe the same place, through the same fields, through an alias such as a second endpoint hostname, or through a nested prefix; cleanup through one could reach the other's content. | The operator refuses `Ready` for the later class with reason `DuplicateStore` or `StoreOverlap` and names the other; see [Store uniqueness](#store-uniqueness). |
| Nested store in another cluster | Another cluster or installation registers a class above or below this one's prefix in the same bucket or claim. The scan cannot see it, and a sweep through the parent could delete the child's data. | The ancestor-marker check refuses the child when the parent registered first. A parent registered later is not detected, so a sweep never deletes inside a subtree whose marker names another store, and deleting content removes only its own recorded handles. Data written by something that leaves no marker is not protected. |
| Old agent, new class | During a rolling upgrade an agent that predates this proposal receives content bound to a non-default class. | It passes its startup store to PageBroker, whose `storeID` does not match the content's; PageBroker returns `STORE_MISMATCH` and the capture fails before any I/O. The existing retry picks it up after the node upgrades. |
| Default misconfigured | Two classes set `isDefault: true`, or the default is deleted and unnamed `PodSnapshot`s fail. | The admission policy denies both. If it is absent or bypassed, the oldest marked class wins with `DefaultConflict` on the others, and a Warning event flags a deleted default; see [Default class](#default-class). |

## Design Details

### API

#### SnapshotStorageClass

A new cluster-scoped kind in `snapshot.nvidia.com/v1alpha1`, a discriminated
union: `type` names the backend and exactly that block is set. Its location
fields are the `storeID` inputs defined in `../../../api/storage` and are
immutable (CEL `self == oldSelf`). `reclaimPolicy`, credentials and TLS settings
stay editable; an edit to `reclaimPolicy` applies to content bound afterwards.

```go
// +kubebuilder:resource:scope=Cluster
type SnapshotStorageClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   SnapshotStorageClassSpec   `json:"spec"`
    Status SnapshotStorageClassStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(self.type == 'PVC') == has(self.pvc)",message="pvc must be set exactly when type is PVC"
// +kubebuilder:validation:XValidation:rule="(self.type == 'S3') == has(self.s3)",message="s3 must be set exactly when type is S3"
type SnapshotStorageClassSpec struct {
    // +kubebuilder:validation:Enum=PVC;S3
    // +unionDiscriminator
    Type StoreType `json:"type"`
    // IsDefault selects this class for PodSnapshots that name none.
    IsDefault bool `json:"isDefault,omitempty"`
    // +kubebuilder:validation:Enum=Delete;Retain
    // +kubebuilder:default=Delete
    ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
    PVC *PVCStoreSpec `json:"pvc,omitempty"`
    S3  *S3StoreSpec  `json:"s3,omitempty"`
}

// A claim in the Snapshot namespace that the chart already mounts.
type PVCStoreSpec struct {
    ClaimName string `json:"claimName"`
    BasePath  string `json:"basePath,omitempty"` // Default "/", the whole claim.
}

// Endpoint, Region, Bucket and Prefix are storeID inputs; the rest are access
// settings and stay editable.
type S3StoreSpec struct {
    Endpoint string `json:"endpoint"`
    Region   string `json:"region"`
    Bucket   string `json:"bucket"`
    Prefix   string `json:"prefix,omitempty"`
    // A Secret in the Snapshot namespace holding a shared-credentials file.
    // The key defaults to "credentials".
    CredentialsSecretRef corev1.SecretKeySelector `json:"credentialsSecretRef"`
    Addressing string `json:"addressing,omitempty"` // virtual or path.
    AllowHTTP  bool   `json:"allowHttp,omitempty"`
    // The key defaults to "ca.crt".
    CABundleConfigMapRef *corev1.ConfigMapKeySelector `json:"caBundleConfigMapRef,omitempty"`
}

type SnapshotStorageClassStatus struct {
    StoreID    string             `json:"storeID,omitempty"` // From the location fields.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

`Ready` carries the reasons `ClaimNotMounted`, `CredentialsUnavailable`,
`DuplicateStore`, `StoreOverlap`, `StoreUnreachable` and `Terminating`. A
separate `DefaultConflict` condition is True on a class marked `isDefault`
while an older class is also marked; `Ready` is unaffected.

#### Request and content fields

`PodSnapshot` (`spec`) and the `SnapshotJob` template
(`spec.podSnapshotTemplate`) gain an optional `storageClassName`; empty selects
the class marked `isDefault`. `PodSnapshotContent.spec.storage` gains the class
name beside the digest, which stays the authority on *where*; the name says
*how to reach it*. It also gains `reclaimPolicy`, copied from the class at
binding and immutable, so editing the class later never changes what happens to
existing content. The class always has a policy (it defaults to `Delete`), so
every binding sets one. Only content bound before classes existed has neither
field, or has no class when no class matches its digest; that content keeps
today's behavior, and an empty policy means `Delete`.

```go
// PodSnapshotContentSpec.Storage; immutable once set.
// +kubebuilder:validation:XValidation:rule="!has(self.storageClassName) || has(self.reclaimPolicy)",message="reclaimPolicy is required when storageClassName is set"
type CheckpointStorageBinding struct {
    StoreID          string        `json:"storeID,omitempty"`
    StorageClassName string        `json:"storageClassName,omitempty"`
    ReclaimPolicy    ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}
```

#### Resolution

1. The `SnapshotJob` reconciler resolves the template's class before it starts
   the source Pod. A missing, not-Ready or terminating class sets
   `InvalidSpec` and no pod is launched, so a long run is not spent on a
   snapshot that cannot be bound.
2. The `PodSnapshot` reconciler reads `spec.storageClassName`, or the class
   marked `isDefault: true`; none marked fails with `StorageClassNotFound`.
3. At binding it requires `Ready=True` and no deletion timestamp, else it sets
   `Ready=False` on the `PodSnapshot` with `StorageClassNotFound`,
   `StorageClassNotReady` or `StorageClassTerminating` and requeues; a watch on
   `SnapshotStorageClass` picks up later changes. This check is the authority:
   the job check is early, this one is where the `storeID` is frozen.
4. It creates the `PodSnapshotContent` with `spec.storage.storeID`,
   `storageClassName` and `reclaimPolicy`, labeled `nvidia.com/snapshot-storage-class=<name>`.
5. The class reconciler's `nvidia.com/snapshotstorageclass-protection`
   finalizer is removed only when the class is terminating and no content
   carries its label.

#### Default class

At most one class should have `isDefault: true`, and the default should not
disappear unnoticed. Two layers hold that.

1. **Admission.** The chart installs a `ValidatingAdmissionPolicy` that denies
   setting `isDefault: true` while another class has it, and denies deleting
   the default. The default is moved first: clear `isDefault` on the old class,
   then set it on the new one. In between, an unnamed `PodSnapshot` fails with
   `StorageClassNotFound` and retries.
2. **Operator.** Admission is not serialized and the policy may be absent. If
   two classes are marked, the earliest `creationTimestamp`, ties by name, is
   the default and the others get `DefaultConflict` naming it; `Ready` is
   unaffected and no `PodSnapshot` fails. If the default is deleted without
   the policy, a Warning event names the effect: unnamed `PodSnapshot`s fail
   with `StorageClassTerminating` while content references it, and
   `StorageClassNotFound` after. The chart recreates its `default` class on the
   next `helm upgrade` while `createDefaultClass` is true.

#### Store uniqueness

One location has exactly one class. When a class is created, the class
reconciler runs these checks in order:

1. **Digest.** Another class with the same `storeID` is `DuplicateStore`.
2. **Overlap.** Another class on the same claim or in the same S3 bucket whose
   `basePath` or prefix is a parent or child of this one's is `StoreOverlap`;
   siblings such as `team-a/` and `team-b/` are allowed. The scan reads the
   class informer cache and compares only immutable location fields, so a result
   changes only when a class is added or deleted. The later
   class by `creationTimestamp`, ties by name, is refused, so two classes
   created at once get the same answer in any reconcile order.
3. **Ancestor markers.** The operator reads the marker at each ancestor prefix
   (S3) or ancestor directory under the claim root (PVC). A marker for another
   store is `StoreOverlap`; this sees stores registered by another cluster,
   which the scan cannot. A read the credentials deny, or a missing marker, is
   skipped, so a class whose credentials cover only its own prefix still
   reaches `Ready`. Sweeps do not repeat it: they delete only inside their own
   prefix, so a parent registered later changes nothing for them.
4. **Marker.** Before `Ready`, the operator creates `<location>/.snapshot-store`
   with the class name and `storeID`, create-only (`If-None-Match: *` on S3,
   `O_EXCL` on PVC). A marker for another class is `DuplicateStore`. The
   storage arbitrates, so two classes created at once, in this cluster or
   another, cannot both win, and a bucket reached through two hostnames is
   caught. Sweeps read it again before they delete.

When a class is deleted, the operator removes its marker before it removes the
finalizer, and only if the marker names this class and `storeID`; a marker that
is already gone counts as removed. A failed delete is retried on a backoff while
the class stays terminating, with reason `StoreUnreachable`, or
`CredentialsUnavailable` when the credentials are the cause. An administrator
can force-remove the finalizer; the marker then stays, is reported, and is
never overwritten, so it must be deleted by hand before the location is reused.

Descendants are not checked: a class registered above an existing store in
another cluster cannot be found without listing the whole prefix, so it reaches
`Ready`, and [Maintenance](#maintenance) guards deletion instead. Only Snapshot
writes markers, so data placed there by anything else is not detected.

#### Agent and PageBroker

The agent already carries `storeID` and the artifact identity to PageBroker; it
adds the resolved store through the `StorageBackend` selector, so PageBroker no
longer depends on one store fixed at startup. PVC reuses `FilesystemStorage`;
S3 is the one new kind. `--storage-config` remains for installations without
classes. Only new fields are shown.

```protobuf
message StorageBackend {
  oneof kind {
    FilesystemStorage filesystem = 1;  // PVC: mount path joined with basePath.
    S3Storage s3 = 2;
  }
}

message S3Storage {
  string endpoint = 1;
  string region = 2;
  string bucket = 3;
  string prefix = 4;
  string credentials_file = 5;  // On the shared staging volume; agent-owned.
  string addressing = 6;
  bool allow_http = 7;
  string ca_bundle_file = 8;
}

message ArtifactTarget    { StorageBackend storage = 3; }  // Resolved class.
message PublishedArtifact { StorageBackend storage = 3; }  // Restore and metadata reads.
```

PageBroker trusts `store_id` and `StorageBackend` as it trusts its startup
configuration today; the binding check is the agent's. It caches S3 clients by
`store_id` and a SHA-256 digest of the credentials and CA bundle file contents,
computed when it builds the client, so rotated credentials get a new client. A missing class or Secret fails the
operation with `StorageClassUnavailable`, and nothing is retried against
another store. For PVC the agent maps `claimName` to a mount path: the default
claim at `/checkpoints`, each extra claim at `/snapshot-stores/<claimName>`,
rendered by the chart into the agent's `config.yaml` and the operator's
`--mounted-claims` flag.

#### Credential delivery

S3 credentials are not mounted: a Secret volume is fixed when the pod starts,
which is why today's single store needs a restart. The agent carries them per
transaction through the staging volume it shares with PageBroker:

1. At Prepare, the agent reads the class's Secret, and the CA bundle ConfigMap
   if set, with an uncached `Get` and writes
   `<staging>/stores/<transaction-id>/credentials` and `ca.crt`, mode `0600`.
2. It passes the paths in `S3Storage`; PageBroker builds its client from that
   file in the shared-credentials format it reads today.
3. It removes the directory on Commit or Abort, and on restart removes
   directories whose transaction is gone. The volume is RAM, so a crash leaves
   nothing on the node.

The directory is per transaction, not per store: concurrent transactions never
share a file, and a rotated Secret reaches the next transaction while an
in-flight one keeps its key. A retried checkpoint or restore is a new
transaction with a new ID and directory, so it reads the Secret again. Sharing
happens in PageBroker's client cache.

#### Maintenance

The backend registry is built from Ready and terminating classes.
`backendForContent` resolves the content's class by name, checks its `storeID`
against the binding, and returns that backend. Under the content's
`reclaimPolicy: Retain`, `delete-content` removes the finalizer without touching
the store and emits `SnapshotsRetained` with the handle. Sweeps run per class
and never cross stores. A sweep first reads the marker at its own
location and stops, reporting why, if it is missing or names another store. It
then lists its prefix once and skips every subtree whose marker names a
different `storeID`; on a PVC it does the same while walking directories.
Deleting content removes only the handles recorded on it. Content whose class
is gone keeps its finalizer and gets `StoreInactive`, as in SNEP-237.

### Security

- **Who may create a class.** Cluster-level RBAC, like `StorageClass`; naming
  one needs only write access to `PodSnapshot` or `SnapshotJob`. A class says
  any `PodSnapshot` may use that store.
- **Credentials.** Secrets live in the Snapshot namespace, replacing today's S3
  file mounts, under a namespaced Role: `get`/`list`/`watch` for the operator,
  `get` for the agent, and no caching of contents. The agent writes a Secret
  only to the staging tmpfs, never to the node filesystem or logs.
- **PageBroker.** No Kubernetes client, RBAC or long-lived credentials; it
  trusts the agent on the local socket as today.

### Configuration

The chart keeps `storage.*` and renders it as the `default` class. New values:

```yaml
storage:
  pvc:
    # Claims to mount for additional PVC classes, in the release namespace.
    additionalClaims: []
  # Create the default class from the values above. Set false to manage classes by hand.
  createDefaultClass: true
```

Classes require `operator.bindNewContents` and `pageBroker.artifactAddressing`;
the chart fails the render otherwise. The operator's `--snapshot-storage-config`
and S3 credential flags are deprecated; with the CRD it reads classes instead.

#### Upgrade from a single-store installation

An installation upgraded from a release without classes keeps working with no
action beyond `helm upgrade`:

1. The same `storage.*` values now render the `default` class, so its `storeID`
   is byte-identical to the one recorded on every bound `PodSnapshotContent`.
2. On start, the operator sets `spec.storage.storageClassName` and
   `reclaimPolicy` on content that has a `storeID` but no class name, by
   matching the digest against Ready classes; content matching none keeps the
   `StoreInactive` handling.
3. Content with no `spec.storage`, bound before SNEP-237, keeps the legacy
   path-addressed flow and is never assigned a class.
4. `PodSnapshot`s and `SnapshotJob`s without `storageClassName` resolve to
   `default`.

Rollback to the previous chart leaves the CRD and `default` object in place;
the old operator ignores both and reads `storage.yaml`.

### Monitoring

- `SnapshotStorageClass` status: `storeID`, `Ready` with the reasons above, and
  `DefaultConflict`. A refused class names the other in its condition and in a
  Warning event once per transition; only the later class is marked.
- Events: `SnapshotsRetained` under `Retain`; `StorageClassTerminating` on the
  class while references remain, with count; a Warning on a terminating default.
- Metrics: `snapshot_storage_class_ready{class}`,
  `snapshot_storage_class_bound_contents{class}`; maintenance metrics gain it.

### Dependencies

- SNEP-237 artifact addressing: bound content, `storeID`, `ArtifactTarget` and
  `PublishedArtifact`; images that accept a per-request `StorageBackend`.

### Test Plan

- **API:** CEL rejects two backend blocks, none, or a location change.
- **Operator:** Ready/NotReady for each reason; digest, overlap, ancestor-marker
  and marker detection, including two classes created at once, a stale marker
  and a denied ancestor read; default resolution with two marked classes and
  with none; terminating class, finalizer, `Retain`, registry rebuild.
- **Admission policy:** rejects a second default and deleting the default,
  accepts moving it, and is skipped when the API is not served; without it,
  deleting the default emits the Warning.
- **Agent:** Secret fetch, file permissions and removal on Commit, Abort and
  crash; mount mapping for default and extra claims; `StorageClassUnavailable`
  on a missing class, a `status.storeID` mismatch, or a claim with no mount.
- **Maintenance:** a sweep stops when its own marker is missing or foreign and
  skips foreign subtrees, on S3 and on a PVC.
- **PageBroker:** per-request `StorageBackend`, client cache, two stores live.
- **End to end:** two S3 and one PVC class in one cluster, checkpoint and
  cross-node restore through each; a class terminates only after its content is
  gone; old content stays put when the default moves; rolling upgrade with an
  old agent fails closed.

### Graduation Criteria

- **Alpha:** CRD and fields as above; PVC and S3 classes; default class from
  the chart; protection finalizer; `Retain`; PVC mount limitation documented.
- **Beta:** a cluster running two or more classes in production; metrics and
  events stable; upgrade and rollback from the single-store chart tested.
- **GA:** API stable for one release; the deprecated operator flags removed.

## Alternatives

- **PageBroker with its own Kubernetes client.** Keeps credentials out of the
  agent, but adds an API dependency, RBAC and a watch to a component driven
  over a local socket. Rejected.
- **Mounting arbitrary PVCs at runtime.** Needs the agent to mount claims or a
  restart per claim; deferred and recorded as the limitation.
