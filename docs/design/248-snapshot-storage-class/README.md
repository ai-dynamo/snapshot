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
  An S3 store is added without reinstalling or restarting Snapshot; a PVC
  store needs its claim mounted on the operator and agent pods first, which
  is a Helm upgrade and a restart.
- Let a `PodSnapshot` or `SnapshotJob` select a store by name; default to the
  installation's store when it does not.
- Bind each `PodSnapshotContent` to one store at creation so checkpoint,
  restore and cleanup use it for its whole life; refuse two classes that
  describe the same location.
- Protect a store while any content still references it, and let an
  administrator decide whether deleting content also deletes its snapshots.

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

**Registering a store.** The administrator creates the backing PVC or bucket,
puts any credentials in a Secret in the Snapshot namespace, and creates a
`SnapshotStorageClass` pointing at them. The operator computes the `storeID`
from the location fields, rejects a digest or prefix that collides with another
class, checks that a PVC claim is mounted or an S3 Secret exists and parses,
claims the location with a store marker, and sets `Ready`. The marker write is
the credential check: it proves endpoint, bucket and key work. A class that is
not Ready re-validates when its Secret or ConfigMap changes, and on a backoff.

**Selecting a store.** A `PodSnapshot` sets `spec.storageClassName`; a
`SnapshotJob` sets it in `spec.podSnapshotTemplate`. When the operator creates
the `PodSnapshotContent`, it resolves the class once and writes the class name
and its `storeID` into the content's immutable `spec.storage`. An unknown or
not-Ready class fails the `PodSnapshot` with a condition naming the class.

**Using the store.** The agent reads the content's class, checks its
`status.storeID` against the content's binding, and hands settings and
credentials to PageBroker with each request, so a class recreated for another
location cannot serve content bound elsewhere. Maintenance does the same.

**Retiring a store.** A protection finalizer holds the class while content
references it. Deleting it marks it terminating: new `PodSnapshot`s may not
name it, existing content keeps restoring and cleaning up through it, and it
goes away with the last reference. `reclaimPolicy` decides what deleting
content does to its snapshots: `Delete` removes them; `Retain` keeps them and
records an event.

