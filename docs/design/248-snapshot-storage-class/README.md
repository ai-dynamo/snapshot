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
added and removed while the cluster runs, and several clusters can share one
store. On first start the operator creates a
default class from today's `storage.*` values, so existing installations keep
working.

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
  classes in one cluster that describe the same location.
- Let several clusters keep their snapshots in one store.
- Protect a store while content references it, and let an administrator decide
  whether deleting content also deletes its snapshots.

### Non-Goals

- Provisioning storage. A class points at a PVC or bucket that already exists.
- Mounting a PVC the chart did not mount. In this iteration a PVC class must
  name a claim already mounted on the operator and every agent.
- Moving snapshots between stores.
- Restoring in one cluster a snapshot taken in another. A shared store only
  keeps the clusters' snapshots side by side; `.origin` and `Retain` leave room
  for it.
- Several layers for one content, with reads falling back from the fastest to
  the slowest. The class stays the one authoritative store; the binding and the
  PageBroker protocol leave room for layers.
- Namespaced classes, per-namespace quotas, or RBAC on who may name a class.
- Changing SNEP-237's `storeID` encoding, publications or artifact layout,
  apart from two small files beside a content's snapshots.

## Proposal

A `SnapshotStorageClass` is to a snapshot store what `StorageClass` plus
`PersistentVolume` are to a disk: it names a prepared location, carries the
settings to reach it, and protects it while something still depends on it.

**Registering a store.** The administrator creates the PVC or bucket, puts any
credentials in a Secret in the Snapshot namespace, and creates a
`SnapshotStorageClass`. The operator computes the `storeID`, refuses a location
another class in the cluster already holds, and sets `Ready` once it can write, read, list
and delete a test object at the location; that is the access check. A class that is not Ready re-validates when its
Secret or ConfigMap changes, and on a backoff.

**Sharing a store between clusters.** Classes in different clusters may point
at the same location; nothing has to be set. The clusters' snapshots sit side
by side, each under its own content UID. Every content's directory records the
cluster that created it, in a file named `.origin`, and a cluster deletes and
sweeps only directories that name it.

**Selecting a store.** A `PodSnapshot`, or the `SnapshotJob` template, names a
class in `spec.storageClassName`, or omits it for the default. The operator
resolves it once and binds the content to that store for its whole life.

**Retiring a store.** A protection finalizer holds the class while content
references it. A terminating class takes no new snapshots but still restores
and cleans up. `reclaimPolicy` decides whether deleting content also deletes
its snapshots: `Delete` removes them, `Retain` keeps them and marks them in the
store so cleanup never removes them. The policy is copied onto the content at
binding and does not change afterwards.

