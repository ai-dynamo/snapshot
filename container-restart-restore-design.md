<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore replacement containers after a successful restore

Working architecture notes, consolidated on November 7, 2026. This records the
design we converged on and the remaining decisions. It is input to the future
SNEP, rather than documentation of a released capability. No implementation is
included here.

The behavioral baseline is Snapshot main at
[`2e263f0`](https://github.com/ai-dynamo/snapshot/commit/2e263f0eefb69a30483c0517a835a2cbd8c78372).
The design is derived from the required behavior. Existing
[PR #174](https://github.com/ai-dynamo/snapshot/pull/174) remains material for a
later comparison, not the specification for these notes.

## Purpose and scope

Dynamo can restore two inference engines into one serving Pod. One serves
requests; the other waits as a shadow engine. If the active engine fails, the
survivor takes over. We then need to restore the replacement container so that
the Pod has a standby again.

Today, Snapshot treats a successfully restored Pod as finished. A later
container incarnation does not get another restore. Recreating the whole Pod
would also kill the healthy engine, defeating the purpose of the survivor.

The proposed capability restores an eligible replacement **inside the same
Pod**, using the same saved snapshot and preserving the surviving engine.
Only a previously successful destination is eligible for replenishment. Failed
or partially successful restore cycles retain today's terminal policy.

```text
Initial state                Failure and takeover       Replenished state

engine-0 / C0: active         C0 exits                   engine-0 / C2: standby
engine-1 / C1: standby   ->   C1 becomes active     ->    engine-1 / C1: active
GMS: loaded                  GMS survives               GMS survives

Same Pod, same snapshot, same shared GPU weight backing throughout.
```

The first qualification case is a single-node, TP1 Dynamo workload. The
Snapshot behavior is expressed in terms of destination containers, without
embedding Dynamo engine roles or election policy in Snapshot.

## Progress map

```text
Ship replacement-container restoration
|
+-- 1. Understand current initial restore             [covered]
+-- 2. Quiz on the existing machinery                 [complete]
|
+-- 3. Develop the architecture                      [current]
|   +-- Attempt state and last success                [fields agreed]
|   +-- Per-incarnation control directories           [preferred; qualify mount]
|   +-- Existing controller/task split                [agreed]
|   `-- Behavior changes and tradeoffs                [captured below]
|
+-- 4. Compare with #174 and related developments
+-- 5. Consolidate into a focused SNEP
+-- 6. Refresh #174 and qualify the implementation
`-- 7. Review, walkthrough, and merge readiness
```

## Entities and their responsibilities

An **incarnation** is one runtime container ID. A container name such as
`engine-0` stays the same when kubelet replaces it; its ID changes from `C0`
to `C2`.

| Entity | Responsibility in this flow |
| --- | --- |
| DynamoGraphDeployment (DGD) and Dynamo operator | Arrange snapshot creation and the serving Pod, including the engine containers and GPU Memory Service (GMS). |
| `SnapshotJob` | Orchestrate snapshot creation. Dynamo's `WaitForCheckpoint` waits for completion, including the GMS saving work. |
| `PodSnapshot` and `PodSnapshotContent` | The named snapshot and its bound backing record. The restore Pod's `nvidia.com/restore-from` annotation names the `PodSnapshot`; its content identifies the saved snapshot being reused. |
| Snapshot node agent | Node-local daemon containing the NRI plugin, restore controller, and restore execution machinery. |
| NRI plugin | Intercept runtime container creation, install the restore proxy, and arrange its control mount. Its post-start callback supplies a runtime-ID hint to the controller. |
| Controller/coordinator | Consume a Pod queue item, decide whether a restore cycle is allowed, launch destination tasks concurrently, wait, and aggregate their results. These are responsibilities of the existing controller, not a new service. |
| Destination task | Resolve one destination container, skip or restore it, and return its outcome. One task is launched for each mapped destination in a pass. |
| Restore proxy | PID 1 inside each destination container. Wait for the agent to publish the restored application PID, then forward signals and follow application exit. |
| Control directory | Files shared by the agent, proxy, and application. `restore-complete` publishes the restored PID; `restore-failed` signals failure. |
| GMS and Dynamo engine lifecycle | GMS owns the shared GPU weights. Dynamo decides which engine wakes and serves. These are separate from Snapshot's physical process restore. |

```text
Dynamo operator / DGD
    |
    +-- SnapshotJob -> PodSnapshot -> PodSnapshotContent -> saved snapshot
    |
    `-- serving Pod, annotated with restore-from
          +-- engine-0: proxy + restored application
          +-- engine-1: proxy + restored application
          `-- GMS: shared GPU weight backing

Snapshot node agent
    +-- NRI plugin --------> destination startup and control mount
    `-- controller
          `-- one Pod pass
                +-- destination task for engine-0
                `-- destination task for engine-1
```

## What we keep from current main

The current controller already launches destination tasks concurrently, waits
for all of them, and records a Pod-level result. Different Pods can restore
concurrently. Successive passes for the same Pod are serialized by the
workqueue.

Tasks already resolve a running runtime container ID before physical restore.
That ID identifies the runtime task and its process/namespaces. A task can wait
for its destination to appear, then return pending if the bounded wait expires.

Current recovery reconstructs work from the Pod condition and control files.
For an in-progress restore with `restore-complete` present, the agent skips
physical restore and resumes the configured `postRestore` notification. The
design preserves this behavior and extends it to replenishment.

We keep this controller/task split, the existing restore sequence, and the
existing terminal failure classifications. We do not introduce a new scheduler
or a registry of independently running destination tasks.

## Walkthrough: recover the missing standby

Use Pod `demo/serve-1`, UID `P77`, and snapshot content UID `K7`. Its mapped
destinations are `engine-0` and `engine-1`.

### 1. Start from a fully successful initial restore

Both `C0` and `C1` have completed their restore workflows. The Pod's custom
`nvidia.com/Restored` condition is `True / RestoreSucceeded`.

We persist the identity and progress of each destination. The following is a
logical representation, not a committed API schema or annotation name:

```yaml
checkpointContentUID: K7
targets:
  engine-0:
    lastSuccess: C0
    attempt:
      containerID: C0
      phase: Succeeded
  engine-1:
    lastSuccess: C1
    attempt:
      containerID: C1
      phase: Succeeded
```

`attempt` records the current or most recent attempt so that another agent
process can pick it up. `lastSuccess` retains the last completed incarnation
while a successor is pending, restoring, or failed. They are updated as distinct
pieces of state. An older success does not make a newer failed attempt
eligible for retry.

Success means the restore, application release, and any configured
`postRestore` action succeeded, and the outcome was recorded. A
`restore-complete` file alone does not prove that entire workflow succeeded.

Persistence format remains open. A typed JSON Pod annotation alongside the
existing condition is a focused option. Kubernetes' built-in Pod status does
not permit arbitrary new fields. Durable Pod state is the source of truth;
the in-memory handled cache is only an optimization.

### 2. The active engine exits; the survivor takes over

`C0` exits. Dynamo lets `C1` become active. Kubelet creates replacement `C2`
under the same destination name, `engine-0`.

```text
Kubernetes runtime truth              Persisted restore history

engine-0: absent, then C2              engine-0.lastSuccess: C0
engine-1: C1                           engine-1.lastSuccess: C1
                 |
                 `-- mismatch identifies the destination needing attention
```

Pod informer updates enqueue the Pod. The existing NRI post-start notification
can also supply the new runtime ID and enqueue the same Pod key. Queueing is a
request to reconsider the Pod, not permission to restore every destination.

### 3. Give the new incarnation its own control directory

NRI's container-create hook runs for the new runtime container. It installs the
proxy and arranges an incarnation-specific view of the existing Pod control
volume:

```text
Pod emptyDir: snapshot-control/
    engine-0/
        C0/                 old incarnation's files
        C2/                 fresh directory for the replacement
    engine-1/
        C1/                 survivor's existing files

Inside C2: /snapshot-control -> engine-0/C2/
Inside C1: /snapshot-control -> engine-1/C1/
```

The proxy in `C2` sees no old success or failure marker and waits. It cannot
mistake `C0`'s published PID for its own application. NRI does not erase old
files or decide whether policy allows another restore.

The same `C2` directory survives a node-agent process restart. It must be the
directory seen by the proxy, application, optional restored startup probe,
restore helper, and agent marker writers. Host-side fallback writes also need
the exact incarnation.

NRI supports runtime mount adjustments, but this particular mount arrangement
still needs qualification, including host Pod identity translation in
vCluster. Creating a directory alone is insufficient; the actual runtime
mount must point to it.

### 4. At dequeue, allow a replenishment cycle

The controller compares current Pod state with the persisted restore history.
For a previously successful Pod, an eligible destination that is absent or has
a different runtime ID allows a new cycle.

```text
Dequeue demo/serve-1
    |
    +-- previous restore succeeded, no incarnation change -> no work
    |
    +-- previous restore succeeded, destination changed/absent
    |       -> persist False / RestoreReplenishing
    |       -> run common preflight and launch destination tasks
    |
    +-- already RestoreReplenishing -> continue that cycle
    |
    `-- failed/partial terminal outcome -> retain existing terminal policy
```

The handled cache must not bypass this comparison. The controller does not
resolve every runtime ID or build a per-destination execution plan here. That
stays with the tasks. Existing preflight and finalizer protection remain in
the flow, and the cycle state must be persisted before workers perform a
restore.

`RestoreReplenishing` distinguishes this cycle from initial restore after the
old success condition has been replaced. It must survive pending passes and
agent restarts. Destinations without a recorded successful initial restore do
not become newly eligible merely because the Pod is replenishing.

### 5. Launch the existing parallel destination tasks

```text
Controller: persist cycle state, launch tasks, then wait
    |
    +-- engine-0 task                     +-- engine-1 task
    |     resolve running destination     |     resolve C1
    |       |                             |       |
    |       +-- absent -> bounded wait    |     C1 matches successful record
    |       |     timeout -> Pending      |       |
    |       |                             |     return satisfied
    |       `-- C2 present                |     no restore, release, or hook
    |             |
    |           persist C2 attempt
    |           restore or recover C2
    |           return identified result
    |
    `-- aggregate after every task returns
```

The decisions inside each task are:

| Destination state | Task action |
| --- | --- |
| No running container ID yet | Wait using the existing resolver; return pending if its bounded wait expires. |
| Same incarnation with a successful record | Return satisfied without repeating physical restore or callbacks. |
| Same incarnation with an interrupted attempt | Resume that attempt using existing recovery behavior and its control files. |
| New incarnation allowed by policy | Persist the attempt identity, then restore it. |
| Newer failed attempt | Preserve terminal failure policy; do not use historical success to retry it. |

A physical attempt has a fixed identity:

```text
(Pod UID P77, snapshot content UID K7, destination engine-0, container ID C2)
```

Once resolved, the task does not switch to a later container incarnation.
Attempt persistence precedes physical effects. A task waiting for an ID must
not discard the evidence needed to resume a known interrupted attempt.

### 6. Restore C2 and finish its workflow

The existing executor targets runtime container `C2`, resolves its process
and namespaces, and restores the saved application into that container.

```text
engine-0 task, fixed to C2
    |
    +-- persist attempt(C2, Restoring); keep lastSuccess=C0
    |
    +-- recover existing completion, or perform physical restore
    |
    +-- publish restored PID in C2/restore-complete
    |       +-- proxy begins following the restored application
    |       `-- optional startup probe can now observe completion
    |
    +-- finish configured postRestore action, if any
    |
    `-- persist attempt(C2, Succeeded) and lastSuccess=C2
```

In the Dynamo workflow, application release does not itself mean the engine
is serving. The restored engine still follows Dynamo's election and wakeup
path. With `C1` already active, `C2` becomes the standby. GMS admission and
weight remapping remain separate from Snapshot's completion signal.

If persisting success fails after physical completion, retain completion
evidence and retry the bookkeeping. Do not replay CRIU or turn an API write
failure into a physical restore failure. Concurrent record updates must
preserve sibling entries and apply to the intended attempt.

If the restore workflow itself fails, record the attempt failure and retain
the existing failure policy. `lastSuccess=C0` remains history, not permission
to keep trying `C2` or its successors.

### 7. Aggregate without restoring the survivor

`C1` returned satisfied; `C2` returned successfully restored. The controller
records `True / RestoreSucceeded` again. The Pod now has an active engine and
a standby.

```text
Initial restore        True  / RestoreSucceeded
Replacement needed     False / RestoreReplenishing
Destination pending    False / RestoreReplenishing   -> requeue
All satisfied          True  / RestoreSucceeded
Failure/refusal        Existing failure classifications and policy
```

Initial-restore pending work continues to use `RestoreInProgress`. Pending
replenishment must not accidentally become initial restore. Aggregation keeps
the current pending, failed, partial-success, and compatibility-refusal
semantics; unchanged successful destinations count as satisfied.

As in current main, record the aggregate condition before marking the Pod
handled in memory. Finalizer cleanup follows the existing completed/terminal
cycle behavior.

### 8. Pick up after a node-agent restart

Another agent process reconstructs work from the cycle condition, destination
records, and the same incarnation's control files:

| Durable evidence | Continuation |
| --- | --- |
| Current incarnation is already recorded successful | Skip it. |
| Attempt is in progress and its `restore-complete` exists | Skip physical restore; resume the configured post-restore work and bookkeeping. |
| Attempt is in progress without completion evidence | Preserve current main's behavior and limitations. |
| Pod has a terminal failed/partial outcome | Preserve terminal policy. |

This does not resume a Go stack. It starts new tasks that interpret persisted
progress. The existing ambiguity when an agent dies during physical restore
before publishing completion remains outside this feature's expansion of
scope. We are not adding a physical-operation journal.

## Distilled behavior changes and decisions

These are consequences of a working implementation, rather than problems in
getting the implementation correct. Numbers retain the identifiers from our
earlier concerns list.

| Concern | What changes | Consequence or remaining decision |
| --- | --- | --- |
| **4. Failure enforcement** | A fresh proxy no longer inherits the previous incarnation's `restore-failed` file. The agent must explicitly reject an ineligible successor. | Failure remains terminal, but during an agent outage the new proxy may wait instead of immediately exiting. Preserve the existing compatibility-override behavior deliberately. |
| **5. Condition semantics** | `Succeeded -> Replenishing -> Succeeded` becomes a normal transition. | A healthy survivor can serve while the custom restored condition is false. Consumers must distinguish restore progress from serving readiness; audit any readiness-gate or controller assumptions. |
| **6. Activation and rollout** | A container restart can trigger another restore instead of exhausting a single restore per Pod. | Decide opt-in versus default. Older successful Pods without incarnation records retain one-shot behavior. Define the supported rollback boundary while a Pod is replenishing. |
| **7. Resource lifetime** | The saved snapshot becomes an ongoing dependency. Each incarnation also adds a control directory. | Keep the referenced snapshot/content and artifacts available for the Pod's replenishment lifetime. Reclaim retired directories after old work and recovery no longer need them. |
| **10. Repeated restore cost** | A successful restore followed by an application crash can cause another successful restore repeatedly. | Kubelet restart backoff paces the loop, but this design adds no Snapshot attempt budget or storage-bandwidth budget. |

```text
Ordinary recovery:
    restore succeeds -> application fails -> replacement restore succeeds

Persistent application problem:
    restore succeeds -> app crashes -> restore succeeds -> app crashes -> ...
```

For Dynamo, a replacement engine reuses surviving GMS weight backing. Repeated
engine restores still read process and other saved state; they do not imply
that GMS must reload its entire weight artifact on every engine restart.

### Accepted scheduling and failure tradeoffs

**A sibling discovered after its task returned waits for another Pod pass.**
Tasks in a pass are concurrent, but the coordinator waits for all of them.
For example:

```text
Pass 1
  engine-0: restoring C2 ---------------------------> finishes
  engine-1: C1 unchanged -> task returns
                              |
                         C1 exits; C3 starts
                              |
                         Pod queued for follow-up
                                                        |
Pass 2                                                  v
  engine-0: C2 unchanged -> skip                  restore C3
```

Kubelet can start `C3` immediately; its Snapshot restore can wait for pass 1.
If its existing task is still resolving the destination, it may discover the
new container within that pass. We accept this barrier to retain the current
controller/task machinery.

**Terminal restore failure remains Pod-wide.** A survivor may keep serving,
but a failed or partially successful restore cycle does not automatically
start more replenishment cycles. Changing that policy belongs to broader
retry/fallback work.

## Implementation hazards to resolve in the implementation pass

These are required correctness details, not separate product capabilities:

1. **Mount and marker consistency.** Every participant must use the same
   incarnation directory, including startup probes, helpers, host fallback
   writes, and vCluster host-identity translation. Do not erase old control
   directories in the create hook.
2. **Identity and state updates.** Preserve sibling records, distinguish
   historical success from a failed successor, and prevent old results from
   overwriting a newer attempt. Revisit the handled-cache early return.
3. **Recovery and aggregation.** Recognize replenishment as resumable,
   preserve its phase across pending/error paths, and distinguish retrying
   bookkeeping from replaying physical restore. Check compatibility-refusal
   and partial-success handling with an unchanged successful sibling.

Per-incarnation directories isolate control files. They do not make every
effect private: the Pod condition and annotation are shared, some configured
HTTP callbacks address the Pod IP, and runtime processes remain external
resources. Use exact runtime IDs and attempt-aware record updates where this
feature requires them. Do not claim that the directory change introduces
general task cancellation or fences every callback; those guarantees do not
exist in main today.

## Focused implementation surface

| Area | Required change |
| --- | --- |
| Restore bookkeeping | Persist snapshot identity, per-destination attempt, and last success; choose encoding and conflict-safe update helper. |
| Dequeue policy | Reconsider successful Pods on incarnation changes; continue replenishment; preserve terminal failure and legacy one-shot behavior. |
| Destination task | Keep runtime resolution and bounded waiting; skip satisfied destinations; bind and persist an eligible attempt; reuse restore/recovery flow. |
| NRI and control paths | Mount an incarnation-specific directory and make every relevant writer/reader resolve the same directory. |
| Conditions and aggregation | Add/recognize the replenishment phase throughout recovery, classification, and consumers without changing the fan-out-and-wait model. |
| Terminal handling | Explicitly reject fresh successors that policy does not permit restoring, since they no longer inherit failure markers. |

The design does not require changing Dynamo election, reloading surviving GMS,
recreating the Pod, retrying failed restores, cold-start fallback, a new
destination scheduler, or a generic restore-attempt journal. Preserve existing
initial-restore behavior and existing recovery guarantees; expand only where
replacement restoration requires it.

## Next steps

1. Compare this walkthrough with #174 and the related upstream work. Identify
   reusable pieces and conflicts without inheriting unrelated machinery.
2. Settle activation, persistence encoding, condition consumers, retention and
   cleanup ownership, and the supported rollback boundary in the SNEP.
3. Qualify per-incarnation runtime mounts and failure notification, then
   implement the focused controller/task changes and their tests.
4. Exercise initial restore, replacement with a serving survivor, agent
   restart during replenishment, pending IDs, and terminal failure behavior.
   Review the resulting workflow and diff before merge.

## Current-main code map

Links pin the baseline used for this discussion. They show existing machinery,
not an implementation of this proposal.

| Machinery | Source |
| --- | --- |
| Pod dequeue, handled cache, terminal decisions, and preflight | [`agent/internal/controller/controller.go:498`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/controller/controller.go#L498-L647) |
| Parallel destination fan-out and aggregation | [`agent/internal/controller/controller.go:838`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/controller/controller.go#L838-L984) |
| Destination task and runtime-ID resolution | [`agent/internal/controller/controller.go:1007`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/controller/controller.go#L1007-L1104) |
| Restore execution and completion-based recovery | [`agent/internal/controller/controller.go:1119`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/controller/controller.go#L1119-L1206) |
| Failure and completion marker writes | [`agent/internal/controller/controller.go:1255`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/controller/controller.go#L1255-L1321) |
| NRI create/start/stop hooks | [`agent/internal/nri/plugin.go:215`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/nri/plugin.go#L215-L317) |
| Proxy marker checks and application following | [`agent/internal/restoreproxy/proxy.go:62`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/restoreproxy/proxy.go#L62-L120) |
| Control mount and optional startup probe | [`api/podcontract/restore_pod.go:316`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/api/podcontract/restore_pod.go#L316-L448) |
| Host control paths and marker writes | [`agent/internal/runtime/control.go:126`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/agent/internal/runtime/control.go#L126-L175) |
| Restored-condition classification | [`api/podcontract/restore_status.go:64`](https://github.com/ai-dynamo/snapshot/blob/2e263f0eefb69a30483c0517a835a2cbd8c78372/api/podcontract/restore_status.go#L64-L78) |
