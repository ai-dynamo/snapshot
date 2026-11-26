<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-248: SnapshotStore

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
    - [SnapshotStore](#snapshotstore)
    - [Request and content fields](#request-and-content-fields)
    - [Resolution](#resolution)
    - [Default store](#default-store)
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

Snapshot keeps every snapshot in one store. An administrator selects that
store when Snapshot is installed. This proposal lets a cluster hold several
stores at the same time, and lets each snapshot request select one. An
administrator registers a store as a cluster-scoped `SnapshotStore` object. A
`PodSnapshot` or `SnapshotJob` names a store in `spec.snapshotStoreName`, or
omits the field to use the default store. An administrator can add and remove
stores while the cluster runs. Several clusters can share one store. On its
first start, the operator creates a default store from the current `storage.*`
values, so an existing installation continues to work.

## Motivation

Teams already own storage, and they prefer it to the cluster-wide default
store. Their storage is faster, closer to the source Pod, or paid for and
administered by the team. Today, a team can use its own storage only through a
separate Snapshot installation. SNEP-237 prepared for this change: each
content records a `storeID` digest, and the design says that "a storage CRD"
adds a reference beside the digest. This proposal is that custom resource
definition (CRD).

### Goals

- Register more than one snapshot store in a cluster, on a PersistentVolumeClaim
  (PVC) or on S3-compatible storage. An administrator can add an S3 store
  without a restart of Snapshot when the store's addressing mode and
  certificate authority (CA) bundle match the ones PageBroker started with. The
  operator refuses a store that differs until PageBroker can restore per store,
  which is follow-up work. A PVC store needs three steps first: mount its claim
  on the operator and on the agents, run a Helm upgrade, and restart.
- Let a `PodSnapshot` or `SnapshotJob` select a store by name. A request that
  names no store uses the installation's default store.
- Bind each `PodSnapshotContent` to one store when the content is created, and
  refuse two stores in one cluster that describe the same location.
- Let several clusters keep their snapshots in one store.
- Protect a store while content references it, and let an administrator decide
  whether the deletion of content also deletes its snapshots.

### Non-Goals

- Provisioning storage. A store points at a PVC or a bucket that already
  exists.
- Mounting a PVC at runtime. In this iteration, a PVC store must name a claim
  that is already mounted on the operator and on every agent. Both components
  must also list the claim in their configuration: the operator in its
  `--pvc-claim-names` flag, and the agent in its `config.yaml`. A change to
  that list is a restart; see [Configuration](#configuration).
- Moving snapshots between stores.
- Restoring, in one cluster, a snapshot that another cluster took. A shared
  store only keeps the clusters' snapshots side by side. The `.origin` file and
  the `Retain` policy leave room for this feature, and
  [SNEP-740](../740-shared-snapshot/README.md) takes it on.
- Several storage layers for one content, where a read falls back from the
  fastest layer to the slowest. The `SnapshotStore` names the one
  authoritative location. The binding and the PageBroker protocol leave room
  for layers.
- Namespaced stores, per-namespace quotas, or role-based access control (RBAC)
  on who can name a store.
- A read-only store, for restore only. An optional access mode on the store
  can be added later without a breaking change. That feature belongs with
  [SNEP-740](../740-shared-snapshot/README.md).
- Changes to the `storeID` encoding, the publications, or the artifact layout
  of SNEP-237. The only additions are two small files beside a content's
  snapshots.

## Proposal

A `SnapshotStore` registers one snapshot store. The object names a prepared
location, carries the settings to reach that location, and protects the
location while something depends on it. Like a `PersistentVolume`, the object
describes storage that already exists and provisions nothing. One object is
one location, and the cluster never holds two objects for the same location.

**Registering a store.** The administrator creates the PVC or the bucket, puts
any credentials in a Secret in the Snapshot namespace, and creates a
`SnapshotStore`. The Snapshot namespace is the namespace in which the operator
and the agents run, which is the Helm release namespace. A `SnapshotStore` is
cluster-scoped. A second installation in the same cluster therefore resolves
the same object's claim and Secret in its own namespace. The operator computes
the `storeID` and refuses a location that another store in the cluster already
holds. The operator then runs the access check: it writes, reads, lists, and
deletes a test object at the location. When the check passes, the operator
sets the `Ready` condition. Every store, Ready or not, is validated again when
its Secret or ConfigMap changes, and at a fixed interval. A Ready store whose
credentials are revoked therefore changes to `Ready=False` and stops taking
new bindings. A store that is not Ready also retries with a backoff.

**Sharing a store between clusters.** Stores in different clusters can point
at the same location. No setting is needed. The clusters' snapshots sit side
by side, each under its own content UID. Every content directory records the
cluster that created it, in a file named `.origin`. A cluster deletes and
sweeps only directories that name that cluster.

**Selecting a store.** A `PodSnapshot`, or the template of a `SnapshotJob`,
names a store in `spec.snapshotStoreName`, or omits the field to use the
default store. The operator resolves the name once and binds the content to
that store for the whole life of the content.

**Retiring a store.** A protection finalizer holds the store while content
references it. A terminating store takes no new snapshots, but it still
serves restores and cleanup. The `reclaimPolicy` field decides what happens
to the snapshots when their content is deleted. `Delete` removes the
snapshots. `Retain` keeps them and marks them in the store, so cleanup never
removes them. The operator copies the policy onto the content at binding, and
the copy does not change afterwards.

**The default.** The operator's configuration names the default store,
`default` unless changed. When the cluster has no store, the operator creates
that store from its storage configuration, which holds the same values that
described the single store before. An upgraded installation therefore needs
no manual step. The operator never overwrites a store. To move the default,
the administrator changes the configured name. Bound content keeps its
binding. See [Default store](#default-store).

### User Stories

- Two teams share a cluster. Team A trains large models and owns an
  S3-compatible store in the same data center as its GPUs. That store has more
  bandwidth than the cluster's default store, so team A's checkpoints upload
  faster, and its restores do not compete with other teams' traffic. Team B
  runs small jobs and is satisfied with the default store. The administrator
  creates a `fast-s3` store with team A's bucket and credentials. Team A's
  `SnapshotJob` sets `snapshotStoreName: fast-s3`. Team B's jobs set nothing
  and use the default store. Each team restores from the store it checkpointed
  to. Any namespace can name `fast-s3`. Convention, not RBAC, keeps team B off
  that store; see [Security](#security).
- A platform team retires an old bucket. The team creates a store for the new
  bucket, points `storage.defaultStoreName` at the new store, and upgrades.
  New snapshots go to the new bucket. Old snapshots stay readable where they
  are. The team deletes the old store when its last snapshot is gone. The
  protection finalizer holds the old store until then.
- Two clusters keep their snapshots in one bucket. Each administrator creates
  a store that points at the bucket. Every content directory records the
  cluster that created it, so the cleanup of one cluster never touches the
  snapshots of the other cluster.

### Limitations, Risks, and Mitigations

| Risk | What goes wrong | How the design handles it |
| --- | --- | --- |
| PVC not mounted | A PVC store names a claim that the agents cannot see. Checkpoints fail on every node. | The operator sets the store to `Ready=False` with reason `ClaimNotMounted` when the claim is not in its own `--pvc-claim-names` list. That list names the claims mounted on the operator and on the agents. To add a claim, the administrator changes that list and the mounts, which restarts both components; see [Configuration](#configuration). This is a documented limitation for this iteration. |
| Agent without the claim | The store is `Ready`, because the operator's list has the claim, but one agent does not have the claim. That agent still runs the previous pod spec during a rolling upgrade, or the claim is missing on its node. | The agent maps `claimName` to a mount path from its own `config.yaml`. Before any I/O, the agent checks that the path is a mount point and not only an existing directory. The directory of a listed claim can exist with nothing mounted on it, because Kubernetes creates the directory inside the shared default claim when another pod mounts the claim there. A claim that the agent cannot map, or a path that is not a mount point, fails the operation with `StoreUnavailable`. Nothing is retried against another store. When the retry is implemented, the agent retries `StoreUnavailable`, and the operation succeeds when the node's agent has the mount. Until then, the operation fails and needs a new attempt. |
| Listed claim cannot mount | A pod starts only when all its volumes mount. A listed claim that cannot mount keeps a restarted operator or agent pod from starting. That stops snapshots on every store that the pod serves, not only on the broken store. | Three things limit the damage. A claim that does not exist or is not bound is refused before any pod restarts; see [Configuration](#configuration). The agent DaemonSet replaces one pod at a time and stops at the first pod that cannot start, so the other nodes and the running operator continue to work. Kubernetes does not remove a claim while a pod mounts it. One case remains: the storage behind a working claim fails, and a pod then restarts. To recover, the administrator removes the claim from `storage.pvc.claimNames` and upgrades. The claim's stores change to `Ready=False` with `ClaimNotMounted`. Isolation per claim is out of scope for this iteration. |
| Store edited after binding | The location fields change. Content bound to the old `storeID` reads or deletes in the wrong place. Or `reclaimPolicy` changes, and existing content loses data that it was meant to keep. | Location fields are immutable through Common Expression Language (CEL) rules, and the policy is frozen on the content at binding. Credentials and TLS settings stay editable, because they are not identity inputs. The agent and the maintenance code compare the store's `status.storeID` with the content's binding before any I/O, and refuse a mismatch. |
| Store deleted under content | Content loses the only description of where its snapshots are. | The protection finalizer holds the store. A terminating store still resolves for existing content. If an administrator removes the finalizer by force, the content gets the existing `StoreInactive` handling from SNEP-237. |
| Secret missing or revoked | The Secret of an S3 store is deleted, or rotated to an invalid key. | The store changes to `Ready=False` with reason `CredentialsUnavailable`, and new bindings stop. In-flight operations fail with the existing credential errors. Nothing is deleted on an access error. |
| Agent holds credentials | Today, only PageBroker and the operator see S3 credentials. With this proposal, the agent reads the store's Secret and passes it to PageBroker in the request. | The Secret must live in the Snapshot namespace. The operator limits the agent's Role by `resourceNames` to the Secrets and ConfigMaps that stores reference. The operator removes a rule whose list would be empty, because an empty list allows every object. A node can still read the credentials of every store, where today it holds the credentials of one store. The credentials stay in memory in both processes and cross only the local socket that the agent already uses. They are never written to a volume, a transaction record, or a log. This is an accepted trade-off against a Kubernetes client in PageBroker; see Alternatives. |
| Shared workload identity | Stores without a Secret are all reached with one identity: the service accounts of the agent and the operator. | That identity needs access to every such store, so credentials do not separate those stores from each other. Use a Secret per store where separation matters. Instance metadata is excluded, so a broken identity fails instead of falling back to the node's role. |
| Duplicate location | Two `SnapshotStore` objects in one cluster describe the same place. One location gets two policies and two sets of access settings. | The operator refuses `Ready` for the later store with reason `DuplicateStore`, and names the other store. Content bound to the refused store keeps its backend. Two stores that reach one bucket through different hostnames are not detected. That is harmless, because a sweep keeps every directory whose content exists in the cluster, whichever store the content is bound to. See [Store uniqueness](#store-uniqueness). |
| Shared store | Several clusters use one location. A sweep or a delete in one cluster could remove the snapshots of another cluster. | Each content directory carries `.origin` with the ID of the creating cluster. A sweep deletes only orphans whose `.origin` names its own cluster. A content deletion skips a directory that names another cluster. The credentials of each cluster can still read and delete the snapshots of the other clusters, so share a store only between clusters that trust each other. The directories of a destroyed cluster stay until an administrator removes them. |
| Older release on the same location | A cluster on a release without `SnapshotStore`, or a cluster rolled back to such a release, writes no `.origin`. It sweeps every directory that has no content in that cluster. | The store cannot prevent this. Do not share a location with such a cluster. Before a rollback, make sure that no other cluster uses the location. |
| Rollback with retained data | The previous release ignores `reclaimPolicy` and `.retained`. After a rollback, its sweep deletes retained directories in the store that `storage.*` names, and the deletion of a `Retain` content deletes its data. | The new release cannot prevent this. The rollback procedure tells the administrator to copy retained data out of that store first. The previous operator does not reach the locations of other stores. |
| Nested stores | The location of one store is a parent of the location of another store, in this cluster or in another cluster. | Nesting is allowed, because each store keeps its snapshots under its own `artifacts/` directory. A sweep looks only there and deletes only orphans named by a UUID. A content deletion removes only the handles recorded on that content. The API rejects a location inside the `artifacts/` directory of another store. |
| Old agent, new store | During a rolling upgrade, an agent from before this proposal receives content bound to a non-default store. | The agent passes its startup store to PageBroker. That store's `storeID` does not match the content, so PageBroker returns `STORE_MISMATCH`, and the checkpoint fails before any I/O. That agent treats the failure as final: the `PodSnapshot` fails with `BrokerRejected` and is not retried. The snapshot needs a new attempt after the node is upgraded. Create non-default stores only after every node runs the new agent. |
| Restore from several S3 stores | The native restore engine holds one session per PageBroker process. A session is fixed to one endpoint, one region, and one set of credentials. The engine reads its CA bundle and addressing mode once, at start. | Restores whose stores share the endpoint, the region, and the credentials run in parallel. A restore that differs in any of the three waits until the other restores drain. PageBroker then replaces the session. A long restore delays the other restores, and a waiting transaction can expire. An S3 store whose CA bundle or addressing mode differs from PageBroker's startup settings cannot restore. The operator therefore refuses such a store with `RestoreSettingsMismatch`, instead of letting checkpoints succeed and restores fail. Parallel restore across sessions is follow-up work; see [Agent and PageBroker](#agent-and-pagebroker). |
| Any namespace may use a store | A store is cluster-scoped, and any `PodSnapshot` can name it. A team's store is therefore not reserved for that team. Another namespace can checkpoint into the store and restore from it. | This is intended for this release. A store is usable by every namespace, like a `StorageClass`, and RBAC on naming a store is a non-goal. Workloads never see the store's credentials. An optional namespace allow-list on the store can be added later without a breaking API change. |
| Default misconfigured | `storage.defaultStoreName` names a store that does not exist, is not Ready, or is being deleted. Unnamed `PodSnapshot`s fail. | The default is one configured name, so two defaults cannot exist. An unnamed `PodSnapshot` fails with the same reasons as a named one, and retries until the store is usable. The operator reports a missing default at start, and a Warning event flags a terminating default; see [Default store](#default-store). |

## Design Details

### API

#### SnapshotStore

`SnapshotStore` is a new cluster-scoped kind in `nvidia.com/v1alpha1`. Exactly
one store block is set, `pvc` or `s3`, and the set block names the kind of
store. The location fields of the block are the `storeID` inputs defined in
`../../../api/storage`. They are immutable through CEL transition rules, and a
store cannot move from one block to the other. The `reclaimPolicy` field, the
credentials, and the TLS settings stay editable. An edit to `reclaimPolicy`
applies to content bound after the edit. A store name has at most 63
characters, because the name is used as a label value.

```go
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters"
type SnapshotStore struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   SnapshotStoreSpec   `json:"spec"`
    Status SnapshotStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.pvc) != has(self.s3)",message="exactly one of pvc or s3 must be set"
// +kubebuilder:validation:XValidation:rule="has(self.pvc) == has(oldSelf.pvc)",message="the store block cannot change between pvc and s3"
type SnapshotStoreSpec struct {
    // +kubebuilder:validation:Enum=Delete;Retain
    // +kubebuilder:default=Delete
    ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
    PVC *PVCStoreSpec `json:"pvc,omitempty"`
    S3  *S3StoreSpec  `json:"s3,omitempty"`
}

// A claim in the namespace Snapshot is installed in. It must be mounted into
// the operator and every node agent.
// +kubebuilder:validation:XValidation:rule="self.claimName == oldSelf.claimName && self.subPath == oldSelf.subPath",message="claimName and subPath are immutable"
// +kubebuilder:validation:XValidation:rule="!self.subPath.split('/').exists(p, p == 'artifacts' || p == '.claims' || p == '..')",message="subPath must not contain an artifacts, .claims or .. segment"
type PVCStoreSpec struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    ClaimName string `json:"claimName"`
    // A directory inside the claim; "/" is the whole claim.
    // +kubebuilder:default="/"
    // +kubebuilder:validation:MaxLength=4096
    // +kubebuilder:validation:Pattern=`^/`
    SubPath string `json:"subPath,omitempty"`
}

// Endpoint, Region, Bucket and Prefix are storeID inputs; the rest are access
// settings and stay editable. An absent and an empty prefix compare as equal,
// so an update that drops the empty field is not taken for a location change.
// +kubebuilder:validation:XValidation:rule="self.endpoint == oldSelf.endpoint && self.region == oldSelf.region && self.bucket == oldSelf.bucket && self.?prefix.orValue('') == oldSelf.?prefix.orValue('')",message="endpoint, region, bucket and prefix are immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !self.prefix.split('/').exists(p, p == 'artifacts' || p == '.claims' || p == '..')",message="prefix must not contain an artifacts, .claims or .. segment"
// +kubebuilder:validation:XValidation:rule="self.?allowHTTP.orValue(false) || !self.endpoint.startsWith('http://')",message="allowHTTP is required for an http:// endpoint"
type S3StoreSpec struct {
    // An absolute http(s) URL with no path, query, fragment or userinfo.
    // +kubebuilder:validation:MaxLength=2048
    // +kubebuilder:validation:Pattern=`^https?://[^/?#@]+$`
    Endpoint string `json:"endpoint"`
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=255
    Region string `json:"region"`
    // +kubebuilder:validation:MinLength=3
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`
    Bucket string `json:"bucket"`
    // A relative key prefix; empty is the bucket root.
    // +kubebuilder:validation:MaxLength=1024
    // +kubebuilder:validation:Pattern=`^[^/].*$|^$`
    Prefix string `json:"prefix,omitempty"`
    // A Secret in the Snapshot namespace holding a shared-credentials file.
    // Omitted: workload identity is used.
    CredentialsSecretRef *SecretKeyRef `json:"credentialsSecretRef,omitempty"`
    // +kubebuilder:validation:Enum=virtual;path
    Addressing string `json:"addressing,omitempty"`
    // Required for an http:// endpoint; PageBroker refuses plain http otherwise.
    AllowHTTP bool `json:"allowHTTP,omitempty"`
    CABundleConfigMapRef *ConfigMapKeyRef `json:"caBundleConfigMapRef,omitempty"`
}

// SecretKeyRef names a key in a Secret in the Snapshot namespace.
type SecretKeyRef struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    Name string `json:"name"`
    // +kubebuilder:default=credentials
    Key string `json:"key,omitempty"`
}

// ConfigMapKeyRef names a key in a ConfigMap in the Snapshot namespace.
type ConfigMapKeyRef struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    Name string `json:"name"`
    // +kubebuilder:default=ca.crt
    Key string `json:"key,omitempty"`
}

type SnapshotStoreStatus struct {
    StoreID    string             `json:"storeID,omitempty"` // From the location fields.
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

The schema carries the limits that `api/storage` enforces. A location that
`api/storage` would refuse is therefore refused at creation, instead of frozen
in an immutable field. A pattern cannot express every rule, for example a
bucket name that is a dotted IP address. Such a value fails normalization in
the operator, which sets `Ready=False` with reason `InvalidLocation`. The
Secret and ConfigMap references are small own types instead of
`corev1.SecretKeySelector`. That type requires `key`, so a default is not
possible, and its `optional` field has no meaning here. `subPath` is the
Kubernetes word for a path inside a volume. The identity input in
`api/storage` is renamed to match. The serialized key in the operator's
storage configuration file stays `basePath` until PageBroker reads the new
name. The digest hashes the value and not the key, so `storeID` does not
change.

`Ready` carries the reasons `ClaimNotMounted`, `CredentialsUnavailable`,
`DuplicateStore`, `InvalidLocation`, `RestoreSettingsMismatch`,
`StoreUnreachable`, and `Terminating`.

#### Request and content fields

`PodSnapshot` (`spec`) and the `SnapshotJob` template
(`spec.podSnapshotTemplate`) get an optional `snapshotStoreName` field. An
empty value selects the default store.

`PodSnapshotContent.spec.storage` has the type `CheckpointStorageBinding`.
Today, the type has one field, `storeID`, which is the digest of the location.
The field is required and immutable, and that does not change. This proposal
adds two fields. `snapshotStoreName` says how to reach the location that the
digest identifies. The digest stays the authority on *where* the location is.
`reclaimPolicy` is copied from the store at binding and is immutable, so a
later edit of the store never changes what happens to existing content. The
store always has a policy, because the policy defaults to `Delete`, so every
binding sets one. Content bound before `SnapshotStore` existed has neither
field. The operator never rewrites such content, because the content `spec` is
immutable. The operator finds the store of such content by matching its
`storeID` against the stores. The empty policy of such content means `Delete`,
as today.

```go
// PodSnapshotContentSpec.Storage; immutable once set.
// +kubebuilder:validation:XValidation:rule="!has(self.snapshotStoreName) || has(self.reclaimPolicy)",message="reclaimPolicy is required when snapshotStoreName is set"
type CheckpointStorageBinding struct {
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
    // +kubebuilder:validation:MaxLength=73
    StoreID           string        `json:"storeID"`
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    SnapshotStoreName string        `json:"snapshotStoreName,omitempty"`
    ReclaimPolicy     ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}
```

#### Resolution

1. The `SnapshotJob` reconciler resolves the store of the template before it
   starts the source Pod. A long run is therefore not spent on a snapshot that
   cannot be bound. A terminating store sets `InvalidSpec`, which is terminal,
   because that store does not come back. A missing store or a store that is
   not Ready is a wait: the job reports `StoreNotFound` or `StoreNotReady`,
   starts no pod, and continues when the store appears or becomes Ready. A
   store and a job that are applied together therefore succeed in either
   order.
2. The `PodSnapshot` reconciler reads `spec.snapshotStoreName`. When the field
   is empty, the reconciler reads the default store name from the operator's
   configuration.
3. At binding, the reconciler requires `Ready=True` and no deletion timestamp.
   Otherwise, the reconciler sets `Ready=False` on the `PodSnapshot` with
   `StoreNotFound`, `StoreNotReady`, or `StoreTerminating`, and requeues. A
   watch on `SnapshotStore` picks up later changes. This check is the
   authority: the job check is early, and this check is where the `storeID`
   is frozen.
4. The reconciler creates the `PodSnapshotContent` with
   `spec.storage.storeID`, `snapshotStoreName`, and `reclaimPolicy`, and
   labels it `nvidia.com/snapshot-store=<name>`. The reconciler then reads
   the store again from the API server. If the store is terminating or gone,
   the reconciler deletes the new content and fails the snapshot with
   `StoreTerminating`.
5. The store reconciler removes the `nvidia.com/snapshotstore-protection`
   finalizer only when two conditions hold: the store is terminating, and no
   content references the store. Content references a store when it names the
   store in `spec.storage.snapshotStoreName`, or, with no store name, when it
   carries the store's `storeID` and resolves to that store. Content with no
   store name protects only the store that `backendForContent` selects for
   it. A store refused as `DuplicateStore` can therefore be deleted, while
   the store that serves the content stays protected. The check is a live
   list on the spec fields. The label is editable and only serves queries.

#### Default store

The default store is named in the operator's configuration, not on the store.
The flag `--default-snapshot-store` names it, `default` unless changed, and
`storage.defaultStoreName` in [Configuration](#configuration) sets the flag. A
request with no `snapshotStoreName` resolves to that name, and then follows
the same rules as a request that names a store. One configured name means
that there is never more than one default. The store therefore carries no
default field, and nothing has to arbitrate between stores.

- **Moving the default.** Create the new store, change the value, and run
  `helm upgrade`. Only the operator restarts. The agents never read the
  default, so their configuration checksum leaves the value out. Bound content
  keeps its binding.
- **Default missing.** If the named store does not exist, the operator reports
  the problem at start. Unnamed `PodSnapshot`s fail with `StoreNotFound` until
  the store exists.
- **Default deleted.** A Warning event names the effect. Unnamed
  `PodSnapshot`s fail with `StoreTerminating` while content references the
  store, and with `StoreNotFound` after that. The operator creates the default
  store only when the cluster has no store at all. A deleted default is
  therefore not brought back while another store exists.
- **Values changed after the first start.** An edit of `storage.*` followed by
  an upgrade restarts the operator but changes no store. Today, the same steps
  switch the store. At start, the operator compares those values with the
  default store. When they differ, the operator logs the difference and emits
  a Warning event `StoreValuesDiffer` on the store. The event names the field
  that differs and the way to switch: create a store and point
  `storage.defaultStoreName` at it.

#### Store uniqueness

Inside a cluster, one location has exactly one store, and no store sits inside
the snapshots of another store. Between clusters, a location can be shared.

- **Reserved paths.** The API rejects a `prefix` or `subPath` with a path
  segment named `artifacts`. That is the directory in which every store keeps
  its snapshots, so no store can be placed inside the `artifacts/` directory
  of another store, in this cluster or in another cluster. The API also
  rejects `.claims`, the directory below which the agent and the operator
  mount the listed claims. A store on the default claim therefore cannot be
  placed where a claim is mounted; see
  [Agent and PageBroker](#agent-and-pagebroker). The API also rejects a `..`
  segment, as the store identity code already does, so a `subPath` cannot
  leave its claim.
- **Digest.** Another store in the cluster with the same `storeID` is
  `DuplicateStore`. The scan reads the `SnapshotStore` informer cache and
  compares only immutable location fields. The addition or deletion of a store
  reconciles every store with the same digest again. A stale cache can
  therefore only delay a refusal, and never make a refusal permanent. The
  operator refuses the later store by `creationTimestamp`. A tie is possible,
  because the timestamp has second precision. The configured default store
  wins a tie, then the name decides. Two stores created at the same time
  therefore get the same answer in any reconcile order. Two stores can both be
  Ready for a moment while the cache is stale. Content bound in that moment
  stays on its store, and the next reconcile refuses the later store. Bindings
  play no part in the decision. A store refused as `DuplicateStore` takes no
  new bindings, and the content that it already has keeps its backend; see
  [Maintenance](#maintenance).
- **Access.** Before `Ready`, the operator writes a test object at the
  location, reads it back, finds it in a listing, and deletes it. Those are
  the four permissions that a snapshot needs over its life: checkpoint,
  restore, cleanup, and deletion. The object is
  `artifacts/.access-check-<cluster ID>`. The object sits inside `artifacts/`,
  where the snapshots are, so a policy scoped to another path cannot pass the
  check. The check proves access to that one key. A policy written per key can
  still deny `artifacts/<uid>/`, so `Ready` does not guarantee access to every
  snapshot key. The object name is not a UUID, so no sweep touches it. The
  object name carries the cluster ID, so two clusters never collide. A failure
  is `StoreUnreachable`, or `CredentialsUnavailable` when the credentials are
  the cause. The condition message names the step that failed. The check runs
  from the operator, so it does not prove that every node can reach the store.

Nothing at the location marks the location as taken. Stores in several
clusters can therefore point at one location. Two files per content keep the
clusters apart:

- **`.origin`.** Every directory that this cluster writes carries
  `artifacts/<uid>/.origin`. The file is a JSON file with a `version`, the
  cluster ID, the time, and the namespace and name of the `PodSnapshot`. The
  cluster ID is the UID of the `kube-system` namespace, which is fixed for the
  life of the cluster. The agent builds the record and passes it in
  `ArtifactTarget.origin`. PageBroker writes the record before the first object
  of every publication, when the file is absent. A publication that lands
  after cleanup therefore recreates the directory with its marker, and the
  next sweep removes the directory, as SNEP-237 intends. The operator writes
  the same record at binding, so the directory is marked before any
  publication. The operator also writes the record for content bound before
  this proposal. The file is never changed once present. PageBroker publishes
  below the file, under `containers/`, so the two do not conflict.
- **`.retained`.** The operator writes this file beside `.origin` when content
  is deleted under `Retain`; see [Maintenance](#maintenance).

A missing or unreadable `.origin`, or an `.origin` with an unknown `version`,
means that the directory does not belong to this cluster. Stores can nest
outside `artifacts/`: `team-a/` and `team-a/prod/` are two stores. Each store
keeps its snapshots under its own `artifacts/`, and a sweep looks only there,
so neither store can reach the data of the other.

#### Agent and PageBroker

The agent already carries `storeID` and the artifact identity to PageBroker.
With this proposal, the agent adds the resolved store through the
`StorageBackend` selector, so PageBroker no longer depends on one store fixed
at startup. PVC reuses `FilesystemStorage`. S3 is the one new kind. The
`--storage-config` flag of PageBroker no longer selects a store. The code
below shows only the new fields.

```protobuf
message StorageBackend {
  oneof kind {
    FilesystemStorage filesystem = 1;  // PVC: mount path joined with subPath.
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
  bytes ca_bundle = 8;    // PEM, from the store's ConfigMap. Empty: system roots.
}

message ArtifactTarget {
  StorageBackend storage = 3;  // Resolved store.
  bytes origin = 4;            // The .origin record; written before the first object if absent.
}
message PublishedArtifact { StorageBackend storage = 3; }  // Restore and metadata reads.
```

PageBroker trusts `store_id` and `StorageBackend` as it trusts its startup
configuration today. The binding check belongs to the agent. PageBroker caches
S3 clients by `store_id`, `addressing`, `allow_http`, and a SHA-256 digest of
the credentials and CA bundle bytes in the request. PageBroker computes the
digest when it builds the client, so rotated credentials or a changed setting
get a new client. PageBroker drops an entry that is idle for 15 minutes. The
cache holds at most 64 entries. PageBroker never drops a client with calls in
flight. A missing store or Secret fails the operation with `StoreUnavailable`,
and nothing is retried against another store. The agent classifies
`StoreUnavailable` as retryable, like a store that is temporarily unavailable,
because a store, Secret, or mount that is missing during a rollout appears
without a new request. This retry is implemented last, after the rest of the
proposal works. Until then, `StoreUnavailable` fails the operation before any
I/O, and the operation needs a new attempt. The retry removes that manual step
and changes nothing else.

For PVC, the agent maps `claimName` to a mount path: the default claim at
`/checkpoints`, and each listed claim at `/checkpoints/.claims/<claimName>`.
The agent reads the claim list from its `config.yaml`. The operator derives
the same paths from its `--pvc-claim-names` flag. The listed claims sit below
one reserved parent, `.claims`. One reserved segment therefore keeps the
claims apart from any directory that a store or the operator's cleanup uses at
the root of the default claim, such as `artifacts/` or `non-target-artifacts/`.
A store on the default claim can use any `subPath` that avoids that segment.
Every store path is below `/checkpoints`, the one source root that the
privileged mount helper accepts. The direct restore of a snapshot on a listed
claim therefore needs no change to the helper.

Before any I/O on a PVC store, the agent confirms from its mount table that the
mount path of the claim, `/checkpoints` or `/checkpoints/.claims/<claimName>`,
is a mount point. A directory that only exists is not enough. The agent
resolves the store's `subPath` below that mount after the check, so a store in
a subdirectory of a claim passes. A listed claim is mounted inside the mount
of the default claim, so Kubernetes creates `.claims/<claimName>` in the
default claim, which every agent shares. An agent pod that still runs the
previous spec sees that directory without the mount. That pod also sees the
new claim list, because `config.yaml` is refreshed in running pods. Without
the check, the pod would write the snapshot into the default claim. A path
that is not a mount point fails with `StoreUnavailable`. The agent runs the
check before it calls PageBroker, which shares the mounts of the pod. The
operator makes the same check before cleanup.

How a restore reads a snapshot depends on the kind of store. From a PVC store,
the restore is direct by default: the snapshot directory on the claim is
mounted read-only into the pod, and nothing is copied into the staging area of
PageBroker. `pageBroker.restoreMode: staged` remains the opt-in for a staged
copy. From an S3 store, the restore is always staged, because S3 has no other
read path. Today, a bound snapshot is restored only through staging, on a PVC
too. The direct restore of a bound snapshot on a PVC is therefore new work
that this proposal depends on; see [Dependencies](#dependencies).

Non-target artifacts are the files that a non-target container writes under
`non-target-artifacts/<SnapshotJob UID>`. They are not snapshot data, and they
do not follow the store. They stay on the default claim at `/checkpoints`,
which the operator and every agent mount. The operator cleans them there,
whatever store the job names. The value that names that claim is in
[Configuration](#configuration).

S3 payload restores do not use the S3 client cache. They go through the native
restore engine, which holds one session per PageBroker process and reads its
CA bundle and addressing mode once, at start. A session is fixed to one
endpoint, one region, and one set of credentials for its whole life. Restores
whose stores share all three run in parallel in one session, as today. That
includes stores in the same region that use the same workload identity. When
the next restore differs in any of the three, or when the credentials have
changed, that restore waits until the running restores drain. PageBroker then
ends the session and starts a session with the new settings. This reuses the
path that replaces a failed session today. Until restores run in parallel
across sessions, a long restore delays the other restores, and a waiting
transaction can expire.

The addressing mode and the CA bundle of the engine are fixed at start and
shared by every session. An S3 store that differs in either could checkpoint
but never restore. The operator refuses such a store. The operator reads the
startup settings from its storage configuration file, which holds the same
values that PageBroker starts with. The operator compares them with the
store's `addressing` and `caBundleConfigMapRef`, and sets `Ready=False` with
`RestoreSettingsMismatch`. The condition names the setting and the value that
PageBroker runs with. The check is part of every reconcile, so a store becomes
Ready after a restart that changes the settings. In an installation whose
default store is a PVC, PageBroker starts with no S3 settings. That means
virtual addressing and the system roots. An S3 store in such an installation
must use those settings. A path-addressed store, or a store with a private
CA, is refused with the same reason until restores run per store.

#### Credential delivery

S3 credentials are not mounted. A Secret volume is fixed when the pod starts,
which is why the single store of today needs a restart. The credentials are
also not written to the volume that the agent shares with PageBroker. This
design adds no file exchange between the two. The agent passes the credentials
in the request:

1. At Prepare, the agent reads the store's Secret, and the CA bundle ConfigMap
   if set, with an uncached `Get`.
2. The agent puts their contents in `S3Storage.credentials` and
   `S3Storage.ca_bundle` on every call that carries a `StorageBackend`. The
   credentials are in the shared-credentials format that PageBroker parses
   today.
3. PageBroker keeps the credentials in memory for the transaction. PageBroker
   hands them to its S3 tooling only through the environment of the child
   process, as today. Credentials never appear in command arguments. PageBroker
   does not store them in a transaction record, so a call after a PageBroker
   restart carries them again. The CA bundle is not secret. Where a tool needs
   a path, PageBroker writes the bundle to its own scratch space.

A store with no `credentialsSecretRef` uses workload identity. The agent
leaves `credentials` empty. PageBroker and the operator then use the default
credential chain of the SDK under their own service accounts, with instance
metadata excluded, so the role of the node is never used. Those credentials
refresh on their own. The rule is the same on every platform. Where the
default chain cannot perform the token exchange of a platform, PageBroker and
the operator need a credential provider for that platform, behind the same
rule. That provider is follow-up work, with no change to the store.

Credentials are read per transaction, not per store. A rotated Secret reaches
the next transaction, while an in-flight transaction keeps its key. For a
restore payload, the new key takes effect when the restore session is
replaced; see [Agent and PageBroker](#agent-and-pagebroker). A retried
checkpoint or restore is a new transaction, so it reads the Secret again. The
client cache of PageBroker is where sharing happens.

#### Maintenance

The backend registry is built from Ready and terminating stores, and from any
store with bound content, whatever its condition. Bound content therefore
always resolves. `backendForContent` resolves the store of the content by
name, or by `storeID` for content with no store name. For content with no
store name, it prefers a store that is not `DuplicateStore`. It then checks
the store's `storeID` against the binding and returns that backend.

Under the content's `reclaimPolicy: Retain`, `delete-content` writes
`artifacts/<uid>/.retained`. The file is a JSON file with the cluster ID, the
store name, the `storeID`, and the time. `delete-content` then removes the
finalizer and emits `SnapshotsRetained` with the handle. It deletes nothing.
The file stands on its own: nothing else has to be read to know that the
directory is kept, and where it came from. Under `Delete`, `delete-content`
reads `.retained` first. When the file exists, `delete-content` keeps the
directory and emits `SnapshotsRetained`, as under `Retain`. The file means the
same thing whichever path finds it. An administrator can therefore keep a
snapshot by creating the file by hand. That is the way to protect content
bound before this proposal, whose empty policy means `Delete`. The presence of
the file is what counts; the fields are informational. Otherwise, `Delete`
removes only the handles recorded on the content, and skips, with a Warning
event, a directory whose `.origin` names another cluster.

Sweeps run per store and never cross stores. A sweep looks only inside
`<location>/artifacts/`. A sweep deletes a directory only when all of these
conditions hold: the directory name is a UUID, the `PodSnapshotContent` UID
that PageBroker names it by; no content in the cluster has that UID; the
directory carries no `.retained`; and its `.origin` names this cluster. The
sweep checks the content first, and reads `.retained` right before the
deletion, after `delete-content` has written it. Today, only the content check
exists: every directory there with a safe name and no matching content is an
orphan. Leftovers from before the upgrade therefore have no `.origin`. They
are never swept, and they need a one-time manual cleanup. Directories that
this release writes always have the marker, including a directory that a late
publication recreates after cleanup, because PageBroker writes the marker
before the data. Content whose store is gone keeps its finalizer and gets
`StoreInactive`. SNEP-237 defines that
[handling](../237-s3-storage/README.md#limitations-risks-and-mitigations), and
this proposal does not change it.

### Security

- **Who may create a store.** Cluster-level RBAC, like `StorageClass`. To name
  a store, a user needs only write access to `PodSnapshot` or `SnapshotJob`.
  The creation of a store says that any `PodSnapshot` can use it.
- **Operator.** The operator gets new rules, none of which it holds today:
  `get`/`list`/`watch` on Secrets and ConfigMaps in the Snapshot namespace,
  with a metadata-only watch so that no contents are cached; `get` and
  `update` on the agent's Role in that namespace, to maintain its
  `resourceNames`, which Kubernetes allows because the operator holds the
  permissions that it grants; `get` on the `kube-system` namespace, for the
  cluster ID; and full access to `snapshotstores` and their status.
- **Credentials.** Secrets live in the Snapshot namespace and replace the S3
  file mounts of today. A namespaced Role gives the agent `get`, and neither
  process caches the contents. A store is not reserved for a namespace; see
  the risk row. The operator maintains the agent's Role so that its
  `resourceNames` list only the Secrets and ConfigMaps that stores reference.
  Every node can still read the credentials of every store. An empty
  `resourceNames` list allows every object of the kind. When no store
  references a Secret, the operator therefore removes the Secret rule from the
  Role, and likewise the ConfigMap rule. The agent's ClusterRole gets
  `get`/`list`/`watch` on `snapshotstores`, and `get` on the `kube-system`
  namespace for the cluster ID that the agent puts in `.origin`. A store
  without a Secret uses the workload identity of the service accounts. The
  agent holds a Secret only in memory and sends it only over the local socket.
  Neither process writes a Secret to a volume or a log.
- **PageBroker.** No Kubernetes client, RBAC, or long-lived credentials.
  PageBroker trusts the agent on the local socket, as today.
- **Shared stores.** Every cluster that shares a store holds credentials that
  can read and delete the snapshots of the other clusters. The `.origin` file
  guides cleanup and is not access control. Sharing is for clusters that trust
  each other.

### Configuration

The chart keeps `storage.*` but ships no store object. Helm applies a release
before the `crd-installer` of the operator has created the new CRD, so the
operator creates the default store instead. The RBAC of the installer gets the
new CRD. These are the new values:

```yaml
storage:
  # The store used by requests that name none. The operator creates it from
  # the values below when the cluster has no store.
  defaultStoreName: default
  pvc:
    # Claims that PVC stores may use, in the Snapshot namespace.
    claimNames: []
```

The contract has three parts: the operator's `--pvc-claim-names` flag, which
names the claims that the operator can mark Ready; the claim list in the
agent's `config.yaml`, which the agent maps to
`/checkpoints/.claims/<claimName>`; and the volume mounts at those paths on
the operator pod and the agent pods. The chart sets all three from
`storage.pvc.claimNames`. An installation that does not use the chart sets
them the same way. A change to the list restarts both components, because
mounts are fixed at pod start. One order matters: every agent and the operator
must have the mount before a store names the claim. The operator marks a
store Ready from its own list. A store created while agents still roll out
therefore fails checkpoints on the nodes that lack the mount, until the retry
lands. To add a claim, follow these steps: add the claim, upgrade, wait for
the agent DaemonSet and the operator Deployment to finish rolling, and then
create the store.

A claim name is all that `claimNames` needs, because the chart derives the
rest. For each name, the chart adds a volume and a mount at
`/checkpoints/.claims/<claimName>` to the operator and to the agent and
PageBroker containers of the agent pod, as it already does for the default
claim at `/checkpoints`. Kubernetes mounts them when those pods restart. The
name is unique in the namespace and is a valid path segment, so no two claims
share a path, and nobody has to choose one. The listed claims are mounted
inside the mount of the default claim, so Kubernetes creates an empty
`.claims/<claimName>` directory in the default claim. The reserved parent is
what keeps a claim from hiding a directory at the claim root; see
[Store uniqueness](#store-uniqueness). The chart refuses a list that contains
`storage.pvc.name`, because one claim name would then map to two paths.
`storage.pvc.name`, the single claim at `/checkpoints`, is deprecated as a
store location: `claimNames` replaces it for stores. It still names the claim
that holds non-target artifacts, and a dedicated value takes that over before
`storage.pvc.name` is removed. When `storage.pvc.name` is removed,
`/checkpoints` holds only `.claims/` with the mounts of the listed claims. The
chart writes the claim names into the agent's `config.yaml` and into the
operator's `--pvc-claim-names` flag, so the `claimName` of a store always
resolves to the directory that was mounted. The claims must already exist in
the Snapshot namespace and be mountable on every agent node. On install and
upgrade, the chart looks up each listed claim and fails when a claim does not
exist or is not bound. A wrong name is therefore an error message and not a
pod that cannot start. A dry run cannot look claims up, so it skips the check.
The list is the whole set, so an upgrade repeats the claims that it keeps:

```yaml
storage:
  pvc:
    claimNames:
      - team-a-snapshots   # /checkpoints/.claims/team-a-snapshots
      - team-b-snapshots   # /checkpoints/.claims/team-b-snapshots
---
apiVersion: nvidia.com/v1alpha1
kind: SnapshotStore
metadata:
  name: team-b
spec:
  pvc:
    claimName: team-b-snapshots   # Which mounted claim.
    subPath: /prod                # A directory inside it, not the mount path.
```

`SnapshotStore` requires store binding and artifact addressing, which the
chart values `operator.bindNewContents` and `pageBroker.artifactAddressing`
turn on. While binding is off, the operator still creates the default store,
unnamed requests resolve to it, and snapshots keep the old flow. The operator
does not reconcile any other store, and a request that names another store
fails with `StoreNotReady`. The message says that binding is off. Both values
are deprecated, because stores leave them nothing to choose between. Their
removal is planned separately.

Every other `storage.*` value keeps its meaning and feeds the default store.
`storage.type` selects the `pvc` or `s3` block. `s3.endpoint`, `region`,
`bucket`, `prefix`, `addressing`, `allowHttp`, `credentialsSecretRef`,
`credentialsSecretKey`, `caBundleConfigMapRef`, and `caBundleConfigMapKey`
become the `s3` fields of the store, with `allowHttp` landing in `allowHTTP`.
`pvc.name` becomes `claimName`, with the store `subPath` at the claim root.
`pvc.basePath` is a different thing: the fixed container mount path
`/checkpoints`. It is not an identity input, and it goes away with the single
claim. `pvc.create`, `size`, and `storageClass` still only provision the
claim. An empty `s3.credentialsSecretRef` gives a default store with no
Secret. What changes is the delivery: the chart no longer mounts the S3
Secret. The agent reads the Secret through the API and passes it per
transaction. The CA ConfigMap stays mounted in PageBroker, because the native
restore engine reads its CA bundle and addressing mode once, at start.

The chart renders these values into the operator's storage configuration
file, which the operator reads through `--snapshot-storage-config`. The chart
passes `storage.defaultStoreName` as `--default-snapshot-store`. The operator
uses the file for one thing: when the cluster has no `SnapshotStore` at all,
the operator creates the default store under that name. That covers a fresh
install and the first upgrade. If any store exists, the operator does
nothing, so it does not recreate a deleted store or revert an edit while
another store remains. A cluster whose last store was deleted has no store
again, so the next operator start creates the default store. That is
intended. Later changes to the `storage.*` values do not change an existing
store. The operator warns when the values differ from the default store; see
[Default store](#default-store). To switch stores, create a new store and
point `storage.defaultStoreName` at it.

A `SnapshotStore` is the only way to define a store. The operator builds its
backend registry from stores only, so this release removes the S3 credential
flags of the operator. An operator started with such a flag fails at start
with an unknown-flag error. Remove the flag from any hand-edited arguments.
`--snapshot-storage-config` stays, only as the seed for the default store.
The `--storage-config` flag of PageBroker no longer selects a store. It
carries the process-wide restore settings.

#### Upgrade from a single-store installation

An installation upgraded from a release without `SnapshotStore` continues to
work after a `helm upgrade` with the previous values:

1. The operator applies the new CRD, finds no store, and creates the default
   store, named `default`, from the same `storage.*` values. Its `storeID` is
   therefore byte-identical to the one recorded on every bound
   `PodSnapshotContent`.
2. Existing content is not rewritten. Content that has a `storeID` but no store
   name is matched to a store by digest whenever the content is used, and the
   content protects that store from deletion. That store is `default`. The
   default is created before any other store can exist, and a tie on
   `creationTimestamp` goes to the default, so the default is never the store
   refused as `DuplicateStore`. Content that matches no store keeps the
   [`StoreInactive` handling](../237-s3-storage/README.md#limitations-risks-and-mitigations)
   of SNEP-237. If a store is created before the operator seeds the default,
   the operator does not seed it. To recover, create a store with the same
   `storage.*` location under any name. Legacy content resolves to that store
   by digest.
3. Content with no `spec.storage`, bound before SNEP-237, keeps the legacy
   path-addressed flow and is never assigned a store.
4. `PodSnapshot`s and `SnapshotJob`s without `snapshotStoreName` resolve to the
   default store.
5. Content bound before the upgrade has no `reclaimPolicy`, which means
   `Delete`. To keep such a snapshot when its content is deleted, create
   `artifacts/<content UID>/.retained` in its store. Both deletion and sweeps
   honor the file.

Create stores other than the default only after the agent rollout finishes. An
agent from the previous release knows one store, and a checkpoint that it
receives for another store fails for good. The same order applies to every
later change of the claim list; see [Configuration](#configuration).

A rollback to the previous chart leaves the CRD and the `default` object in
place. The old operator ignores both and reads `storage.yaml`.

The previous release does not know `reclaimPolicy` or the `.retained` file,
and its sweep cannot be turned off. In the store that `storage.*` names, a
rollback therefore loses retained data in two ways. The first sweep deletes
every directory that has no content in the cluster, which includes all data
already retained. The deletion of a content created with `Retain` deletes its
data. Before a rollback, copy the retained data out of that store, and do not
delete `Retain` content until the cluster is upgraded again. The previous
operator reads only that one store, so the locations of other stores are not
touched.

### Monitoring

- `SnapshotStore` status: `storeID`, and `Ready` with the reasons above. A
  refused store names the other store in its condition, and in a Warning
  event once per transition. Only the later store is marked.
- Events: `SnapshotsRetained` under `Retain`, or under `Delete` when
  `.retained` exists; `StoreTerminating` on the store while references remain,
  with a count; a Warning on a terminating default; and `StoreValuesDiffer`
  on the default store when the storage values of the operator no longer
  match it.
- Metrics: `snapshot_store_ready{store}` and
  `snapshot_store_bound_contents{store}`. The maintenance metrics get the
  store label.

### Dependencies

- SNEP-237 artifact addressing: bound content, `storeID`, `ArtifactTarget`,
  and `PublishedArtifact`, and images that accept a per-request
  `StorageBackend`.
- Direct restore of a bound snapshot on a PVC store, in the agent and in the
  protocol to PageBroker. Until it exists, a restore from a PVC store is
  staged. It therefore comes before binding becomes the default for PVC.
- Workload identity support in PageBroker and in the cleanup of the operator,
  for stores without a Secret: the default credential chain, tracked in issue
  988, and refreshable credentials from a projected service-account token,
  tracked in issue 987.

### Test Plan

- **API:** CEL rejects two store blocks, no store block, a move between
  blocks, a location change, a name over 63 characters, an `artifacts` or
  `.claims` path segment, an `http://` endpoint without `allowHTTP`, an
  `addressing` value outside the enum, and each `api/storage` limit. An update
  that drops an empty `prefix` is accepted. `subPath` defaults to `/`. A
  reference without `key` gets the default key.
- **Operator:** Ready and NotReady for each reason. Digest detection,
  including two stores created at the same time. The access check runs inside
  `artifacts/`, including with credentials that can write but not read, list,
  or delete. Nested stores are accepted. A Ready store changes to
  `Ready=False` when its Secret is revoked or deleted. A store on the default
  claim with a `subPath` beside `.claims/` is accepted. An S3 store whose
  addressing or CA bundle differs from the startup settings is
  `RestoreSettingsMismatch`, in an S3-default and in a PVC-default
  installation, and becomes Ready when the settings change. With binding off,
  the default store is created, and a request that names another store fails
  with `StoreNotReady`. `.origin` is written at binding and for content bound
  earlier. Default resolution from the configured name, with the named store
  missing, and after the name changes. The default store is created only when
  no store exists, and `StoreValuesDiffer` fires when the values move. A job
  that names a missing store waits and proceeds when the store appears.
  Terminating store, finalizer, `Retain`, and registry rebuild. A
  `DuplicateStore` store is deletable while legacy content exists. The
  agent's Role lists only referenced Secrets and ConfigMaps, and has no Secret
  rule when no store references one.
- **Chart:** a change of only `storage.defaultStoreName` changes the pod
  template of the operator and leaves the pod template of the agent DaemonSet
  unchanged. A listed claim that is missing or not bound fails the upgrade. A
  claim that cannot mount stops the agent rollout at the first pod. A list
  that contains `storage.pvc.name` fails the render.
- **Agent:** Secret fetch per transaction. Credentials are in the request and
  never on a volume or in a log. Empty credentials for a store without a
  Secret. Mount mapping for the default claim and for listed claims below
  `.claims/`. The directory of a listed claim with nothing mounted on it is
  refused, and a store with a `subPath` below a mounted claim passes. A bound
  snapshot restores directly from the single claim and from a listed claim,
  staged with `pageBroker.restoreMode: staged`, and always staged from S3.
  `StoreUnavailable`, and its retry once implemented, on a missing store, a
  `status.storeID` mismatch, or a claim with no mount.
- **Maintenance:** a sweep looks only inside `artifacts/` and deletes only
  UUID-named orphans whose `.origin` names this cluster, on S3 and on a PVC.
  Directories with the `.origin` of another cluster, with no `.origin`, or
  with `.retained` survive. A publication that lands after cleanup leaves a
  directory with `.origin` that the next sweep removes. A content deletion
  under `Delete` keeps a directory with `.retained`, and skips a directory
  that names another cluster. The data of a nested store survives the sweep
  of its parent.
- **PageBroker:** per-request `StorageBackend`, client cache, and two stores
  live. `.origin` is written before the first object and never overwritten.
  Credentials from the request are not written to disk or to a transaction
  record. Restores from stores that differ in endpoint, region, or
  credentials queue and run in turn. Stores that share them restore in
  parallel. With a store that has no Secret, an upload and a restore each run
  past one token lifetime.
- **End to end:** two S3 stores and one PVC store in one cluster, with a
  checkpoint and a cross-node restore through each. A store terminates only
  after its content is gone. Old content stays put when the default moves. A
  rolling upgrade with an old agent fails closed.

### Graduation Criteria

- **Alpha:** CRD and fields as above; PVC and S3 stores; default store from
  the storage configuration of the operator; protection finalizer; `Retain`;
  shared stores; PVC mount limitation documented. Alpha needs store binding in
  the operator and artifact addressing in PageBroker turned on. Both are off
  by default; see [Configuration](#configuration). Turning them on by default
  is planned together with their removal.

## Alternatives

- **PageBroker with its own Kubernetes client.** Keeps credentials out of the
  agent, but adds an API dependency, RBAC, and a watch to a component that is
  driven over a local socket. Rejected.
- **Credential files on the volume the agent shares with PageBroker.** Keeps
  secrets out of the request, but adds a file contract between the two
  components, with its own permissions and cleanup. Rejected. The request
  already carries the store.
- **Mounting arbitrary PVCs at runtime.** Needs the agent to mount claims, or
  a restart per claim. Deferred, and recorded as the limitation.
- **A default marker on the store (`isDefault`).** Lets the default move with
  one edit and no restart. Any number of stores can carry the marker, so it
  needs an admission policy and a tie-break rule in the operator. A configured
  name cannot conflict. Rejected.
- **A marker file that reserves a location for one store.** Stops a second
  cluster from registering the location, which is the opposite of sharing it,
  and needs create-only writes in the storage. Recording the cluster on each
  content does the protecting instead. Rejected.
- **Reference tracking for snapshots used by several clusters.** Each cluster
  that uses a snapshot records itself, and the directory is deleted with the
  last record. Belongs with restoring the snapshot of another cluster.
  Deferred. Until then, `Retain` in the creating cluster keeps such a
  snapshot.
- **Parallel restore from several stores.** Needs a restore worker per store
  connection, or a change in the native engine. Deferred, with the session
  queue as the interim.