**The default.** The operator's configuration names the default class:
`storage.defaultClassName` in the chart, `default` unless changed. When the
cluster has no class, the operator creates that class from the chart's
`storage.*` values, so an upgraded installation needs no manual step. It never
overwrites a class. The default moves by changing the value; bound content
keeps its binding. See [Default class](#default-class).

### User Stories

- A team has a nearby high-bandwidth S3-compatible store. The administrator
  creates a `fast-s3` class; the team's `SnapshotJob` names it; others default.
- Two clusters keep their snapshots in one bucket. Each administrator creates a
  class that points at it.

### Limitations, Risks, and Mitigations

| Risk | What goes wrong | How the design handles it |
| --- | --- | --- |
| PVC not mounted | A PVC class names a claim the agents cannot see; captures would fail on every node. | The operator marks the class `Ready=False`, reason `ClaimNotMounted`, by checking the claim against the mounted set the chart renders. Adding a claim is a Helm upgrade (`storage.pvc.claimNames`). Documented limitation for this iteration. |
| Agent without the claim | The class is `Ready`, because the operator reads the chart's mounted set, but a particular agent does not have the claim: it still runs the previous pod spec during a rolling upgrade, or the claim is missing on its node. | The agent maps `claimName` to a mount path from its own `config.yaml` and checks the path exists before any I/O. A claim it cannot map fails the operation with `StorageClassUnavailable`, and nothing is retried against another store. The existing retry succeeds once the node's agent has the mount. |
| Class edited after binding | The location fields change; content bound to the old `storeID` would read or delete in the wrong place. Or `reclaimPolicy` flips and existing content loses data it was meant to keep. | Location fields are immutable through CEL, and the policy is frozen on the content at binding. Credentials and TLS settings stay editable because they are not identity inputs. The agent and maintenance compare the class's `status.storeID` with the content's binding before any I/O and refuse a mismatch. |
| Class deleted under content | Content loses the only description of where its snapshots are. | Protection finalizer; a terminating class still resolves for existing content. Force-removing the finalizer leaves content with the existing `StoreInactive` handling from SNEP-237. |
| Secret missing or revoked | An S3 class's Secret is deleted or rotated to an invalid key. | The class goes `Ready=False`, reason `CredentialsUnavailable`, and new bindings stop. In-flight operations fail with the existing credential errors; nothing is deleted on an access error. |
| Agent holds credentials | Today only PageBroker and the operator see S3 credentials. The agent now reads the class's Secret and passes it to PageBroker in the request. | The Secret must live in the Snapshot namespace. The operator keeps the agent's Role limited by `resourceNames` to the Secrets and ConfigMaps that classes reference, and removes a rule whose list would be empty, because an empty list allows every object; a node can still read the credentials of every store, where today it holds one. The credentials stay in memory in both processes and cross only the local socket the agent already uses; they are never written to a volume, a transaction record or a log. Accepted trade-off against giving PageBroker a Kubernetes client; see Alternatives. |
| Shared workload identity | Classes without a Secret are all reached with one identity, that of the agent's and the operator's service accounts. | That identity needs access to every such store, so credentials do not separate those stores from each other; use a Secret per class where separation matters. Instance metadata is excluded, so a broken identity fails instead of falling back to the node's role. |
| Duplicate location | Two classes in one cluster describe the same place, so one store gets two policies and two sets of access settings. | The operator refuses `Ready` for the later class with reason `DuplicateStore` and names the other. Two classes that reach one bucket through different hostnames are not detected; that is harmless, because a sweep keeps every directory whose content exists in the cluster, whichever class it is bound to. See [Store uniqueness](#store-uniqueness). |
| Shared store | Several clusters use one location. A sweep or a delete in one cluster could remove another's snapshots. | Each content directory carries `.origin` with the creating cluster's ID. A sweep deletes only orphans whose `.origin` names its own cluster, and deleting content skips a directory that names another. Each cluster's credentials can still read and delete the others' snapshots, so share only between clusters that trust each other. A destroyed cluster's directories stay until an administrator removes them. |
| Older release on the same location | A cluster on a release without classes, or one rolled back to it, writes no `.origin` and sweeps every directory that has no content in that cluster. | Not preventable from the store. Do not share a location with such a cluster, and before a rollback make sure no other cluster uses the location. |
| Rollback with retained data | The previous release ignores `reclaimPolicy` and `.retained`. After a rollback its sweep deletes retained directories in the store that `storage.*` names, and deleting a `Retain` content deletes its data. | Not preventable from the new release. The Rollback procedure tells the administrator to copy retained data out of that store first. Stores of other classes are not reached by the previous operator. |
| Nested stores | One store's location is a parent of another's, in this cluster or another. | Allowed, because each store keeps its snapshots under its own `artifacts/`: a sweep looks only there and deletes only orphans named by a UUID, and deleting content removes only its own recorded handles. The API rejects a location inside another store's `artifacts/`. |
| Old agent, new class | During a rolling upgrade an agent that predates this proposal receives content bound to a non-default class. | It passes its startup store to PageBroker, whose `storeID` does not match the content's; PageBroker returns `STORE_MISMATCH` and the capture fails before any I/O. The existing retry picks it up after the node upgrades. |
| Restore from several S3 stores | The native restore engine holds one session per PageBroker process, fixed to one endpoint, one region and one set of credentials, and reads its CA bundle and addressing mode once at start. | Restores whose stores share the endpoint, region and credentials run in parallel; a restore that differs in any of them waits until the others drain, then PageBroker replaces the session. A long restore delays the others, and a waiting transaction can expire. An S3 class whose CA bundle or addressing mode differs from PageBroker's startup values cannot restore. Parallel restore across sessions is a follow-up; see [Agent and PageBroker](#agent-and-pagebroker). |
| Default misconfigured | `storage.defaultClassName` names a class that does not exist, is not Ready or is being deleted, and unnamed `PodSnapshot`s fail. | The default is one configured name, so two defaults cannot exist. An unnamed `PodSnapshot` fails with the same reasons as a named one and retries until the class is usable. The operator reports a missing default at start, and a Warning event flags a terminating one; see [Default class](#default-class). |

## Design Details

### API

#### SnapshotStorageClass

A new cluster-scoped kind in `snapshot.nvidia.com/v1alpha1`. Exactly one
store block is set, `pvc` or `s3`, and the block that is set names the kind of
store. Its location fields are the `storeID` inputs defined in
`../../../api/storage`; they are immutable through CEL transition rules, and a
class cannot move from one block to the other. `reclaimPolicy`,
credentials and TLS settings stay editable; an edit to `reclaimPolicy` applies
to content bound afterwards.
A class name is at most 63 characters, because it is used as a label value.

```go
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters"
type SnapshotStorageClass struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   SnapshotStorageClassSpec   `json:"spec"`
    Status SnapshotStorageClassStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.pvc) != has(self.s3)",message="exactly one of pvc or s3 must be set"
// +kubebuilder:validation:XValidation:rule="has(self.pvc) == has(oldSelf.pvc)",message="the store block cannot change between pvc and s3"
type SnapshotStorageClassSpec struct {
    // +kubebuilder:validation:Enum=Delete;Retain
    // +kubebuilder:default=Delete
    ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
    PVC *PVCStoreSpec `json:"pvc,omitempty"`
    S3  *S3StoreSpec  `json:"s3,omitempty"`
}

// A claim in the Snapshot namespace that the chart already mounts.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pvc is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.basePath) || !self.basePath.split('/').exists(p, p == 'artifacts')",message="basePath must not contain an artifacts segment"
type PVCStoreSpec struct {
    ClaimName string `json:"claimName"`
    BasePath  string `json:"basePath,omitempty"` // Default "/", the whole claim.
}

// Endpoint, Region, Bucket and Prefix are storeID inputs; the rest are access
// settings and stay editable.
// +kubebuilder:validation:XValidation:rule="self.endpoint == oldSelf.endpoint && self.region == oldSelf.region && self.bucket == oldSelf.bucket && self.?prefix == oldSelf.?prefix",message="endpoint, region, bucket and prefix are immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !self.prefix.split('/').exists(p, p == 'artifacts')",message="prefix must not contain an artifacts segment"
type S3StoreSpec struct {
    Endpoint string `json:"endpoint"`
    Region   string `json:"region"`
    Bucket   string `json:"bucket"`
    Prefix   string `json:"prefix,omitempty"`
    // A Secret in the Snapshot namespace holding a shared-credentials file.
    // The key defaults to "credentials". Omitted: workload identity is used.
    CredentialsSecretRef *corev1.SecretKeySelector `json:"credentialsSecretRef,omitempty"`
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
`DuplicateStore`, `StoreUnreachable` and `Terminating`.

#### Request and content fields

`PodSnapshot` (`spec`) and the `SnapshotJob` template
(`spec.podSnapshotTemplate`) gain an optional `storageClassName`; empty selects
the default class. `PodSnapshotContent.spec.storage` gains the class
name beside the digest, which stays the authority on *where*; the name says
*how to reach it*. It also gains `reclaimPolicy`, copied from the class at
binding and immutable, so editing the class later never changes what happens to
existing content. The class always has a policy (it defaults to `Delete`), so
every binding sets one. Content bound before classes existed has neither field
and is never rewritten, because the content `spec` is immutable. Its class is
found by matching its `storeID` against the classes, and its empty policy means
`Delete`, as today.

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
   the source Pod, so a long run is not spent on a snapshot that cannot be
   bound. A missing or terminating class sets `InvalidSpec`, which is terminal.
   A not-Ready class is a wait: the job reports `StorageClassNotReady`, launches
   no pod, and continues when the class becomes Ready.
2. The `PodSnapshot` reconciler reads `spec.storageClassName`, or the default
   class name from the operator's configuration when it is empty.
3. At binding it requires `Ready=True` and no deletion timestamp, else it sets
   `Ready=False` on the `PodSnapshot` with `StorageClassNotFound`,
   `StorageClassNotReady` or `StorageClassTerminating` and requeues; a watch on
   `SnapshotStorageClass` picks up later changes. This check is the authority:
   the job check is early, this one is where the `storeID` is frozen.
4. It creates the `PodSnapshotContent` with `spec.storage.storeID`,
   `storageClassName` and `reclaimPolicy`, labeled
   `nvidia.com/snapshot-storage-class=<name>`. It then re-reads the class from
   the API server; if the class is terminating or gone, it deletes the new
   content and fails the snapshot with `StorageClassTerminating`.
5. The class reconciler's `nvidia.com/snapshotstorageclass-protection`
   finalizer is removed only when the class is terminating and no content
   names it in `spec.storage.storageClassName` or, with no class name, carries
   its `storeID`. The check is a live list on the spec fields; the label is
   editable and only serves queries.

#### Default class

The default class is named in the operator's configuration, not on the class:
the chart value `storage.defaultClassName`, passed to the operator as
`--default-storage-class`, `default` unless changed. A request with no
`storageClassName` resolves to that name and then follows the same rules as a
request that names a class. One configured name means there is never more than
one default, so the class carries no default field and nothing has to arbitrate
between classes.

- **Moving the default.** Create the new class, change the value and run
  `helm upgrade`. Only the operator restarts: the agents never read the
  default, so their configuration checksum leaves the value out. Bound content
  keeps its binding.
- **Default missing.** If the named class does not exist, the operator reports
  it at start, and unnamed `PodSnapshot`s fail with `StorageClassNotFound` until
  it does.
- **Default deleted.** A Warning event names the effect: unnamed `PodSnapshot`s
  fail with `StorageClassTerminating` while content references the class, and
  `StorageClassNotFound` after. The operator creates the default class only
  when the cluster has no class at all, so a deleted default is not brought
  back while another class exists.

#### Store uniqueness

Inside a cluster, one location has exactly one class, and no store sits inside
another store's snapshots. Between clusters a location may be shared.

- **Reserved path.** The API rejects a `prefix` or `basePath` with a path
  segment named `artifacts`, the directory every store keeps its snapshots in,
  so no class can be placed inside another store's `artifacts/`, in this
  cluster or another.
- **Digest.** Another class in the cluster with the same `storeID` is
  `DuplicateStore`. The scan reads the class informer cache and compares only
  immutable location fields, so a result changes only when a class is added or
  deleted. The later class by `creationTimestamp`, ties by name, is refused, so
  two classes created at once get the same answer in any reconcile order.
- **Access.** Before `Ready`, the operator writes a test object at the
  location, reads it back, finds it in a listing and deletes it. Those are the
  four permissions a snapshot needs over its life: capture, restore, cleanup
  and deletion. The object sits outside `artifacts/` and is named for the
  cluster, so two clusters never collide. A failure is `StoreUnreachable`, or
  `CredentialsUnavailable` when the credentials are the cause, and the
  condition message names the step that failed. The check runs from the
  operator, so it does not prove that every node can reach the store.

Nothing in the store marks a location as taken, so classes in several clusters
can point at one location. What keeps them apart is recorded per content:

- **`.origin`.** When the operator binds a content it writes
  `artifacts/<uid>/.origin`, a JSON file with a `version`, the cluster ID (the
  UID of the `kube-system` namespace, fixed for the life of the cluster), the
  time, and the namespace and name of the `PodSnapshot`. It is written once and
  never changed. PageBroker publishes below it, under `containers/`, so the two
  do not conflict. The operator also writes it for content bound before this
  proposal, so every directory the cluster owns is marked.
- **`.retained`.** Written beside it when content is deleted under `Retain`;
  see [Maintenance](#maintenance).

A missing or unreadable `.origin`, or one with an unknown `version`, means the
directory is not this cluster's. Stores may nest outside `artifacts/`:
`team-a/` and `team-a/prod/` are two stores. Each keeps its snapshots under its
own `artifacts/` and a sweep looks only there, so neither can reach the other's
data.

#### Agent and PageBroker

The agent already carries `storeID` and the artifact identity to PageBroker; it
adds the resolved store through the `StorageBackend` selector, so PageBroker no
longer depends on one store fixed at startup. PVC reuses `FilesystemStorage`;
S3 is the one new kind. PageBroker's `--storage-config` no longer selects a
store. Only new fields are shown.

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
  bytes credentials = 5;  // The Secret's shared-credentials content. Empty: workload identity.
  string addressing = 6;
  bool allow_http = 7;
  bytes ca_bundle = 8;    // PEM, from the class's ConfigMap. Empty: system roots.
}

message ArtifactTarget    { StorageBackend storage = 3; }  // Resolved class.
message PublishedArtifact { StorageBackend storage = 3; }  // Restore and metadata reads.
```

PageBroker trusts `store_id` and `StorageBackend` as it trusts its startup
configuration today; the binding check is the agent's. It caches S3 clients by
`store_id`, `addressing`, `allow_http` and a SHA-256 digest of the credentials
and CA bundle bytes in the request, computed when it builds the client, so rotated
credentials or a changed setting get a new client. An entry idle for 15 minutes
is dropped, the cache holds at most 64 entries, and a client with calls in
flight is never dropped. A missing class or Secret fails the operation with
`StorageClassUnavailable`, and nothing is retried against another store. For PVC the agent maps `claimName` to a mount path: the default
claim at `/checkpoints`, each listed claim at `/checkpoints/<claimName>`,
rendered by the chart into the agent's `config.yaml` and the operator's
`--pvc-claim-names` flag. Every store path is below `/checkpoints`, the one
source root the privileged mount helper accepts, so a restore that mounts a
snapshot directly from a listed claim needs no change to the helper.

Payload restores do not use that client. They go through the native restore
engine, which holds one session per PageBroker process and reads its CA bundle
and addressing mode once at start. A session is fixed to one endpoint, one
region and one set of credentials for its whole life. Restores whose stores
share all three run in parallel in one session, as today; that includes stores
in the same region that use the same workload identity. When the next restore
differs in any of the three, or the credentials have changed, it waits until
the running restores drain; PageBroker then ends the session and starts one
with the new settings. This reuses the path that replaces a failed session
today. Until restores run in parallel across sessions, a long restore delays
the others and a waiting transaction can expire, and an S3 class whose CA
bundle or addressing mode differs from PageBroker's startup values, taken from
`storage.s3`, cannot restore.

#### Credential delivery

S3 credentials are not mounted: a Secret volume is fixed when the pod starts,
which is why today's single store needs a restart. They are also not written to
the volume the agent shares with PageBroker; this design adds no file exchange
between the two. The agent passes them in the request:

1. At Prepare, the agent reads the class's Secret, and the CA bundle ConfigMap
   if set, with an uncached `Get`.
2. It puts their contents in `S3Storage.credentials` and `S3Storage.ca_bundle`
   on every call that carries a `StorageBackend`. The credentials are in the
   shared-credentials format PageBroker parses today.
3. PageBroker keeps them in memory for the transaction and hands them to its
   S3 tooling as environment or arguments, as it does today. It does not store
   them in a transaction record, so a call after a PageBroker restart carries
   them again. The CA bundle is not secret; where a tool needs a path,
   PageBroker writes it to its own scratch space.

A class with no `credentialsSecretRef` uses workload identity. The agent leaves
`credentials` empty; PageBroker and the operator then use the SDK default
credential chain under their own service accounts, with instance metadata
excluded so the node's role is never used. Those credentials refresh on their
own. The rule is the same on every platform. Where the default chain cannot
perform a platform's token exchange, PageBroker and the operator need a
credential provider for it behind the same rule; that is a follow-up, with no
change to the class.

Credentials are read per transaction, not per store: a rotated Secret reaches
the next transaction while an in-flight one keeps its key. For a restore
payload the new key takes effect when the restore session is replaced; see
[Agent and PageBroker](#agent-and-pagebroker). A retried checkpoint or restore
is a new transaction, so it reads the Secret again. Sharing happens in
PageBroker's client cache.

#### Maintenance

The backend registry is built from Ready and terminating classes.
`backendForContent` resolves the content's class by name, or by `storeID` for
content with no class name, checks its `storeID` against the binding, and
returns that backend. Under the content's `reclaimPolicy: Retain`,
`delete-content` writes `artifacts/<uid>/.retained`, a JSON file with the
cluster ID, the class name, the `storeID` and the time, then removes the
finalizer and emits `SnapshotsRetained` with the handle; it deletes nothing.
The file stands on its own, so nothing else has to be read to know the
directory is kept and where it came from. Under `Delete` it removes only the
handles recorded on the content, and skips, with a Warning event, a directory
whose `.origin` names another cluster.

Sweeps run per class and never cross stores. A sweep looks only inside
`<location>/artifacts/` and deletes a directory only when all of these hold:
its name is a UUID, the `PodSnapshotContent` UID that PageBroker names it by; no
content in the cluster has that UID; it carries no `.retained`; and its
`.origin` names this cluster. Only the content check exists today, when every
directory there with a safe name and no matching content is an orphan. So
leftovers from before the upgrade, which have no `.origin`, are never swept and
need a one-time manual cleanup. Content whose class is gone keeps its finalizer
and gets `StoreInactive`, as in SNEP-237.

### Security

- **Who may create a class.** Cluster-level RBAC, like `StorageClass`; naming
  one needs only write access to `PodSnapshot` or `SnapshotJob`. A class says
  any `PodSnapshot` may use that store.
- **Credentials.** Secrets live in the Snapshot namespace, replacing today's S3
  file mounts, under a namespaced Role: `get`/`list`/`watch` for the operator,
  `get` for the agent, and no caching of contents. The operator maintains the
  agent's Role so its `resourceNames` list only the Secrets and ConfigMaps that
  classes reference; every node can still read every store's credentials. An
  empty `resourceNames` list allows every object of the kind, so when no class
  references a Secret the operator removes the Secret rule from the Role, and
  likewise the ConfigMap rule. The
  agent's ClusterRole gains `get`/`list`/`watch` on `snapshotstorageclasses`.
  A class without a Secret uses the service accounts' workload identity.
  The agent holds a Secret only in memory and sends it only over the local
  socket; neither process writes it to a volume or a log.
- **PageBroker.** No Kubernetes client, RBAC or long-lived credentials; it
  trusts the agent on the local socket as today.
- **Shared stores.** Every cluster that shares a store holds credentials that
  can read and delete the others' snapshots; `.origin` guides cleanup and is not
  access control. Sharing is for clusters that trust each other.

### Configuration

The chart keeps `storage.*` but ships no class object: Helm applies a release
before the operator's `crd-installer` has created the new CRD, so the operator
creates the default class instead. The installer's RBAC gains the new CRD.
New values:

```yaml
storage:
  # The class used by requests that name none. The operator creates it from
  # the values below when the cluster has no class.
  defaultClassName: default
  pvc:
    # Claims that PVC classes may use, in the release namespace.
    claimNames: []
```

A claim name is all `claimNames` needs, because the chart derives the
rest. For each name it adds a volume and a mount at
`/checkpoints/<claimName>` to the operator and to the agent pod's agent and
PageBroker containers, as it already does for the default claim at
`/checkpoints`; Kubernetes mounts them when those pods restart. The name is
unique in the namespace and a valid path segment, so no two claims share a
path and nobody has to choose one. The listed claims are mounted inside the
default claim's mount, so Kubernetes creates an empty directory per name at
the root of the default claim. The chart refuses the name `artifacts`, which
would hide the default store's snapshots, and a name must not match a
directory that already exists at that root. `storage.pvc.name`, the single
claim at `/checkpoints`, is deprecated: `claimNames` replaces it, and once it
is removed `/checkpoints` holds only the listed claims' mounts. The chart
writes the same name-to-path
mapping into the agent's `config.yaml` and the names into the operator's
`--pvc-claim-names` flag, so a class's `claimName` always resolves to the
directory that was mounted. The claims must already exist in the release
namespace and be mountable on every agent node. The list is the whole set, so
an upgrade repeats the claims it keeps:

```yaml
storage:
  pvc:
    claimNames:
      - team-a-snapshots   # /checkpoints/team-a-snapshots
      - team-b-snapshots   # /checkpoints/team-b-snapshots
---
apiVersion: snapshot.nvidia.com/v1alpha1
kind: SnapshotStorageClass
metadata:
  name: team-b
spec:
  pvc:
    claimName: team-b-snapshots   # Which mounted claim.
    basePath: /prod               # A directory inside it, not the mount path.
```

Classes require `operator.bindNewContents` and `pageBroker.artifactAddressing`;
the chart fails the render otherwise.

Every other `storage.*` value keeps its meaning and feeds the default class: `storage.type`
selects the `pvc` or `s3` block; `s3.endpoint`, `region`, `bucket`, `prefix`, `addressing`,
`allowHttp`, `credentialsSecretRef`, `credentialsSecretKey`,
`caBundleConfigMapRef` and `caBundleConfigMapKey` become the class's `s3` fields;
`pvc.name` becomes `claimName`, with the class `basePath` at the claim root.
`pvc.basePath` stays the container mount path, and `pvc.create`, `size` and
`storageClass` still only provision the claim. An empty
`s3.credentialsSecretRef` gives a default class with no Secret. What changes
is delivery: the
chart no longer mounts the S3 Secret, which is read through the API and passed
per transaction. The CA ConfigMap stays mounted in PageBroker, because the
native restore engine reads its CA bundle and addressing mode once at start.

The chart renders these values into the operator's storage configuration file,
read through `--snapshot-storage-config`, and passes `storage.defaultClassName`
as `--default-storage-class`. The operator uses the file for one thing: when
the cluster has no `SnapshotStorageClass` at all, it creates the default class
under that name. That covers a fresh install and the first upgrade. If any
class exists it does nothing, so it never recreates a deleted class or reverts
an edit. Later changes to the store values do not change an existing class; to
switch stores, create a new class and point `storage.defaultClassName` at it.

Classes are the only way to define a store. The operator builds its backend
registry from classes only, so this release removes its S3 credential flags; an
operator started with one fails at start with an unknown-flag error, so remove
it from any hand-edited arguments. `--snapshot-storage-config` stays, only as
the seed for the default class. PageBroker's `--storage-config` no longer
selects a store; it carries the process-wide restore settings.

#### Upgrade from a single-store installation

An installation upgraded from a release without classes keeps working after a
`helm upgrade` run with the previous values:

1. The operator applies the new CRD, finds no class, and creates the default
   class, named `default`, from the same `storage.*` values, so its `storeID`
   is byte-identical to the one recorded on every bound `PodSnapshotContent`.
2. Existing content is not rewritten. Content that has a `storeID` but no class
   name is matched to a class by digest whenever it is used, and it protects
   that class from deletion; content matching none keeps the `StoreInactive`
   handling.
3. Content with no `spec.storage`, bound before SNEP-237, keeps the legacy
   path-addressed flow and is never assigned a class.
4. `PodSnapshot`s and `SnapshotJob`s without `storageClassName` resolve to
   the default class.

Rollback to the previous chart leaves the CRD and `default` object in place;
the old operator ignores both and reads `storage.yaml`.

The previous release does not know `reclaimPolicy` or the `.retained` file, and
its sweep cannot be turned off. In the store that `storage.*` names, a rollback
therefore loses retained data in two ways: the first sweep deletes every
directory that has no content in the cluster, which includes all data already
retained, and deleting a content created with `Retain` deletes its data. Before
a rollback, copy the retained data out of that store, and do not delete
`Retain` content until the cluster is upgraded again. The previous operator
reads only that one store, so the stores of other classes are not touched.

### Monitoring

- `SnapshotStorageClass` status: `storeID` and `Ready` with the reasons above.
  A refused class names the other in its condition and in a
  Warning event once per transition; only the later class is marked.
- Events: `SnapshotsRetained` under `Retain`; `StorageClassTerminating` on the
  class while references remain, with count; a Warning on a terminating default.
- Metrics: `snapshot_storage_class_ready{class}`,
  `snapshot_storage_class_bound_contents{class}`; maintenance metrics gain it.

### Dependencies

- SNEP-237 artifact addressing: bound content, `storeID`, `ArtifactTarget` and
  `PublishedArtifact`; images that accept a per-request `StorageBackend`.
- Workload identity support in PageBroker and the operator's cleanup, for
  classes without a Secret: the default credential chain, tracked in issue
  988, and refreshable credentials from a projected service-account token,
  tracked in issue 987.

### Test Plan

- **API:** CEL rejects two store blocks, none, a move between blocks, a location change, a name over
  63 characters, or an `artifacts` path segment.
- **Operator:** Ready/NotReady for each reason; digest detection, including
  two classes created at once; the access check, including credentials that
  can write but not read, list or delete; nested stores are accepted;
  `.origin` is written at binding and for content bound earlier; default
  resolution from the configured name, with the named
  class missing, and after the name changes; the default class is created only
  when no class exists; terminating class, finalizer, `Retain`, registry rebuild;
  the agent's Role lists only referenced Secrets and ConfigMaps, and has no
  Secret rule when no class references one.
- **Chart:** changing only `storage.defaultClassName` changes the operator's
  pod template and leaves the agent DaemonSet's unchanged.
- **Agent:** Secret fetch per transaction; credentials in the request and never
  on a volume or in a log; empty credentials for a class without a Secret; mount mapping for the default and listed claims, and a direct restore from a listed claim; `StorageClassUnavailable`
  on a missing class, a `status.storeID` mismatch, or a claim with no mount.
- **Maintenance:** a sweep looks only inside `artifacts/` and deletes only
  UUID-named orphans whose `.origin` names this cluster, on S3 and on a PVC.
  Directories with another cluster's `.origin`, with none, or with `.retained`
  survive. Deleting content skips a directory that names another cluster. A
  nested store's data survives its parent's sweep.
- **PageBroker:** per-request `StorageBackend`, client cache, two stores live;
  credentials from the request are not written to disk or a transaction record;
  restores from stores that differ in endpoint, region or credentials queue and
  run in turn; stores that share them restore in parallel; with a class that
  has no Secret, an upload and a restore each run past one token lifetime.
- **End to end:** two S3 and one PVC class in one cluster, checkpoint and
  cross-node restore through each; a class terminates only after its content is
  gone; old content stays put when the default moves; rolling upgrade with an
  old agent fails closed.

### Graduation Criteria

- **Alpha:** CRD and fields as above; PVC and S3 classes; default class from
  the chart's values; protection finalizer; `Retain`; shared stores; PVC mount limitation documented.

## Alternatives

- **PageBroker with its own Kubernetes client.** Keeps credentials out of the
  agent, but adds an API dependency, RBAC and a watch to a component driven
  over a local socket. Rejected.
- **Credential files on the volume the agent shares with PageBroker.** Keeps
  secrets out of the request, but adds a file contract between the two, with
  its own permissions and cleanup. Rejected; the request already carries the
  store.
- **Mounting arbitrary PVCs at runtime.** Needs the agent to mount claims or a
  restart per claim; deferred and recorded as the limitation.
- **A default marker on the class (`isDefault`).** Lets the default move with
  one edit and no restart, but any number of classes can carry it, so it needs
  an admission policy and a tie-break rule in the operator. A configured name
  cannot conflict. Rejected.
- **A marker file that reserves a location for one class.** Stops a second
  cluster from registering the location, which is the opposite of sharing it,
  and needs create-only writes in the storage. Recording the cluster on each
  content does the protecting instead. Rejected.
- **Reference tracking for snapshots used by several clusters.** Each cluster
  that uses a snapshot records itself, and the directory is deleted with the
  last one. Belongs with restoring another cluster's snapshot; deferred. Until
  then `Retain` in the creating cluster keeps such a snapshot.
- **Parallel restore from several stores.** Needs a restore worker per store
  connection or a change in the native engine; deferred, with the session
  queue as the interim.