**The default.** The chart renders its `storage.*` values as a class named
`default` with `isDefault: true`, so an upgraded installation keeps its store
and bound content with no manual step; see
[Upgrade from a single-store installation](#upgrade-from-a-single-store-installation).
The default may be moved to another class; bound content keeps its binding.
At most one class should be the default. Two marked classes are refused at
admission where the cluster supports it, and otherwise resolved by age and
flagged `DefaultConflict`; see [Default class](#default-class).

### User Stories

- A team has a nearby high-bandwidth S3-compatible store. The administrator
  creates a `fast-s3` class; the team's `SnapshotJob` names it; others default.
- An administrator retires the old default bucket: create a new class, move
  the default to it (clear `isDefault` on the old class, set it on the new one),
  delete the old one. While terminating, no `PodSnapshot` or
  `SnapshotJob` may bind to it, so no new checkpoint is written there; restore
  and deletion of its existing snapshots continue until the last one is gone.

### Limitations, Risks, and Mitigations

| Risk | What goes wrong | How the design handles it |
| --- | --- | --- |
| PVC not mounted | A PVC class names a claim the agents cannot see; captures would fail on every node. | The operator marks the class `Ready=False`, reason `ClaimNotMounted`, by checking the claim against the mounted set the chart renders. Adding a claim is a Helm upgrade (`storage.pvc.additionalClaims`). Documented limitation for this iteration. |
| Agent without the claim | The class is `Ready`, because the operator reads the chart's mounted set, but a particular agent does not have the claim: it still runs the previous pod spec during a rolling upgrade, or the claim is missing on its node. | The agent maps `claimName` to a mount path from its own `config.yaml` and checks the path exists before any I/O. A claim it cannot map fails the operation with `StorageClassUnavailable`, and nothing is retried against another store. The existing retry succeeds once the node's agent has the mount. |
| Class edited after binding | The location fields change; content bound to the old `storeID` would read or delete in the wrong place. | Location fields are immutable through CEL. Credentials and TLS settings stay editable because they are not identity inputs. The agent and maintenance compare the class's `status.storeID` with the content's binding before any I/O and refuse a mismatch. |
| Class deleted under content | Content loses the only description of where its snapshots are. | Protection finalizer; a terminating class still resolves for existing content. Force-removing the finalizer leaves content with the existing `StoreInactive` handling from SNEP-237. |
| Secret missing or revoked | An S3 class's Secret is deleted or rotated to an invalid key. | The class goes `Ready=False`, reason `CredentialsUnavailable`, and new bindings stop. In-flight operations fail with the existing credential errors; nothing is deleted on an access error. |
| Agent holds credentials | Today only PageBroker and the operator see S3 credentials. The agent now reads the class's Secret and writes it to a local file for PageBroker. | The Secret must live in the Snapshot namespace; the agent's RBAC is a Role there, not a ClusterRole. The file is written to the memory-backed volume the two containers already share, mode `0600`, under a per-transaction directory removed on Commit or Abort. Accepted trade-off against giving PageBroker a Kubernetes client; see Alternatives. |
| Duplicate or overlapping location | Two classes describe the same place, through the same fields, through an alias such as a second endpoint hostname, or through a nested prefix; cleanup through one could reach the other's content. | The operator refuses `Ready` for the later class with reason `DuplicateStore` or `StoreOverlap` and names the other. The digest catches equal fields, the overlap rule catches nesting, and the store marker catches aliases; see [Store uniqueness](#store-uniqueness). |
| Nested store in another cluster | Another cluster or installation registers a class above or below this one's prefix in the same bucket or claim. The scan cannot see it, and a sweep through the parent could delete the child's data. | The ancestor-marker check refuses the child when the parent registered first. A parent registered later is not detected, so a sweep never deletes inside a subtree whose marker names another store, and deleting content removes only its own recorded handles. Data written by something that leaves no marker is not protected. |
| Old agent, new class | During a rolling upgrade an agent that predates this proposal receives content bound to a non-default class. | It passes its startup store to PageBroker, whose `storeID` does not match the content's; PageBroker returns `STORE_MISMATCH` and the capture fails before any I/O. The existing retry picks it up after the node upgrades. |
| Two defaults | Two classes set `isDefault: true`. | Where the API server serves `ValidatingAdmissionPolicy`, the chart's policy rejects the second one at admission. Where it does not, or when two are created at once, the operator treats the oldest as the default and sets `DefaultConflict` on the others; classes with an explicit name are unaffected. See [Default class](#default-class). |
| Default deleted | The default class is deleted, so `PodSnapshot`s without a class name fail, including manifests written for the single-store release. | The admission policy denies deleting a class that is the default; move the default first. Without the policy the operator emits a Warning event on a terminating default naming the effect. See [Default class](#default-class). |

## Design Details

### API

#### SnapshotStorageClass

A new cluster-scoped kind in `snapshot.nvidia.com/v1alpha1`, a discriminated
union: `type` names the backend and exactly that block is set. Its location
fields are the `storeID` inputs defined in `../../../api/storage` and are immutable.

```go
// SnapshotStorageClass names a snapshot store an administrator has prepared.
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
    // Type selects the backend block; a new backend adds an enum value, a
    // block and one rule above.
    // +kubebuilder:validation:Enum=PVC;S3
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
    // +unionDiscriminator
    Type StoreType `json:"type"`

    // IsDefault selects this class for PodSnapshots that name none.
    // +optional
    IsDefault bool `json:"isDefault,omitempty"`

    // ReclaimPolicy is what deleting a PodSnapshotContent does to its
    // snapshots in this store.
    // +kubebuilder:validation:Enum=Delete;Retain
    // +kubebuilder:default=Delete
    ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

    // +optional
    PVC *PVCStoreSpec `json:"pvc,omitempty"`
    // +optional
    S3 *S3StoreSpec `json:"s3,omitempty"`
}

// PVCStoreSpec is a claim in the Snapshot namespace that the chart already
// mounts. Field names mirror api/storage.PVC; Namespace is fixed. Both fields
// are storeID inputs and immutable.
type PVCStoreSpec struct {
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="claimName is immutable"
    ClaimName string `json:"claimName"`
    // BasePath is a directory inside the claim; "/" is the whole claim.
    // +kubebuilder:default="/"
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="basePath is immutable"
    BasePath string `json:"basePath,omitempty"`
}

// S3StoreSpec is a prefix in an S3-compatible bucket. The four storeID inputs
// mirror api/storage.S3 and are immutable; the rest are access settings.
type S3StoreSpec struct {
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="endpoint is immutable"
    Endpoint string `json:"endpoint"`
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
    Region string `json:"region"`
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="bucket is immutable"
    Bucket string `json:"bucket"`
    // +optional
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prefix is immutable"
    Prefix string `json:"prefix,omitempty"`
    // CredentialsSecretRef names a Secret in the Snapshot namespace holding
    // a shared-credentials file under Key.
    // +kubebuilder:default={key: "credentials"}
    CredentialsSecretRef corev1.SecretKeySelector `json:"credentialsSecretRef"`
    // +kubebuilder:validation:Enum=virtual;path
    // +optional
    Addressing string `json:"addressing,omitempty"`
    // +optional
    AllowHTTP bool `json:"allowHttp,omitempty"`
    // +optional
    // +kubebuilder:default={key: "ca.crt"}
    CABundleConfigMapRef *corev1.ConfigMapKeySelector `json:"caBundleConfigMapRef,omitempty"`
}

type SnapshotStorageClassStatus struct {
    // StoreID is the digest the operator computed from the location fields.
    // +optional
    StoreID string `json:"storeID,omitempty"`
    // Conditions: Ready, with reasons ClaimNotMounted, CredentialsUnavailable,
    // DuplicateStore, StoreOverlap, StoreUnreachable, Terminating. A separate
    // DefaultConflict condition is True on a class marked isDefault while an
    // older class is also marked; Ready is unaffected.
    // +listType=map
    // +listMapKey=type
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

```yaml
apiVersion: snapshot.nvidia.com/v1alpha1
kind: SnapshotStorageClass
metadata:
  name: fast-s3
spec:
  type: S3
  reclaimPolicy: Retain
  s3:
    endpoint: https://s3.example.com
    region: us-east-1
    bucket: team-a-snapshots
    prefix: prod
    credentialsSecretRef:
      name: team-a-s3-credentials   # key defaults to "credentials"
    addressing: path
status:
  storeID: store-v1-4f2a…
  conditions:
    - type: Ready
      status: "True"
      reason: StoreReady
      message: marker written at prod/.snapshot-store
      lastTransitionTime: "2026-11-08T09:12:00Z"
---
apiVersion: snapshot.nvidia.com/v1alpha1
kind: SnapshotStorageClass
metadata:
  name: default
spec:
  type: PVC
  isDefault: true
  pvc:
    claimName: snapshot-pvc
    basePath: /
status:
  storeID: store-v1-9c1e…
  conditions:
    - type: Ready
      status: "True"
```

#### Request and content fields

`PodSnapshot` and the `SnapshotJob` template gain one optional field.
`PodSnapshotContent.spec.storage` gains the class name beside the digest; the
digest stays the authority on *where*, the name says *how to reach it*.

```go
type PodSnapshotSpec struct {
    // StorageClassName selects the SnapshotStorageClass for this snapshot.
    // Empty selects the class marked isDefault.
    // +optional
    StorageClassName string `json:"storageClassName,omitempty"`
    // ... existing fields
}

type PodSnapshotTemplate struct {
    // +optional
    StorageClassName string `json:"storageClassName,omitempty"`
    // ... existing fields
}

type CheckpointStorageBinding struct {
    StoreID string `json:"storeID"`
    // StorageClassName is the class resolved when this content was created.
    // Empty on content bound before classes existed; it is the installation's
    // default store of that time.
    // +optional
    StorageClassName string `json:"storageClassName,omitempty"`
}
```

```yaml
apiVersion: snapshot.nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: vllm
spec:
  podSnapshotTemplate:
    storageClassName: fast-s3
    targetContainers: [main]
```

#### Resolution

1. The `SnapshotJob` reconciler resolves the template's `storageClassName`
   before it starts the source Pod, beside its existing template checks.
   A missing, not-Ready or terminating class sets `InvalidSpec` with the same
   reasons as below, and no pod is launched; a long run is not spent on a
   snapshot that cannot be bound.
2. The `PodSnapshot` reconciler reads `spec.storageClassName`, or picks the
   class marked `isDefault: true`; see [Default class](#default-class). With no
   class marked it fails with `StorageClassNotFound`.
3. At binding it requires `Ready=True` and no deletion timestamp, else it sets
   `Ready=False` on the `PodSnapshot` with reason `StorageClassNotFound`,
   `StorageClassNotReady` or `StorageClassTerminating` and requeues; a later
   class change is picked up by a watch on `SnapshotStorageClass`. This check
   is the authority: the job check is early, this one is where the `storeID`
   is frozen.
4. It creates the `PodSnapshotContent` with `spec.storage.storeID` from the
   class status and `spec.storage.storageClassName` set, labeled
   `nvidia.com/snapshot-storage-class=<name>` so references are a label query.
5. The class reconciler holds the `nvidia.com/snapshotstorageclass-protection`
   finalizer and removes it only when the class is terminating and no content
   carries its label.

#### Default class

At most one class should have `isDefault: true`, the default should not
disappear unnoticed, and two layers hold that.

1. **Admission.** The chart installs a `ValidatingAdmissionPolicy` and its
   binding for `SnapshotStorageClass`. The policy takes the classes as its
   parameters and denies a create or update that sets `isDefault: true` while
   another class already has it. It also denies deleting a class that has
   `isDefault: true`, so the default must be moved before the class is
   retired. It needs Kubernetes 1.30, where the API
   went GA; the chart installs it only when the API server serves
   `admissionregistration.k8s.io/v1` `ValidatingAdmissionPolicy`, so the
   proposal adds no minimum version (see [Dependencies](#dependencies)).
   Moving the default is then two edits: clear `isDefault` on the old class,
   then set it on the new one. Between them no class is the default, and a
   `PodSnapshot` without a name fails with `StorageClassNotFound` and retries.
2. **Operator.** Admission is not serialized, and the policy may be absent, so
   two classes can still end up marked. The operator then treats the one with
   the earliest `creationTimestamp`, ties broken by name, as the default and
   sets the `DefaultConflict` condition on the others, naming the winner, with
   a Warning event once per transition. `Ready` is unaffected, so every class
   stays usable by name. Resolution is deterministic and no `PodSnapshot`
   fails because of the conflict. Where the policy is absent, deleting the
   default is allowed, so the operator emits a Warning event on a class that
   is both terminating and the default, naming the effect: unnamed
   `PodSnapshot`s fail with `StorageClassTerminating` while content still
   references it, and with `StorageClassNotFound` once it is gone. The chart's
   `default` class is recreated by the next `helm upgrade` while
   `createDefaultClass` is true.

#### Store uniqueness

One location has exactly one class. These checks run in the class reconciler
when a class is created, in this order:

1. **Digest.** Another class with the same `storeID` is `DuplicateStore`.
2. **Overlap.** On creation, the operator looks for another class on the same
   claim or in the same S3 bucket whose `basePath` or prefix is a parent or
   child of this class's; a match is `StoreOverlap`. Siblings such as
   `team-a/` and `team-b/` are two stores and are allowed. The check scans the
   class informer cache and compares only the immutable location fields, so a
   result changes only when a class is added or deleted, and the cost is
   linear in the classes that share the bucket or claim. The later class is
   the one refused, where later means the later `creationTimestamp`, ties
   broken by name. The rule reads specs alone, so two classes created at once
   reach the same answer whatever order they reconcile in.
3. **Ancestor markers.** The operator reads the marker at each ancestor prefix
   of an S3 class, or each ancestor directory under the claim root of a PVC
   class, one read per level of path depth. A marker for another store is
   `StoreOverlap`. This sees a store registered by another cluster or
   installation, which the scan cannot. A read the credentials do not allow, or
   a missing marker, is skipped, so a class whose credentials cover only its
   own prefix still reaches `Ready`; the check protects only where it can see.
   Only registration runs it. A sweep deletes only inside its own prefix, so a
   parent registered later does not change what it may delete.
4. **Marker.** Before `Ready`, the operator creates `<location>/.snapshot-store`
   holding the class name and `storeID`, create-only (`If-None-Match: *` on
   S3, `O_EXCL` on PVC). A marker for another class is `DuplicateStore`. The
   storage arbitrates, so two classes created at once, in this cluster or
   another, cannot both win and a bucket reached through two hostnames is
   caught. The marker goes with the finalizer; one left by a vanished class is
   reported, not overwritten. Sweeps read it again before they delete; see
   [Maintenance](#maintenance).

Descendants are not checked. A class registered above an existing store in
another cluster cannot be found without listing the whole prefix, so it reaches
`Ready`; [Maintenance](#maintenance) guards deletion instead. Markers are
written only by Snapshot, so data placed there by anything else is not
detected.

#### Agent and PageBroker

The agent already carries `storeID` and the artifact identity to PageBroker;
it adds the resolved store through the existing `StorageBackend` selector, so
PageBroker no longer depends on one store fixed at startup. PVC reuses
`FilesystemStorage`; S3 is the one new kind. `--storage-config` remains for
installations without classes.

```protobuf
message StorageBackend {
  oneof kind {
    FilesystemStorage filesystem = 1;  // Existing; PVC: mount path joined with basePath.
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

message ArtifactTarget {
  StorageBackend storage = 3;   // Resolved class, trusted like today's startup config.
}

message PublishedArtifact {
  StorageBackend storage = 3;   // Same for restore and metadata reads.
}
```

PageBroker trusts `store_id` and `StorageBackend` as it trusts its startup
configuration today; the binding check is the agent's. It caches S3 clients by
`store_id` and credentials-file digest.

The agent resolves the class from its informer when bound content arrives; a
missing class or Secret fails the operation with `StorageClassUnavailable`,
and nothing is retried against a different store.

For PVC the agent maps `claimName` to a mount path: the default claim stays at
`/checkpoints`, each extra claim is mounted at `/snapshot-stores/<claimName>`
in the agent, PageBroker and operator, and the chart renders the list into the
agent's `config.yaml` and the operator's `--mounted-claims` flag.

#### Credential delivery

S3 credentials are not mounted: a Secret volume is fixed when the pod starts,
which is why today's single store needs a restart. The agent carries them per
transaction through the staging volume it already shares with PageBroker:

1. At Prepare, the agent reads the class's Secret, and CA bundle ConfigMap if
   set, with an uncached `Get` and writes
   `<staging>/stores/<transaction-id>/credentials` and `ca.crt`, mode `0600`.
2. It passes the paths in `S3Storage`; PageBroker builds its S3 client from
   that file in the same shared-credentials format it reads today.
3. It removes the directory on Commit or Abort. On restart it removes
   directories whose transaction is gone; the volume is RAM, so a pod crash
   leaves nothing on the node.

The directory is per transaction, not per store: concurrent transactions never
share a file, lifetime needs no reference counting, and a rotated Secret is
picked up by the next transaction while an in-flight one keeps its key.
Sharing happens in PageBroker's client cache, keyed by `store_id` and digest.

#### Maintenance

The backend registry is keyed by `storeID` and built from Ready and
terminating classes by the class informer. `backendForContent` resolves the
content's class by name, checks its `storeID` against the binding, and returns
that backend. Under `reclaimPolicy: Retain`, `delete-content` removes the
finalizer without touching the store and emits `SnapshotsRetained` with the
handle. Sweeps run per class and never cross stores. Before it deletes
anything, a sweep reads the marker at its own location and stops, reporting
the reason, if the marker is missing or names another store. It then lists its
prefix once, collects the `.snapshot-store` markers in it, and excludes every subtree
whose marker names a different `storeID` before it deletes anything; on a PVC
it does the same while walking directories. Deleting content removes only the
handles recorded on it. Content whose class is gone keeps its finalizer and
gets `StoreInactive`, as in SNEP-237.

### Security

- **Who may create a class.** Creating one needs cluster-level RBAC, like
  `StorageClass`; naming one needs only write access to `PodSnapshot` or
  `SnapshotJob`. A class says any `PodSnapshot` may use that store.
- **Credentials.** Secrets live in the Snapshot namespace, replacing today's
  S3 file mounts. RBAC is the boundary: a namespaced Role, `get`/`list`/`watch`
  for the operator, `get` only for the agent. Neither caches Secret contents:
  the operator watches Secret metadata and reads one when validating a class;
  the agent reads one at Prepare, writes it to the staging tmpfs, never to the
  node filesystem or logs, and drops it with the transaction.
- **Isolation between stores.** Every operation carries class and digest; the
  agent and maintenance refuse a class whose `status.storeID` does not match
  the binding. Cleanup lists and deletes only under its own class's prefix,
  and never inside a subtree marked by a different store.
- **PageBroker.** No Kubernetes client, RBAC or long-lived credentials; it
  trusts the agent on the local socket as today.

### Configuration

The chart keeps `storage.*` and renders it as the `default` class. New values:

```yaml
storage:
  type: pvc
  pvc:
    name: snapshot-pvc
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

1. The same `storage.*` values that configured the single store now render the
   `default` class, so its `storeID` is byte-identical to the one already
   recorded on every bound `PodSnapshotContent`.
2. The operator, on start, sets `spec.storage.storageClassName` on content that
   has a `storeID` but no class name by matching the digest against Ready
   classes; content matching no class keeps the `StoreInactive` handling.
3. Content with no `spec.storage` at all, bound before SNEP-237, keeps the
   legacy path-addressed flow and is never assigned a class.
4. `PodSnapshot`s and `SnapshotJob`s without `storageClassName` resolve to
   `default`, so manifests written for the old release behave as before.

Rollback to the previous chart leaves the CRD and `default` object in place;
the old operator ignores both and the unknown field, and reads `storage.yaml`.

### Monitoring

- `SnapshotStorageClass` status: `storeID` and a `Ready` condition with the
  reasons above.
- `SnapshotStorageClass` `DefaultConflict` condition on a class marked
  `isDefault` while an older class is also marked.
- `PodSnapshot` `Ready=False` reasons: `StorageClassNotFound`, `NotReady` and
  `Terminating`.
- Events: `SnapshotsRetained` under `Retain`; `StorageClassTerminating` on
  the class while references remain, with count; a Warning on a terminating
  class that is the default, once per transition.
- Metrics: `snapshot_storage_class_ready{class}`,
  `snapshot_storage_class_bound_contents{class}`; maintenance metrics gain it.

A refused class names the other in its condition and in a Warning event
emitted once per transition; only the later class is marked:

```yaml
status:
  storeID: store-v1-4f2a…
  conditions:
    - type: Ready
      status: "False"
      reason: StoreOverlap
      message: 'prefix "some/path/child" in bucket "team-a-snapshots" is inside prefix "some/path" of SnapshotStorageClass "team-a"'
```

### Dependencies

- SNEP-237 artifact addressing: bound content, `storeID`, `ArtifactTarget` and
  `PublishedArtifact`; images that accept a per-request `StorageBackend`.
- Kubernetes 1.30 or newer, only for the optional default-class admission
  policy. The repository declares no minimum Kubernetes version today: neither
  the README prerequisites nor the chart's `kubeVersion` sets one, and the
  end-to-end suite runs on 1.32. The policy is skipped on an API server
  without `ValidatingAdmissionPolicy`, and the operator rule still applies.

### Test Plan

- **API:** CEL rejects two backend blocks, none, and a location change, and
  accepts credential and TLS changes; the binding carries name and digest.
- **Operator:** Ready/NotReady for each reason; digest, overlap and marker
  detection, including two classes created at once, refused by creation order
  whatever order they reconcile in, a stale marker, an ancestor marker from
  another store, and an ancestor read the credentials deny;
  default resolution, including two marked classes resolving to the oldest and
  setting `DefaultConflict` on the other, and none marked; terminating class
  refuses new bindings and
  resolves old ones; finalizer removed at zero references; `Retain` skips
  deletion and emits the event; registry rebuilt on class add and remove.
- **Admission policy:** a second `isDefault: true` is rejected on create and
  update, deleting the default is rejected while deleting a non-default is
  accepted, clearing then setting is accepted, and the chart skips the policy
  when the API server does not serve `ValidatingAdmissionPolicy`. Without the
  policy, deleting the default emits the Warning event.
- **Agent:** Secret fetch, file permissions and removal on Commit, Abort and
  crash; `StorageClassUnavailable` on a missing class or a `status.storeID`
  that does not match the binding; mount mapping for default and extra claims,
  and `StorageClassUnavailable` for a claim the agent has no mount for.
- **Maintenance:** a sweep stops without deleting when its own marker is
  missing or names another store, and leaves a subtree whose marker names
  another store untouched, on S3 and on a PVC; deleting content removes only
  its own handles.
- **PageBroker:** per-request `StorageBackend`, client cache, two stores live.
- **End to end:** two S3 and one PVC class in one cluster, checkpoint and
  cross-node restore through each; a class with bound content terminates only
  after the content is gone; old content stays put when the default moves;
  rolling upgrade with an old agent fails closed.

### Graduation Criteria

- **Alpha:** CRD and fields as above; PVC and S3 classes; default class from
  the chart; protection finalizer; `Retain`; PVC mount limitation documented.
- **Beta:** a cluster running two or more classes in production; metrics and
  events stable; upgrade and rollback from the single-store chart tested.
- **GA:** API stable for one release; the deprecated operator flags removed.

## Alternatives

- **Pod annotation instead of a spec field.** Hides the choice from the
  `PodSnapshot` a user reads and from `SnapshotJob` validation. Rejected.
- **PageBroker with its own Kubernetes client.** Keeps credentials out of the
  agent, but adds an API dependency, RBAC and a watch to a component driven
  over a local socket. Rejected for this iteration.
- **Namespaced classes.** A store is a cluster-wide location served by a
  cluster-wide agent; namespacing only moves the authorization question.
- **Mounting arbitrary PVCs at runtime.** Needs the agent to mount claims or
  a restart per claim; deferred and recorded as the limitation.
- **Helm list of stores instead of a CRD.** Every change becomes a Helm
  upgrade and a restart, which is the problem being solved. Rejected.
