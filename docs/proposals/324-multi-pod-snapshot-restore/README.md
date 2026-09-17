<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-324: Coordinated Multi-Pod Snapshot and Restore

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Checkpoint a Multi-Node Replica](#checkpoint-a-multi-node-replica)
    - [Restore a Multi-Node Replica](#restore-a-multi-node-replica)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
    - [<code>PodGroupSnapshot</code>](#podgroupsnapshot)
    - [<code>PodGroupSnapshotContent</code>](#podgroupsnapshotcontent)
    - [<code>PodGroupRestore</code>](#podgrouprestore)
  - [Group Identity and Leaf Reuse](#group-identity-and-leaf-reuse)
  - [Security](#security)
  - [Checkpoint Flow](#checkpoint-flow)
  - [Restore Flow](#restore-flow)
  - [Network Identity and Socket Restore](#network-identity-and-socket-restore)
  - [Failure and Recovery](#failure-and-recovery)
  - [Ownership and Scheduling](#ownership-and-scheduling)
  - [Initial Supported Profile](#initial-supported-profile)
  - [Normative Requirements](#normative-requirements)
  - [Configuration](#configuration)
  - [Performance and Scalability](#performance-and-scalability)
  - [Monitoring](#monitoring)
  - [Dependencies](#dependencies)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
- [Implementation History](#implementation-history)
- [Alternatives](#alternatives)
  - [Restore Every Pod Independently](#restore-every-pod-independently)
  - [Preserve the Original Pod IP Addresses](#preserve-the-original-pod-ip-addresses)
  - [Put the Protocol in a Workload Scheduler](#put-the-protocol-in-a-workload-scheduler)
  - [Let Each Inference Backend Coordinate Kubernetes Restore](#let-each-inference-backend-coordinate-kubernetes-restore)
  - [Coordinate the Group Only in Dynamo](#coordinate-the-group-only-in-dynamo)
  - [Extend <code>SnapshotJob</code> to Multiple Pods](#extend-snapshotjob-to-multiple-pods)
- [Appendix](#appendix)
  - [References](#references)
<!-- /toc -->

## Summary

This SNEP defines a Kubernetes protocol for checkpointing and restoring a fixed
group of mutually dependent Pods as one Snapshot operation. The protocol
freezes group membership, assigns stable logical member identities, fans out
the existing per-Pod checkpoint and restore operations, supplies every member
with the complete source-to-target network identity map, and reports one
aggregate group result. It does not make distributed checkpoint or restore
atomic.

## Motivation

Snapshot currently treats each Pod as an independent checkpoint and restore
target. That is insufficient for multi-node workloads whose restored process
state contains connections and identities belonging to other members of the
group, including MPI sockets, NCCL bootstrap connections, peer address tables,
and engine rank metadata.

A controlled two-node TensorRT-LLM TP=2 experiment exposed the first concrete
failure:

1. Both ranks completed NCCL checkpoint preparation and capture.
2. Restore failed during CRIU socket reconstruction, before CUDA or NCCL
   restore.
3. Each restore received only its local old-to-new Pod IP mapping, while the
   restored process state also referenced its peer's old IP.

The existing CRIU remap plugin accepts multiple address mappings, but the
current Snapshot restore path constructs only the local Pod mapping. A
multi-node restore therefore needs the complete group-level mapping at every
member.

That mapping is necessary but not sufficient. The standalone restore path
currently converts an established TCP socket whose reciprocal endpoint is
outside the local checkpoint image into an unconnected socket. A group restore
that preserves member-to-member TCP sessions must identify in-group peers,
retain those sockets, apply the complete map, and coordinate reconstruction of
both endpoints. A backend that instead rebuilds its communicator must declare
and validate that lifecycle explicitly.

Independent per-Pod execution also provides no group identity or aggregate
result. A partial capture may appear usable, and callers cannot tell whether
every member of one logical checkpoint or restore attempt succeeded. Snapshot
needs a durable group coordinator above its existing per-Pod execution paths,
while preserving the failure semantics of those paths.

The group protocol does not by itself make every communication stack
checkpoint-safe. Inference engines and communication libraries remain
responsible for the lifecycle of persistent communication resources they own.

### Goals

- Represent one immutable group checkpoint separately from any particular
  restore attempt.
- Reuse the existing per-Pod capture and restore machinery as subordinate leaf
  operations.
- Validate every member as far as possible before creating subordinate capture
  or restore work, then fan out that work without serializing members.
- Publish group success only when every mandatory leaf operation succeeds.
- Deliver the complete validated source-to-target network identity map to every
  applicable restore.
- Preserve existing per-Pod checkpoint, restore, and failure semantics.
- Make group publication, failure aggregation, retry, cancellation, and cleanup
  durable and idempotent.
- Compose with Grove or another scheduler without moving gang or topology-aware
  scheduling into Snapshot.
- Qualify an initial two-node TensorRT-LLM TP=2 workload using NCCL Socket.

### Non-Goals

- Automatic workload or group-member discovery.
- A mandatory dependency on Grove or any other workload manager.
- Cross-namespace or cross-cluster restore.
- Elastic membership or topology changes during restore.
- In-flight request migration.
- Atomic all-or-nothing checkpoint or restore, rollback to the pre-operation
  workload state, or a guarantee that the source workload remains operational
  after capture starts.
- Group-wide termination or fencing after a member failure.
- Determining whether a restored workload is healthy or ready to serve.
- RDMA reconstruction.
- NVSHMEM, MNNVL, NVLS, FlashInfer multi-node collectives, MoE, or
  expert-parallel qualification.
- Transparent support for an unqualified inference backend, transport, or
  communication-resource lifecycle.

## Proposal

Snapshot adds a group-level API above the current `PodSnapshot`,
`PodSnapshotContent`, and restore-Pod contracts. A group checkpoint has one
immutable member list and becomes Ready only when all mandatory member
artifacts are durable. Each restore attempt maps every logical member to an
existing target Pod, validates the complete mapping, starts the existing
per-Pod restore path for every member, and becomes Ready only when every target
reports `nvidia.com/Restored=True`.

The API is additive. Existing standalone APIs preserve their current
cardinality and behavior. Group-created leaf objects are owned by the group and
cannot be restored independently because they do not contain enough context to
restore a rank safely.

The workload owner remains responsible for creating source and target Pods and
for their scheduling constraints. Grove may continue to create the Pods and
provide gang scheduling, topology-aware placement, and workload membership.
Snapshot pins those Pods by UID, provides group checkpoint and restore
coordination, and aggregates the existing per-Pod outcomes.

![Component and API ownership](diagrams/component-ownership.svg)

### User Stories

#### Checkpoint a Multi-Node Replica

As a workload controller, I can identify the complete set of Pods and target
containers for one distributed replica and create one `PodGroupSnapshot`.
Snapshot validates the complete group before fanning out per-Pod captures and
publishes one reusable group checkpoint only if every member succeeds.

#### Restore a Multi-Node Replica

As a workload controller, I can gang-schedule one replacement Pod per logical
member and create a `PodGroupRestore` that maps the checkpoint's members to
those existing Pods by name and UID. Snapshot validates the complete target
group and network identity map before starting the per-Pod restores, then
reports success only if every target reports successful restoration.

### Limitations, Risks, and Mitigations

| Risk or limitation | Mitigation |
| --- | --- |
| Capture is destructive and one member may fail after another was captured. | Validate the complete group before fan-out, never publish a partial group checkpoint as Ready, expose every member outcome, and leave workload recovery to its owner. Atomicity and rollback are explicitly not guaranteed. |
| A complete IP map does not by itself preserve established cross-Pod sockets. | Distinguish in-group from external peers, retain qualified in-group connections, remap both endpoints, and fail closed when membership is ambiguous. |
| One target may restore while another fails. | Preserve each node agent's existing restore behavior, fail the aggregate group attempt, expose every member outcome, and let the workload owner recreate or repair the group. |
| A controller or node agent can restart during fan-out. | Persist operation identity, member identity, observed generation, leaf references, and per-member outcomes. Make reconciliation idempotent. |
| Grove `startsAfter` currently waits for Kubernetes Pod Ready and can prevent all restore placeholders from existing. | Omit or rewrite inter-member `startsAfter` dependencies for restore-shaped Pods, or add a Grove milestone that distinguishes placeholder availability from workload readiness. |
| Successful process restore does not imply serving readiness. | Complete `PodGroupRestore` from the existing `nvidia.com/Restored` conditions. The workload owner separately observes Pod readiness and controls serving registration. |
| Backend or transport state may not survive checkpoint and restore. | Publish an explicit capability profile and qualify backend/transport combinations separately. |

## Design Details

### API

The API adds three resources in `nvidia.com/v1alpha1`. The field names below
are the proposed contract; implementation review may refine individual names
without collapsing the separation between checkpoint artifact and restore
attempt.

#### `PodGroupSnapshot`

`PodGroupSnapshot` is a namespaced capture request and binding. Its immutable
specification contains the complete source group:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodGroupSnapshot
metadata:
  name: trtllm-replica-a
spec:
  members:
    - id: rank-0
      source:
        podRef:
          name: trtllm-rank-0
          uid: "<source-pod-uid>"
          containers: [engine]
    - id: rank-1
      source:
        podRef:
          name: trtllm-rank-1
          uid: "<source-pod-uid>"
          containers: [engine]
```

Its status contains:

- one subordinate `PodSnapshot` reference and local phase per logical member;
- member-scoped errors with stable reason codes;
- the bound `PodGroupSnapshotContent` name; and
- group-level `Ready` and `Failed` conditions.

The source Pod UID is frozen before preparation so a same-named replacement
cannot be captured accidentally.

#### `PodGroupSnapshotContent`

`PodGroupSnapshotContent` is the cluster-scoped, immutable artifact of record.
It contains:

- a namespace, name, and UID back-reference to its `PodGroupSnapshot`;
- the immutable logical member list;
- each member's source network identity;
- one subordinate `PodSnapshotContent` reference per member; and
- artifact, protocol, and capability versions required for restore.

The content becomes Ready through one root publication point only after every
mandatory leaf artifact is durable. Atomic publication does not require all
artifacts to be stored in one file or storage transaction.

#### `PodGroupRestore`

`PodGroupRestore` is a namespaced restore attempt. It references one immutable
group checkpoint and maps every logical member to exactly one target Pod:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodGroupRestore
metadata:
  name: trtllm-replica-a-restore-1
spec:
  snapshotRef:
    name: trtllm-replica-a
    uid: "<group-snapshot-uid>"
  targets:
    - id: rank-0
      podRef:
        name: trtllm-rank-0-restored
        uid: "<target-pod-uid>"
    - id: rank-1
      podRef:
        name: trtllm-rank-1-restored
        uid: "<target-pod-uid>"
```

The workload owner creates and schedules every target Pod before creating the
`PodGroupRestore`. The caller supplies each existing target Pod's name and UID.
The controller validates and pins those identities, then derives their network
identities; the caller does not author an unvalidated CRIU remap table.

Status contains the canonical target-specific identity map, or a durable
reference to it; durable per-member pending, restoring, restored, and error
state; and group-level `Ready` and `Failed` conditions. `Ready` means every
member reports `nvidia.com/Restored=True`; it does not mean every Pod is
Kubernetes Ready or that the workload is ready to serve. Checkpoint artifacts
and restore attempts have separate identities so one checkpoint can be
restored repeatedly.

### Group Identity and Leaf Reuse

Every member has a stable logical ID independent of Pod name, UID, IP address,
node, or creation order. A workload controller may use an opaque ID or a stable
component, replica, role, and rank tuple. The complete member list is resolved
before the operation starts; Snapshot V1 does not discover members by
understanding every possible workload-controller API.

Existing `PodSnapshot`, `PodSnapshotContent`, and restore-Pod mechanisms remain
the per-Pod execution path. Their artifacts are subordinate to the group
checkpoint and are not independently advertised as usable workload
checkpoints.

Group-created leaf objects carry the group UID and logical member ID.
Standalone restore of a group-owned leaf artifact is rejected unless an
authorized group restore supplies the complete group context.

`SnapshotJob` remains a single-Pod, capture-only convenience API that creates
and owns a Kubernetes Job. It does not become the group coordinator because
the workload owner, including Dynamo or Grove, owns creation of multi-node
source and target Pods.

### Security

This proposal does not reduce the privileges required by Snapshot. Node agents
still perform privileged CRIU, container-runtime, mount, and CUDA operations,
and checkpoint artifacts can contain process memory, credentials, and other
workload secrets. Deployment policies and existing mechanisms for artifact
encryption, access control, retention, and node isolation continue to apply to
every subordinate artifact and to the group manifest.

The group APIs add several authorization and isolation requirements:

- V1 accepts source and target Pods only from the namespaced group object's
  namespace. A caller cannot use a group operation to capture or restore a Pod
  in another namespace.
- Every Pod, group checkpoint, restore attempt, and subordinate artifact
  reference includes a UID where applicable. Reconcilers reject a same-named
  object recreated with a different UID.
- Creating a `PodGroupRestore` does not grant access to its referenced
  `PodGroupSnapshotContent` or leaf artifacts. The controller and node agents
  enforce the same RBAC and storage authorization as standalone restore.
- Group-owned leaf artifacts reject standalone restore so a caller cannot
  bypass the group mapping or compatibility checks.
- The controller validates membership cardinality, uniqueness, and supported
  size before dispatching node work to limit resource-exhaustion and oversized
  API-object attacks.

Group status exposes member identity, Pod references, failure reasons, and the
network remap or a reference to it. RBAC must treat these fields as workload
metadata and restrict them consistently with existing Snapshot resources.
Logs and metrics must not expose checkpoint contents, credentials, or
unbounded identity values.

### Checkpoint Flow

A group checkpoint proceeds as follows:

1. The caller supplies the complete source member list.
2. Snapshot freezes membership and validates every controller-observable,
   non-destructive condition for the complete group, including Pod identity,
   readiness, node-agent availability, artifact storage, and declared
   capabilities.
3. If group validation succeeds, Snapshot creates subordinate per-Pod snapshot
   requests approximately concurrently. The existing per-Pod controller and
   node-agent paths perform their own validation and destructive capture.
4. Snapshot waits for every mandatory leaf and artifact to succeed.
5. If every leaf succeeds, Snapshot publishes the group checkpoint record as
   Ready. If any leaf fails, Snapshot marks the group Failed and does not
   publish a usable group checkpoint.

The workload must complete any collective application quiescence or
communicator preparation before its Pods become snapshot-ready. Snapshot's
group validation reduces predictable partial failure but cannot prove that
every CRIU or CUDA capture will succeed.

Checkpoint capture is destructive: Snapshot terminates the captured source
process and does not publish a `snapshot-complete` sentinel. Group publication
controls artifact visibility, not source-process lifetime. After leaf creation,
the operation has the same failure semantics as independent single-Pod
captures: some source processes may be terminated even when the group fails,
and Snapshot does not roll them back or fence their replacements.

![Group checkpoint sequence](diagrams/checkpoint-sequence.svg)

### Restore Flow

A group restore proceeds as follows:

1. Snapshot loads the complete group checkpoint record.
2. The workload owner asks Grove or another workload manager to create and
   schedule one restore-shaped target Pod for every logical member.
3. After every target Pod exists, the caller creates `PodGroupRestore` with the
   exact Pod names and UIDs.
4. Snapshot waits until all targets are scheduled and their placeholders are
   available, validates the one-to-one mapping, and constructs the complete
   source-to-target network identity map.
5. Snapshot creates or activates durable per-member restore work approximately
   concurrently. Each node agent performs the existing compatibility checks,
   CRIU and CUDA restore, writes `restore-complete`, and reports the existing
   `nvidia.com/Restored` condition.
6. If every required target reports `nvidia.com/Restored=True`, Snapshot marks
   `PodGroupRestore` Ready. If any target reports a terminal restore failure,
   Snapshot marks the group Failed and retains every member outcome.
7. The workload owner separately observes Kubernetes Pod readiness and decides
   when the restored group may receive serving traffic.

![Group restore sequence](diagrams/restore-sequence.svg)

The durable member phases are:

```text
Pending -> Restoring -> Restored
```

![PodGroupRestore state machine](diagrams/restore-state.svg)

Group restore deliberately retains the standalone restore completion semantic.
The node agent writes `restore-complete` and reports
`nvidia.com/Restored=True` as soon as its local restore succeeds. Snapshot does
not redefine this condition to mean application health or serving readiness,
and `PodGroupRestore` does not wait for Kubernetes Pod Ready.

### Network Identity and Socket Restore

For the initial established-TCP implementation, the identity map contains
every changed source Pod IPv4 address and its corresponding target Pod IPv4
address. Every applicable CRIU restore receives the same complete map so both
local and peer endpoints can be rewritten.

The preserved-session path does not apply the standalone external-peer
disconnection behavior to an established socket whose remote endpoint belongs
to the same group. Socket retention and remapping fail closed when the peer
cannot be resolved uniquely to a member. If a backend chooses communicator
reconstruction instead, the checkpoint records that capability and the path is
qualified independently.

The immutable group checkpoint records each source identity. The complete
target-specific map is a restore work order and belongs to
`PodGroupRestore`, or to a durable leaf work object derived from it. Pod
annotations are not the source of truth for the map or aggregate member state.

Missing, duplicate, ambiguous, or unsupported mappings fail restore before
process reconstruction begins.

### Failure and Recovery

A failure in any mandatory member fails the group operation.

If checkpoint group validation fails before leaf creation, no destructive
capture has started. After leaf creation, every member follows the existing
single-Pod checkpoint semantics. A failed group may therefore contain both a
member whose source process was terminated and a member whose source remains
running. Snapshot publishes no Ready group checkpoint, exposes every leaf
outcome, and does not promise rollback or group-wide fencing. The workload
owner decides how to recover or recreate the source workload.

Restore failure also preserves the existing per-Pod semantics. An incompatible
target leaves its placeholder running and reports `RestoreIncompatible`. An
execution failure follows the node agent's existing fail-closed behavior for
that target and reports `RestoreFailed`. A successfully restored sibling stays
`nvidia.com/Restored=True`; Snapshot does not delete, restart, terminate, or
fence it solely because another member failed. `PodGroupRestore` reports
Failed with every member outcome, and the workload owner decides how to repair
or recreate the distributed replica.

Group operation state survives controller restart. Reconciliation does not
duplicate leaf creation or already completed per-Pod work. Operation identity,
member identity, observed generation, leaf references, and durable per-member
outcomes fence repeated work. Cancellation or deletion stops work that has not
started and records outstanding artifact cleanup, but it cannot undo a capture
or restore that crossed its existing per-Pod destructive boundary.

### Ownership and Scheduling

The workload owner or caller owns:

- source and target Pod creation;
- group membership and logical role or rank assignment;
- placement and scheduling requirements; and
- observing Pod readiness, recovering failed workloads, and deciding when a
  restored group may receive serving traffic.

Snapshot owns:

- frozen operation membership and identity;
- the group checkpoint and restore-attempt identities;
- network identity remapping;
- leaf fan-out, aggregate publication, failure reporting, and artifact cleanup;
  and
- the final group result.

Node agents and leaf execution paths own node-local runtime interaction, CRIU
and CUDA execution, and per-member status and artifacts. Inference engines and
communication libraries own application quiescence, persistent
communication-resource lifecycle, local checkpoint and restore ordering, and
post-restore communicator validation.

Grove or another workload manager creates the source and target Pods and owns
their gang scheduling and topology-aware placement. The caller creates
`PodGroupRestore` only after every target Pod exists, including its UID.
Snapshot does not duplicate those responsibilities. Restore adds one
constraint: every mandatory target container must reach placeholder-available
state before Snapshot starts group-owned per-Pod restore work.

Grove `startsAfter` currently waits for the prerequisite clique's Pods to
become Kubernetes Ready. This can create a cycle when a dependent rank is not
created until another clique becomes Ready, while that clique cannot restore
through the group path until every target placeholder exists. For V1, the
workload owner may omit or rewrite inter-member
`startsAfter` dependencies for restore-shaped Pods while preserving gang and
topology constraints. Alternatively, Grove may add a restore-aware milestone
that distinguishes placeholder availability from workload readiness.

![Grove startup dependency considerations](diagrams/grove-scheduling.svg)

No Grove API change is required for membership, gang scheduling, or topology
placement. A Grove enhancement is required only if its existing API cannot
express the placeholder-availability behavior needed before group restore
activation.

### Initial Supported Profile

The first qualified profile is:

- one TensorRT-LLM replica;
- dense TP=2;
- two Pods on two nodes;
- one GPU per Pod;
- one Kubernetes namespace and cluster;
- no in-flight requests;
- checkpoint after engine initialization and before serving registration;
- NCCL Socket transport;
- a readiness probe that validates TensorRT-LLM and communicator health;
- artifact storage accessible from all source and target nodes; and
- restore may relocate either member and assign new Pod IPs.

CUDA graphs are disabled unless the backend demonstrates that every
graph-visible communicator and allocation remains valid across the selected
checkpoint and restore lifecycle. Enabling CUDA graphs is a separate
qualification dimension, not an implied property of NCCL Socket support.

Passing this profile does not imply support for other NCCL transports or
communication-resource paths.

### Normative Requirements

The key words **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, and **MAY**
are interpreted as described in [RFC 2119].

1. A multi-Pod checkpoint **MUST** have one group operation identity and one
   durable group checkpoint identity.
2. The complete member list **MUST** be resolved and frozen before
   state-changing preparation begins.
3. Every member **MUST** have a stable logical identity independent of Pod name,
   UID, IP address, node, and creation order.
4. Source and target Pods **MUST** map one-to-one through logical member
   identity.
5. Group checkpoint publication **MUST** require every mandatory member and
   artifact.
6. Per-member success **MUST NOT** be exposed as group success.
7. Snapshot **MUST** fan out subordinate member operations without
   intentionally serializing one member behind another.
8. Snapshot **MUST NOT** claim atomic checkpoint, atomic restore, or rollback
   of work that has crossed a per-Pod destructive boundary.
9. Every applicable CRIU restore **MUST** receive the complete source-to-target
   network identity map.
10. Missing or unsupported identity mappings **MUST** fail before process
    reconstruction.
11. Controller-observable group identity, readiness, node-agent, storage,
    backend, and transport checks **MUST** be evaluated for every member before
    leaf creation. Each leaf **MUST** retain its existing node-local validation.
12. A mandatory member failure **MUST** fail the group operation.
13. A group failure **MUST NOT** cause Snapshot to terminate, restart, delete,
    or fence a successful sibling solely because another member failed.
14. Group state and reconciliation **MUST** survive controller restart and
    remain idempotent.
15. Cancellation or deletion **MUST** stop work that has not started and record
    outstanding cleanup, but **MUST NOT** claim to roll back completed leaf
    work.
16. The protocol **MUST NOT** depend on a particular workload controller or
    scheduler.
17. V1 **MUST** remain within one Kubernetes namespace and cluster.
18. Broader transport and topology support **MUST** be qualified separately
    before being advertised.
19. Checkpoint artifacts and restore attempts **MUST** have separate identities
    so one checkpoint can be restored repeatedly.
20. A group-owned leaf artifact **MUST NOT** be restored outside an authorized
    group restore context.
21. Snapshot **MUST NOT** create subordinate capture work until the complete
    member list passes controller-level group validation.
22. Each target **MUST** retain the standalone restore completion semantic:
    its node agent writes `restore-complete` and reports
    `nvidia.com/Restored=True` when its local restore succeeds.
23. Restore target startup dependencies **MUST** allow every mandatory target
    container to reach placeholder-available state before group restore
    activation.
24. Gang and topology-aware scheduling **MAY** be provided by Grove or another
    workload manager and **MUST NOT** be reimplemented by Snapshot.
25. The complete target-specific identity map **MUST** be represented by
    durable Snapshot state.
26. Repeated reconciliation **MUST NOT** duplicate leaf creation or already
    completed per-Pod work.
27. Snapshot **MUST** expose an additive durable API for the group checkpoint
    artifact and each restore attempt.
28. The group restore API **MUST** reference one immutable group checkpoint and
    map every logical member to exactly one target Pod.
29. The group API **MUST NOT** change `PodSnapshot` or `SnapshotJob` into
    multi-Pod workload owners.
30. Every target Pod **MUST** exist with a pinned UID, and the complete
    identity map **MUST** be valid, before Snapshot activates any group-owned
    per-Pod restore.
31. A preserved-session restore **MUST** distinguish in-group TCP peers from
    external peers and **MUST NOT** apply the standalone external-peer
    disconnection policy to a qualified in-group connection.
32. `PodGroupRestore` **MUST** become Ready from successful per-Pod
    `nvidia.com/Restored` outcomes and **MUST NOT** depend on Kubernetes Pod
    Ready or serving readiness.

### Configuration

This proposal introduces no new user-facing runtime configuration and does not
choose a feature-gate name or default. Whether an alpha implementation needs a
feature gate is an implementation and release decision. Installing or enabling
the group APIs does not change standalone `PodSnapshot` or `SnapshotJob`
behavior.

The initial supported profile uses the existing Snapshot artifact-storage,
CRIU, CUDA checkpoint, and restore compatibility configuration. Backend and
transport capability are recorded in the checkpoint rather than selected by an
out-of-band annotation. The initial qualification covers exactly two members;
a general maximum group size remains an implementation decision informed by
the scalability tests below.

### Performance and Scalability

Group validation, capture, and restore fan out across members and must not be
intentionally serialized by the group controller. Completion time is therefore
bounded primarily by the slowest mandatory member, while artifact bytes remain
the sum of the existing per-Pod artifacts plus a small group manifest.

API object size and controller work grow linearly with member count. A complete
network identity map has linear size and is delivered to every applicable
member, producing quadratic aggregate distribution work if every member needs
every mapping. The initial two-member profile does not establish a general
scale limit. Tests must measure reconciliation latency, API-object size,
controller memory, identity-map distribution, and cleanup time at increasing
group sizes before beta limits are selected.

The implementation must avoid unbounded fan-out that can starve unrelated
standalone or group operations. The precise concurrency policy is selected
during implementation. Metrics and logs avoid member identity labels whose
cardinality grows with group size.

### Monitoring

The group resources expose Kubernetes conditions and member status sufficient
to answer which phase is active, which leaf is still pending, whether a failure
is terminal, and what cleanup remains. Conditions use stable reason codes and
include observed generation and transition time.

The operator emits Kubernetes Events for group acceptance, validation failure,
leaf creation, checkpoint publication, restore completion, group failure,
cancellation, and cleanup failure.

Metrics cover operation count, duration, and failure count by operation type,
phase, result, and stable reason. Member IDs, Pod names, UIDs, IP addresses, and
artifact paths are excluded from metric labels to avoid unbounded cardinality.
Logs carry the group UID, restore-attempt UID, member ID, and leaf operation UID
as structured correlation fields.

### Dependencies

- Existing Snapshot `PodSnapshot`, `PodSnapshotContent`, restore-Pod, CRIU,
  CUDA checkpoint, compatibility-preflight, and artifact-storage paths.
- A backend and communicator lifecycle qualified for checkpoint and restore.
- A workload owner capable of creating the complete source and target Pod set.
- A scheduler or workload manager capable of placing the target group. Grove is
  optional.
- A placeholder-startup policy that does not wait for restored workload
  readiness before all required targets exist.

### Test Plan

Unit tests cover API validation, immutable membership, one-to-one target
mapping, leaf ownership, phase transitions, idempotent leaf creation, cleanup,
and source-to-target identity-map construction.

Controller integration tests cover:

- failure before and after the destructive checkpoint boundary;
- one incompatible target while another target restores successfully;
- one member completing before another fails;
- controller restart during leaf fan-out and result aggregation;
- node-agent retry without duplicate capture or restore;
- rejection of standalone restore from a group-owned leaf;
- cancellation and deletion during each nonterminal phase; and
- confirmation that a member failure does not cause group-wide Pod fencing or
  deletion.

End-to-end qualification uses the initial supported profile and verifies:

1. cold initialization and inference;
2. checkpoint of both members;
3. restore onto newly scheduled Pods;
4. both-changed, one-changed, and unchanged Pod-IP cases;
5. delivery of the complete identity map to both members;
6. preservation of qualified in-group TCP sessions without standalone
   external-peer disconnection;
7. successful process, CUDA, and communicator restoration;
8. correct post-restore inference;
9. repeated restore from the same checkpoint;
10. injected single-member failure, exact per-member status, and workload-owner
    recovery; and
11. a Grove-managed restore with placeholder-safe `startsAfter` handling.

### Graduation Criteria

Alpha requires the three group resources in `v1alpha1`, the initial supported
profile, controller and node-agent restart coverage, failure-injection
coverage, and end-to-end evidence for repeated restore and Pod-IP relocation.
Documentation must state the exact qualified backend, transport, CUDA-graph,
namespace, and cluster boundaries.

Beta requires operational evidence for the declared supported profiles,
upgrade and downgrade behavior, published metrics and runbooks, and no known
path that exposes a partial group as Ready.

GA requires a stable API version, documented compatibility and storage
policies, scale and longevity testing, conformance coverage for supported
profiles, and a migration plan from the alpha and beta APIs.

## Implementation History

- 2026-09: Initial SNEP drafted from the multi-node Snapshot investigation.

## Alternatives

### Restore Every Pod Independently

Independent restore cannot construct a complete peer identity map or provide
one durable group result. A partial group may appear successful even though a
required member failed.

### Preserve the Original Pod IP Addresses

Preserving IPs constrains scheduling and relies on networking behavior that
Kubernetes does not generally guarantee. It also does not address group
publication, cleanup, or non-network peer identities.

### Put the Protocol in a Workload Scheduler

A scheduler such as Grove can provide membership and placement, but it should
not own CRIU and CUDA ordering, backend lifecycle, artifact publication, or
Snapshot cleanup policy.

### Let Each Inference Backend Coordinate Kubernetes Restore

This duplicates Kubernetes orchestration, identity, failure semantics, and
cleanup across TensorRT-LLM, vLLM, and SGLang. Backends should own their
communication-resource lifecycle while Snapshot owns the outer Kubernetes
operation.

### Coordinate the Group Only in Dynamo

Dynamo could aggregate multiple leaf snapshots for an integration-specific
prototype. That leaves Snapshot unable to represent the real artifact
boundary, requires callers to reproduce result aggregation and cleanup
semantics, and makes complete remap delivery an out-of-band contract. The
generic group mechanics belong in Snapshot while Dynamo remains a
workload-specific caller.

### Extend `SnapshotJob` to Multiple Pods

`SnapshotJob` owns creation of one capture Job and is intentionally
capture-only. Extending it to own a multi-Pod workload conflicts with Dynamo or
Grove ownership of membership, rank assignment, gang scheduling, and topology
placement. The group API instead composes caller-owned Pods and existing
per-Pod artifacts.

## Appendix

### References

- [RFC 2119]
- [Dynamo DEP #13220: Composable container checkpoint/restore and CUDA data
  plane](https://github.com/ai-dynamo/dynamo/issues/13220)
- [Dynamo PR #12961: Stable CUDA launch-job
  identity](https://github.com/ai-dynamo/dynamo/pull/12961)
- [Dynamo PR #2269: Grove multi-node
  support](https://github.com/ai-dynamo/dynamo/pull/2269)
- [Dynamo PR #2405: Grove deployment type
  integration](https://github.com/ai-dynamo/dynamo/pull/2405)
- [Snapshot API types](https://github.com/ai-dynamo/snapshot/tree/main/api/v1alpha1)
- [Snapshot workload
  contract](https://github.com/ai-dynamo/snapshot/blob/main/docs/reference/workload-contract.md)
- [Snapshot restore-Pod
  contract](https://github.com/ai-dynamo/snapshot/blob/main/docs/reference/restore-pod-contract.md)
- [Snapshot per-Pod INET
  remap](https://github.com/ai-dynamo/snapshot/blob/main/agent/internal/criu/inet_remap.go)
- [Snapshot CRIU INET remap
  plugin](https://github.com/ai-dynamo/snapshot/blob/main/agent/plugins/inet-remap/snapshot_inet_remap.c)
- [Snapshot socket restore
  handling](https://github.com/ai-dynamo/snapshot/blob/main/agent/internal/criu/restore_images.go)
- [Snapshot PR #140: Restore compatibility
  checks](https://github.com/ai-dynamo/snapshot/pull/140)
- [Snapshot issue #295: Checkpointing same-node CUDA peer
  mappings](https://github.com/ai-dynamo/snapshot/issues/295)
- [Snapshot issue #294: Cross-node relocation coverage for one multi-GPU
  Pod](https://github.com/ai-dynamo/snapshot/issues/294)
- [Grove `startsAfter`
  API](https://github.com/ai-dynamo/grove/blob/main/operator/api/core/v1alpha1/podclique.go)
- [Grove `startsAfter` readiness
  implementation](https://github.com/ai-dynamo/grove/blob/main/operator/initc/internal/wait.go)
- [Grove](https://github.com/ai-dynamo/grove)

[RFC 2119]: https://www.rfc-editor.org/rfc/rfc2119
