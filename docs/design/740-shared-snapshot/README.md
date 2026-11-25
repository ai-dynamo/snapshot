<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-740: Share Snapshots Across Namespaces and Clusters

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: Several Namespaces Restore One Snapshot](#story-1-several-namespaces-restore-one-snapshot)
    - [Story 2: A Snapshot Is Promoted From Development to Production](#story-2-a-snapshot-is-promoted-from-development-to-production)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [Binding a PodSnapshot to a Shared Snapshot](#binding-a-podsnapshot-to-a-shared-snapshot)
  - [Which PodSnapshots a Content Lists](#which-podsnapshots-a-content-lists)
  - [Sharing a Snapshot Within a Cluster](#sharing-a-snapshot-within-a-cluster)
  - [Importing a Snapshot From Another Cluster](#importing-a-snapshot-from-another-cluster)
  - [Restore](#restore)
  - [Lifetime and Deletion](#lifetime-and-deletion)
  - [API](#api)
  - [Security](#security)
  - [Performance and Scalability](#performance-and-scalability)
  - [Monitoring](#monitoring)
  - [Dependencies](#dependencies)
  - [Upgrade and Version Skew](#upgrade-and-version-skew)
  - [Test Plan](#test-plan)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

A snapshot can be restored outside the namespace and the cluster that took it,
without checkpointing the workload again and without copying its data. A cluster
admin creates a `SharedSnapshot` that names one snapshot and the namespaces that
can use it. A user in one of those namespaces creates a `PodSnapshot` that
refers to the `SharedSnapshot` and restores from it the same way as from any
other `PodSnapshot`. The same object covers a snapshot taken in another
namespace of the cluster and a snapshot taken in another cluster that uses the
same snapshot store.

## Motivation

A checkpoint is expensive to reach. Before the node agent can checkpoint a
container, the workload has pulled its image, loaded its model into GPU memory,
and become ready. Today the result can only be restored in the namespace and the
cluster that took it:

- The `nvidia.com/restore-from` annotation names a `PodSnapshot` in the restore
  Pod's own namespace.
- A `PodSnapshot` is bound to exactly one `PodSnapshotContent`, and the content
  accepts only that `PodSnapshot`.
- A cluster has no object that describes a snapshot it did not take.

A team that wants to start the same workload in several namespaces has to
checkpoint it in each one, and store the data once per namespace. A team that
tests a snapshot in a development cluster cannot promote that snapshot to
production. It has to checkpoint again in production and hope the result behaves
like the one it tested.

### Goals

1. A namespace can restore a snapshot that was checkpointed in another namespace
   of the same cluster.
2. A cluster can restore a snapshot that another cluster checkpointed, when both
   clusters use the same snapshot store.
3. An admin decides which namespaces can use a snapshot, with a permission that
   is separate from the permission to create a `PodSnapshot`. One approval can
   cover several namespaces.
4. A restore Pod is written the same way as today. The restore Pod contract does
   not change.
5. A namespace that was allowed to use a snapshot can still restore from it
   after the source namespace or its `PodSnapshot` is deleted.
6. The snapshot's data is stored once, and is removed when its last user in the
   cluster is gone.

### Non-Goals

1. **Checkpointing a pod in another namespace.** A `PodSnapshot` still
   checkpoints a pod in its own namespace.
2. **Sharing by users.** In this proposal only an admin shares a snapshot. A
   way for a user to share their own snapshot is a possible later addition, see
   [Alternatives](#alternatives).
3. **Revoking access that is already in use.** Narrowing or deleting a
   `SharedSnapshot` stops new use. It does not detach a `PodSnapshot` that is
   already bound.
4. **Copying snapshot data between stores.** Both clusters must reach the same
   snapshot store. Moving data from one store to another is outside this
   proposal.
5. **Deciding retention across clusters.** Keeping data that another cluster
   still uses relies on the retain setting of SNEP-248, see
   [Dependencies](#dependencies).
6. **Quota or accounting** for a snapshot that several namespaces use.
7. **Changes to `SnapshotJob` and `snapshotctl`.** Both keep checkpointing a
   pod. Neither creates a `SharedSnapshot` or a `PodSnapshot` that refers to
   one.

## Proposal

A snapshot has three parts today. A `PodSnapshot` is the namespaced object a
user creates and restores from. A `PodSnapshotContent` is the cluster-scoped
record of what was stored and where, created by the operator. The artifact is
the checkpoint data in the snapshot store.

This proposal keeps one `PodSnapshotContent` for each snapshot in a cluster and
lets several `PodSnapshot` objects, in any namespace, bind to it. A new
cluster-scoped kind, `SharedSnapshot`, is the only object an admin writes. It
names one snapshot and the namespaces that can use it.

The following diagram shows how the objects relate.

```text
  SharedSnapshot            cluster-scoped, written by an admin
     |   names one snapshot and the namespaces that can use it
     v
  PodSnapshotContent        cluster-scoped, internal, one per snapshot
     ^           ^          lists every PodSnapshot bound to it
     |           |
  PodSnapshot  PodSnapshot  namespaced, one per user and namespace
  (team-a)     (team-b)
     ^
     |
  restore Pod               nvidia.com/restore-from names a PodSnapshot
```

A `SharedSnapshot` names its snapshot in one of two ways:

- **Share.** It refers to a `PodSnapshot` that exists in the cluster. The
  operator resolves that `PodSnapshot` to its content and opens the content to
  the allowed namespaces.
- **Import.** It gives the location of an artifact in the snapshot store. The
  cluster has no object for that artifact, and the operator creates the content
  for it.

In both cases a user in an allowed namespace creates a `PodSnapshot` whose
source is the `SharedSnapshot`, and not a pod to checkpoint as today. The
operator checks the namespace once, binds the `PodSnapshot` to the content, and
marks it `Ready`. From then on the `PodSnapshot` behaves like one that
checkpointed a pod.

Every `PodSnapshot` bound to a content is an equal user of it. No `PodSnapshot`
owns the snapshot. The operator deletes the content, and with it the artifact,
when no `PodSnapshot` and no `SharedSnapshot` uses it.

A restore from a shared snapshot uses the container settings saved with the
snapshot, such as `lifecycle.postRestore`. A user in another namespace does not
write them again, and every namespace restores the same way.

### User Stories

#### Story 1: Several Namespaces Restore One Snapshot

The platform team checkpoints a model server in the `training` namespace. The
serving teams work in the `serving-a` and `serving-b` namespaces. An admin
creates a `SharedSnapshot` that refers to the `PodSnapshot` in `training` and
lists the two namespaces. Each serving team creates a `PodSnapshot` in its own
namespace that names the `SharedSnapshot`, and starts restore Pods from it. The
platform team later deletes the `training` namespace. The serving teams keep
restoring.

#### Story 2: A Snapshot Is Promoted From Development to Production

A snapshot is checkpointed and tested in a development cluster. Both clusters
use the same snapshot store, and the development cluster retains the snapshot's
data. An admin of the production cluster creates a `SharedSnapshot` that gives
the artifact's location, copied from the development cluster, and the namespaces
that can use it. Users in production create a `PodSnapshot` that names the
`SharedSnapshot` and restore. The restore runs the same compatibility checks as
a restore in the development cluster.

### Limitations, Risks, and Mitigations

- **The source namespace has no say.** An admin can share a namespace's snapshot
  without that namespace's consent, and the data can outlive the `PodSnapshot`
  that took it. This is a deliberate choice, see [Security](#security).
- **`PodSnapshot` and `PodSnapshotContent` are no longer one to one.** The
  operator and the agent both assume one `PodSnapshot` for each content today.
  The affected code is listed in
  [Upgrade and Version Skew](#upgrade-and-version-skew).
- **Deleting a `PodSnapshot` no longer always deletes the data.** The data stays
  while another `PodSnapshot` or a `SharedSnapshot` uses it. A user who deletes
  their `PodSnapshot` to remove the data cannot see that it is still in use.
- **Access is checked once.** Removing a namespace from a `SharedSnapshot`, or
  deleting the `SharedSnapshot`, does not affect a `PodSnapshot` that is already
  bound. An admin removes that access by deleting the `PodSnapshot`.
- **Cluster sharing depends on SNEP-248.** Without its ownership marker and
  retain setting, the maintenance of one cluster deletes the data of another.
  Import is implemented after SNEP-248.
- **Component versions can differ between clusters.** An artifact written by one
  release is read by another, as in a restore after an upgrade in one cluster.
  The importing cluster must run the same Snapshot release as the cluster that
  took the snapshot, or a newer one.

## Design Details

The following sections use "content" for a `PodSnapshotContent`.

### Binding a PodSnapshot to a Shared Snapshot

A `PodSnapshot` whose source is `sharedSnapshotRef` does not checkpoint a pod.
The operator binds it in these steps:

1. Add the existing `PodSnapshot` finalizer.
2. Read the `SharedSnapshot`. Wait while it is not `Ready`.
3. Check the namespace of the `PodSnapshot` against `allowedNamespaces`. If it
   does not match, the `PodSnapshot` stays not `Ready` with a reason that says
   so.
4. Check that the content named in `SharedSnapshot.status.contentName` is
   `Ready` and is not being deleted.
5. Add the `PodSnapshot` to `status.boundPodSnapshots` of the content.
6. Set `status.boundSnapshotContentName` on the `PodSnapshot`.

The operator writes the content before the `PodSnapshot`. If it stops between
steps 5 and 6, the next reconcile completes step 6. The reverse order could
leave a bound `PodSnapshot` that the content does not list, and its restore
would be refused.

A `PodSnapshot` that checkpoints a pod is bound the same way as today, and the
operator also adds it to `status.boundPodSnapshots` of the content it creates.

### Which PodSnapshots a Content Lists

`status.boundPodSnapshots` is the record of which `PodSnapshot` objects can
restore from a content. The operator is the only writer. It lists a
`PodSnapshot` only when one of these holds:

- **It took the checkpoint.** It matches `spec.snapshotRef` of the content by
  namespace, name, and UID.
- **It was admitted through a `SharedSnapshot`.** Its `spec.source` names a
  `SharedSnapshot` whose `status.contentName` is this content, and its namespace
  matched `allowedNamespaces` when it was bound.

The operator decides from the `spec` of the `PodSnapshot`, which is immutable,
and never from its `status`. A user who can write the status of a `PodSnapshot`
cannot use it to enter the list.

An entry stays once it is in the list. The operator removes an entry when its
`PodSnapshot` is gone or has a different UID. It does not remove an entry when
the `SharedSnapshot` changes. If the status of a content is lost, the operator
rebuilds the list with the same two rules, using each `SharedSnapshot` as it is
at that time.

The list is flat. The `PodSnapshot` that took the checkpoint is one entry like
any other. `spec.snapshotRef` of the content keeps recording which `PodSnapshot`
asked for the checkpoint. It is a record of the past and can name a
`PodSnapshot` that no longer exists. No component uses it to decide who can
restore from the content, who is bound to it, or when it is deleted. Those
decisions read the two lists. The one remaining reader is the agent during a
checkpoint, which takes the namespace of the source pod from it. That value is
fixed when the content is created and stays correct.

### Sharing a Snapshot Within a Cluster

A `SharedSnapshot` with `source.podSnapshotRef` shares a snapshot that the
cluster already has. The operator:

1. Reads the named `PodSnapshot` and waits until it is `Ready` and bound.
2. Records the content in `SharedSnapshot.status.contentName`.
3. Adds the `SharedSnapshot` to `status.sharedSnapshots` of the content.
4. Marks the `SharedSnapshot` `Ready`.

The operator resolves the reference once. After that the `SharedSnapshot` refers
to the content and not to the `PodSnapshot`. If the `PodSnapshot` is deleted and
another is created with the same name, the `SharedSnapshot` still refers to the
first snapshot.

The named `PodSnapshot` can itself be one that was bound through another
`SharedSnapshot`. Both then refer to the same content.

Several `SharedSnapshot` objects can refer to one content, for example to open
it to different sets of namespaces. A `PodSnapshot` is checked against the one
it names.

### Importing a Snapshot From Another Cluster

A `SharedSnapshot` with `source.storage` describes an artifact that the cluster
has no object for. The admin names the store with a `SnapshotStorageClass` of
this cluster, as defined by SNEP-248, that points at the location the snapshot
was written to. The admin copies the artifact handles from
`status.storage.artifacts` of the content in the cluster that took the snapshot.
The operator:

1. Reads the named `SnapshotStorageClass` and takes the store identity from its
   status. If the class does not exist or is not `Ready`, the `SharedSnapshot`
   is not `Ready` and names the reason.
2. Looks for a content that already refers to the same store and handles. If
   one exists, it uses that content.
3. Otherwise creates a content with `spec.storage` and `spec.containers`, and
   without `spec.snapshotRef` and `spec.source`. `spec.storage` records the
   class and its store identity, as for any content. The name of the content is
   derived from the store identity and the handles, so that two imports of the
   same artifact resolve to one content.
4. Writes `status.storage` with the handles and adds the `SharedSnapshot` to
   `status.sharedSnapshots`.
5. Checks, through the store access of the `SnapshotStorageClass`, that the
   manifest of each artifact exists in the store. If one is missing, the content
   and the `SharedSnapshot` are not `Ready` and name the reason. The operator
   checks again with backoff.
6. Marks the content `Ready`, records it in `SharedSnapshot.status.contentName`,
   and marks the `SharedSnapshot` `Ready`.

`Ready` on an imported content means that the operator found the manifest of
each artifact in the store. It does not mean that the artifact can be restored
on a given node. The compatibility checks run at restore, as for any snapshot.

An imported content has no source pod and no node. The agent does not
checkpoint for it. A content that the operator created for a `SharedSnapshot`
is not released before that `SharedSnapshot` is recorded on it.

If a second `SharedSnapshot` imports an artifact that already has a content, it
uses that content only when its `containers` equal those saved on the content.
Otherwise it is not `Ready` and says that the settings differ.

Legacy content, which has no `spec.storage`, can be shared within a cluster. It
cannot be imported, because it has no artifact handle to copy.

### Restore

A restore Pod names a `PodSnapshot` in its own namespace with
`nvidia.com/restore-from`, as today. The agent reads the `PodSnapshot`, then the
content it is bound to. Two checks change:

- **The back-link.** The operator and the agent check today that
  `spec.snapshotRef` of the content equals the `PodSnapshot`. They check instead
  that `status.boundPodSnapshots` lists the `PodSnapshot` by namespace, name,
  and UID.
- **The artifact identity.** The agent checks today that the content UID
  recorded in the artifact manifest equals the UID of the content. A shared
  content is the one that took the checkpoint, and the check is unchanged. An
  imported content has a different UID than the one in the manifest, and the
  agent skips this check for it.

The compatibility checks read the artifact manifest and do not change. A restore
in another cluster is refused on an incompatible node for the same reasons as in
the cluster that took the snapshot.

The agent reads `lifecycle.postRestore` from `spec.containers` of the content,
as today. A shared content carries the settings of the `PodSnapshot` that took
the checkpoint. An imported content carries `SharedSnapshot.spec.containers`.

### Lifetime and Deletion

The operator deletes a content when both `status.boundPodSnapshots` and
`status.sharedSnapshots` are empty. Deleting the content deletes the artifact
through the existing cleanup finalizer.

- **Deleting a `PodSnapshot`.** The operator removes it from
  `status.boundPodSnapshots`. If both lists are then empty, the operator deletes
  the content and waits until it is gone, as today. Otherwise the `PodSnapshot`
  is released at once.
- **Deleting a `SharedSnapshot`.** A finalizer on the `SharedSnapshot` removes
  it from `status.sharedSnapshots`. If both lists are then empty, the operator
  deletes the content.

A `SharedSnapshot` keeps its content alive without any `PodSnapshot`. A snapshot
that was shared stays available after the `PodSnapshot` that took it is deleted,
and an imported snapshot can wait for its first user.

A new binding and the removal of the last user can happen together. Every
change to the two lists is a conditional update of the content. The operator
deletes the content with a precondition on its resource version, so a binding
that landed first makes the delete fail and the operator evaluates again. The
operator does not bind a `PodSnapshot` to a content that is being deleted.

For an imported content, the existing cleanup removes the directory named after
the UID of the content. An imported content has a new UID, and no such directory
exists. Deleting an imported content therefore removes no data. Protection
against the orphan sweep, and retention in the cluster that took the snapshot,
come from SNEP-248, see [Dependencies](#dependencies).

### API

`SharedSnapshot` is a new cluster-scoped kind in the `nvidia.com/v1alpha1`
group.

The following example shares a snapshot that exists in the cluster.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SharedSnapshot
metadata:
  name: llama-stable
spec:
  allowedNamespaces:
    names: [serving-a, serving-b]
  source:
    podSnapshotRef:
      namespace: training
      name: llama
status:
  contentName: podsnapshotcontent-0a1b2c3d
  conditions:
  - type: Ready
    status: "True"
```

The following example imports a snapshot that another cluster took. It selects
the allowed namespaces by label, which is the second field of
`allowedNamespaces`.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SharedSnapshot
metadata:
  name: llama-stable
spec:
  allowedNamespaces:
    selector:
      matchLabels:
        team: serving
  source:
    storage:
      storageClassName: shared-store
      artifacts:
      - containerName: main
        artifactHandle: <handle>
  containers:
  - name: main
    lifecycle:
      postRestore:
        httpPost:
          path: /resume
          port: 8000
```

The following table lists the fields of `SharedSnapshot`.

| Field | Description |
| --- | --- |
| `spec.allowedNamespaces` | Required. A struct with two fields, of which at least one is set. `names` is a list of namespace names, and is the basic form. `selector` is a label selector on namespaces, for a set of namespaces that changes over time. When both are set, a namespace is allowed if it matches either one. Mutable. An empty selector is rejected. |
| `spec.source` | Required, immutable. Exactly one of `podSnapshotRef` and `storage`. |
| `spec.source.podSnapshotRef` | The namespace and name of a `PodSnapshot` in this cluster. |
| `spec.source.storage` | `storageClassName` and `artifacts`. `storageClassName` names a `SnapshotStorageClass` of this cluster, and an empty value selects the default class, as in SNEP-248. `artifacts` has the same shape as `status.storage.artifacts` of a content. |
| `spec.containers` | Optional, immutable, only with `source.storage`. The container settings a restore applies, with the same shape as `PodSnapshot.spec.containers`. |
| `status.contentName` | The content this object resolves to. Written by the operator. |
| `status.conditions` | `Ready`. |

`PodSnapshot.spec.source` becomes exactly one of `podRef` and
`sharedSnapshotRef`. The following example is written by a user in an allowed
namespace.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  namespace: serving-a
  name: llama
spec:
  source:
    sharedSnapshotRef:
      name: llama-stable
```

A `PodSnapshot` with `sharedSnapshotRef` cannot set `spec.containers`. The
`spec` of a `PodSnapshot` stays immutable.

`PodSnapshotContent` changes as follows:

- `spec.snapshotRef` and `spec.source` become optional. A validation rule
  requires both for a checkpoint, and neither for an import. An import requires
  `spec.storage`.
- `status.boundPodSnapshots` is a new list of `{namespace, name, uid}`, keyed by
  `uid`, with at most 64 entries.
- `status.sharedSnapshots` is a new list of `{name, uid}`, keyed by `uid`, with
  at most 64 entries.
- The validation rules that read `spec.source.podRef.containers` are adjusted
  for a content that has no source pod.
- The print columns that show `spec.snapshotRef` are revisited, since a content
  can have several `PodSnapshot` objects or none.

The `spec` of a content stays immutable, and only the operator creates a
content. The restore Pod contract and its annotations do not change.

The two lists are separate fields and not one list with a kind on each entry.
Their entries have different shapes, and each list has its own rule for adding
and removing an entry.

### Security

**Who can share.** Creating or changing a `SharedSnapshot` gives the named
namespaces the process memory of the snapshot. A snapshot holds whatever the
source process held: model weights, request data, credentials, and service
account tokens. A process restored in another namespace starts with that memory.
The permission on `SharedSnapshot` is therefore equal to read access to the
memory of every workload that has a snapshot in the cluster. The chart grants it
only to the operator. A cluster grants it to people like a cluster-admin
permission.

**The source namespace is not asked.** An admin can share a snapshot without the
consent of the namespace that took it. A user with access to that namespace
only cannot see the `SharedSnapshot` or the content, and cannot tell that the
data is in use after the `PodSnapshot` is deleted. This is accepted. A
user-controlled way to share is a possible later addition.

**Who is allowed.** A selector in `allowedNamespaces` relies on namespace
labels. A cluster that lets tenants label their own namespaces must use only
`names`. The operator checks the namespace when it binds a `PodSnapshot`, and a
later change to the labels or to the `SharedSnapshot` does not remove that
binding.

**The binding cannot be forged.** A restore requires that the content lists the
`PodSnapshot`. Only the operator writes that list, and it decides from the
immutable `spec` of the `PodSnapshot` and from the `SharedSnapshot`. Writing the
`status` of a `PodSnapshot` does not grant access. The agent can write the
status of a content today, to record a checkpoint. It does not write the two
lists.

**The content stays internal.** No user or admin creates a content. The node
agents act on any content that has a source pod, and keeping creation with the
operator means that the permission to share is not a permission to start a
checkpoint on a node.

**The store is not chosen freely.** An import names a `SnapshotStorageClass`
that exists in the cluster. A `SharedSnapshot` cannot point the operator or the
agent at a location that the cluster is not configured with.

**What a content reveals.** A content lists the namespace and name of every
`PodSnapshot` bound to it. The chart grants access to contents only to the
operator and the agent.

### Performance and Scalability

Binding and deleting a `PodSnapshot` each update the status of one content.
Updates to one content are serialized, and concurrent ones retry. This is the
only new cost on the restore path, and it is paid when a `PodSnapshot` is
created, not when a Pod restores.

`status.boundPodSnapshots` grows with the number of `PodSnapshot` objects that
use a snapshot. An entry holds a namespace, a name, and a UID. The list holds at
most 64 entries, which bounds the size of the content and the cost of its
validation rules. A `PodSnapshot` that would be the 65th stays not `Ready`, with
a reason that says the snapshot has reached its limit, and binds when an entry
is removed. `status.sharedSnapshots` has the same limit of 64 entries, and a
`SharedSnapshot` over the limit stays not `Ready` in the same way. Raising
either limit later is a compatible change.

### Monitoring

The following status conditions and Events report the state of the feature.

| Object | Signal |
| --- | --- |
| `SharedSnapshot` | `Ready` is `False` while the named `PodSnapshot` is not ready, when the named `SnapshotStorageClass` does not exist or is not ready, when an artifact is not found in the store, when its container settings differ from the content it resolves to, or when the snapshot has reached its limit of `SharedSnapshot` objects. |
| `PodSnapshot` | `Ready` is `False` with a reason that says the namespace is not allowed, the `SharedSnapshot` does not exist, the `SharedSnapshot` is not ready, or the snapshot has reached its limit of bound `PodSnapshot` objects. |
| `PodSnapshot` | An Event when it is bound through a `SharedSnapshot`, naming the `SharedSnapshot`. |
| `PodSnapshotContent` | `status.boundPodSnapshots` and `status.sharedSnapshots` show who uses the snapshot. |

This proposal adds no metrics.

### Dependencies

**SNEP-248, `SnapshotStorageClass`.** Importing a snapshot depends on
[SNEP-248](https://github.com/ai-dynamo/snapshot/pull/889), which is under
review. This proposal uses three things from it:

- `SnapshotStorageClass`, which an import names as its store.
- The record of the creating cluster on each artifact directory, so that a
  cluster deletes and sweeps only its own data.
- The retain setting. An admin sets it in the cluster that took the snapshot, so
  that deleting the last `PodSnapshot` there keeps data that another cluster
  uses. This proposal adds no switch of its own for cleanup.

Sharing within a cluster does not depend on SNEP-248.

**Snapshot release of the importing cluster.** An import follows the same rule
as a restore after an upgrade in one cluster. The importing cluster must run the
same Snapshot release as the cluster that took the snapshot, or a newer one.
Importing into an older release is not supported.

### Upgrade and Version Skew

A content created before this change has no `status.boundPodSnapshots`. The
operator fills it on the first reconcile from `spec.snapshotRef`. Until then the
back-link check accepts `spec.snapshotRef`.

An agent from a release without this change refuses a restore from a
`PodSnapshot` that is not named in `spec.snapshotRef`. Restores in the namespace
that took the checkpoint keep working. The upgrade order is the agent, then the
operator. A `SharedSnapshot` is created only after both are upgraded.

The following code assumes one `PodSnapshot` for each content and changes:

| Component | Code | Change |
| --- | --- | --- |
| Operator | `verifyContentBacklink` | Check `status.boundPodSnapshots`. |
| Agent | Restore preflight | Check `status.boundPodSnapshots`. |
| Operator | Mapping of content events to `PodSnapshot` | Enqueue every listed `PodSnapshot`. |
| Operator | `handleDelete` of `PodSnapshot` | Delete the content only for the last user. |
| Agent | Checkpoint path | Skip a content with no source pod. |
| Agent | `validateRestoreManifest` | Skip the content UID check for an import. |

### Test Plan

**Unit.** The validation rules of the three kinds are tested for each source
variant and for the forbidden combinations. The rule that decides whether a
`PodSnapshot` is listed is table-tested, including a `PodSnapshot` with a forged
status and one in a namespace that is not allowed. The release rule is tested
for each order of removing `PodSnapshot` and `SharedSnapshot` objects, and for a
binding that arrives while the last user is removed.

**Integration.** Operator tests cover binding through a `SharedSnapshot`,
refusal for a namespace that is not allowed, narrowing a `SharedSnapshot` with a
bound `PodSnapshot`, several `SharedSnapshot` objects on one content, and the
fill of `status.boundPodSnapshots` for a content created before the change.

**End to end.** The following scenarios run in a cluster:

- A snapshot taken in one namespace is restored in another.
- The `PodSnapshot` that took the checkpoint is deleted, and the other namespace
  restores again.
- The last `PodSnapshot` and the `SharedSnapshot` are deleted, and the artifact
  is removed from the store.
- A snapshot is imported and restored. This scenario needs the store behavior of
  SNEP-248 and is added with the import work.

## Alternatives

**One content for each `PodSnapshot`, all referring to one artifact.** This
keeps the one-to-one binding and most of the current code. The operator deletes
an artifact by the UID of its content, so every copy needs to carry a second
identifier for cleanup, and the operator has to count the copies that hold an
artifact before it deletes the data. One content for each snapshot removes that
bookkeeping, and matches what a user means by "the snapshot".

**The admin creates or edits a `PodSnapshotContent`, with no new kind.** This
follows pre-provisioned `PersistentVolume` and `VolumeSnapshotContent` objects.
It was rejected for four reasons. Node agents act on a content, so permission to
create one is permission to checkpoint a pod on a node. The `spec` of a content
is immutable, and the allowed namespaces have to change. The artifact handles
live in `status`, which an admin does not write. The name of a content differs
between clusters, and a `SharedSnapshot` name can be the same in each.

**The `PodSnapshot` lists the namespaces that can use it.** This lets a user
share without an admin. It cannot describe a snapshot from another cluster,
which has no `PodSnapshot`, and it requires a mutable `PodSnapshot` `spec`. It
can be added later on top of this proposal: the operator would create a
`SharedSnapshot` from a sharing field on the `PodSnapshot`.

**A separate grant object in the source namespace.** This follows
`ReferenceGrant` in Gateway API. It covers only namespace sharing, and the grant
is deleted with the source namespace, which conflicts with goal 5.

**The `PodSnapshot` that took the checkpoint owns the snapshot.** The rule is
simple. Deleting that `PodSnapshot`, or its namespace, would remove the snapshot
for every other namespace, which conflicts with goal 5.
