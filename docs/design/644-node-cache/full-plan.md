<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Node cache: preheat a snapshot onto the node NVMe and restore from it

Prepared 2026-11-11, for internal review before a SNEP. This is a design only.
Nothing here is implemented, built or run.

This plan replaces `plan.md` (2026-11-10), which describes an earlier scope.

Code references are to the local branch `feat/pagebroker-restore-file-ranges`
at `fcb89163`. Line numbers must be rechecked against `main` before work starts.

Terms used here:

| Term | Meaning |
| --- | --- |
| snapshot | The saved state of a pod (what we often call a checkpoint). |
| snapshot object | The `PodSnapshotContent` that records a snapshot. |
| node cache | A directory on a node's local NVMe that holds copies of snapshots. |
| preheat | Put a snapshot into a node's cache before the application starts. |
| S3 install | A Snapshot installation with `storage.type: s3`. |

## 1. Summary

Today, on an S3 install, every restore downloads the snapshot from S3 into RAM
and then restores. This plan adds three things:

1. **Restore from the node cache.** If the snapshot is on the node's NVMe, the
   node agent restores from it. If not, it restores from S3 as today.
2. **Preheat.** Snapshot itself can download a snapshot from S3 onto a node's
   NVMe ahead of time, with no restore and no pod involved.
3. **Cleanup.** Snapshot removes node copies of snapshots that were deleted.

Another tool may also place a snapshot in the node cache, if it follows the
contract in 6.4. The restore does not care who put it there.

S3 stays the source of truth. A node copy is only a copy.

**The first version is deliberately small.** It keeps the checks that are cheap
and prevent silent damage. Heavier protections are left for later, and what an
admin must do instead is listed in 6.6.

## 2. Why

- **First priority (Yoed):** restore from the NVMe when the snapshot is there,
  and fall back to S3 when it is not.
- **Main use case:** the snapshot is on the node before the application runs,
  so the restore does not wait for a download.
- **Product direction:** Snapshot offers its own way to fill the NVMe, so a
  customer does not need to build a stager.
- **Related issue:** #644 (node-local cache, opened by Oz). It is open, not yet
  `approved`, and has no SNEP. This plan is its first step.

**Where Crusoe's flow fits.** Crusoe runs a PVC store on the node NVMe and
fills it with their own stager. This plan is for S3 installs and does not
change or depend on that flow. Their fork commits, including the import, are
handled separately by the import owner.

## 3. Phases

| Phase | What it delivers | API change | Depends on |
| --- | --- | --- | --- |
| 1 | Single cluster: restore from the node cache or S3, a preheat command, cleanup | None | #644 approved |
| 2 | Preheat through the API: "this snapshot, on these nodes", with per-node status; hardening | Yes, one new object | Phase 1 |
| 3 | Multi-cluster | None on our side | The import work (owned by someone else) |

This document asks for approval of phase 1 and of the direction for phases 2
and 3. Phase 2 reuses phase 1 and replaces nothing in it.

## 4. Fit with issue #644

The issue is written around a shared PVC as the source of truth. This plan
covers S3 installs first, because that is where every restore pays for a
download today. PVC installs can follow with the same node cache.

| # | Requirement in #644 | In this plan |
| --- | --- | --- |
| 1 | The shared store stays the source of truth; the cache is only an optimization | Yes. The store here is S3. |
| 2 | Node cache: a hit reads locally, a miss falls back | Phase 1 |
| 3 | Fill the cache on the first restore | Not covered. Possible later, by copying after an S3 restore. |
| 4 | Prefer nodes that hold a copy | Phase 2, partly: a node label lets users choose such nodes. No automatic preference. |
| 5 | Pre-stage on chosen nodes | Phase 1 by command; phase 2 through the API. By policy: later. |
| 6 | Lifecycle tied to the snapshot: delete, inspect, status | Phase 1: deleting a snapshot removes its copies. Phase 2: per-node status. Removal from chosen nodes is open, for the SNEP (section 7). |
| 7 | Integrity and freshness | Partly (6.2). A snapshot never changes after it is taken, so a copy cannot go stale. There are no checksums. |
| 8 | Switches for each behavior | Phase 1: one switch for the node cache. Others come with their features. |

## 5. How it works

```
Preheat (before the application starts)     Restore (when the pod starts)
---------------------------------------     -----------------------------
1. An admin runs the preheat command        1. Node agent looks in the node
   on node N for snapshot X.                   cache for snapshot X.
2. The command asks PageBroker to           2. Found and valid: restore
   download X from S3 into RAM.                straight from the NVMe.
3. The command copies it from RAM           3. Not found: restore from S3,
   to the NVMe, then renames it                exactly as today.
   into place.
4. PageBroker frees the RAM.
```

## 6. Phase 1 design

### 6.1 The node cache directory

Proposed Helm values (final names to be settled in the SNEP, following
`docs/development/naming.md`):

| Value | Meaning |
| --- | --- |
| `storage.nodeCache.enabled` | Turns the node cache on. Off by default. S3 installs only in phase 1. |
| `storage.nodeCache.hostPath` | The directory on the node's local disk. |
| `storage.nodeCache.minFreeSpace` | A preheat is skipped if it would leave less free space than this. |

- The directory is mounted read-write into the node agent and read-only into
  PageBroker.
- Layout: `artifacts/<PodSnapshotContent UID>/containers/<container>`. This is
  the same layout a PVC store uses, so the existing local restore reads it
  unchanged.
- To find the UID: a `PodSnapshot` names its `PodSnapshotContent` in
  `status.boundSnapshotContentName`; the UID is that object's `metadata.uid`.
- The UID is the one the snapshot was taken under. In one cluster this is the
  object's own UID. Reading it through one accessor now keeps phase 3 simple.

**Mount path.** Two options:

| Option | Effect |
| --- | --- |
| Mount at `/checkpoints` (recommended for phase 1) | Smallest change. The agent config, the privileged mount helper and the restore code already use this path. On an S3 install nothing else is mounted there. |
| A dedicated path such as `/snapshot-cache` | Cleaner, but adds changes to the mount helper (`agent/cmd/ns-bind-mount`) and `agent/internal/nsmount`. The draft SNEP-248 reserves `/checkpoints` for PVC stores. |

### 6.2 Restore: node cache first, else S3

Where: `resolveRestoreArtifact` in `agent/internal/controller/controller.go`,
before the S3 branch.

1. If the node cache is on, look for the snapshot's directory by UID and
   container. Symlinks and non-directories are rejected, as today.
2. Read the manifest in that directory and check that its UID and container
   match. Check that the copy is not marked bad (below).
3. **Found and valid:** return a local source with no S3 handle. The existing
   local restore then runs: compatibility check, then restore straight from
   the NVMe. No RAM staging is used.
4. **Not found, marked bad, or the manifest check fails:** restore from S3 as
   today.

Rules:

- A node copy is only used through a live, Ready snapshot object. A copy of a
  deleted snapshot is never restorable.
- The snapshot can come from any node. The normal compatibility check decides
  whether this node can restore it.
- The agent writes a log line and a pod event saying whether the restore used
  the node cache or S3.

**What "valid" covers.** There are three levels, and the first version checks
two of them:

| Level | Meaning | Checked |
| --- | --- | --- |
| Identity | The copy is the snapshot that was asked for | At every restore, from the manifest |
| Completeness | Every file is there, with the right size | By our preheat, when it writes the copy. Not at restore. |
| Content | The bytes are unchanged | Not checked. There are no checksums. |

**When a restore from a node copy fails.** It cannot switch source midway, so
it fails like any restore. What happens to the copy depends on when it failed:

- **Before the copy was read,** for example at the compatibility check: the
  copy is left alone. Otherwise a node that cannot restore this snapshot would
  set a good copy aside on every attempt.
- **After the restore started reading the copy:** the agent marks the copy as
  bad, by writing a small marker file next to it. Later restores skip a marked
  copy and use S3, and the next preheat rebuilds it (6.3). The agent cannot
  tell whether the copy caused the failure, so every such failure marks it;
  the cost is one extra preheat.

The agent never moves or deletes a copy on its own after a failure. Other
restores may be reading the same copy, or be about to mount it: the restore
resolves the copy's path again after the lookup
(`agent/internal/executor/restore.go:260`). A marker file disturbs none of
them. A marked copy stays on disk until a preheat rebuilds it or its snapshot
is deleted.

**Who does what in a local restore.** The node agent and CRIU do the restore:
the agent mounts the NVMe directory into the pod and CRIU reads from it.
PageBroker copies nothing. It is still involved for two reasons:

- **GPU memory.** PageBroker is the component that loads GPU memory back into
  the GPUs, for PVC and S3 alike. For that it must be able to read the
  snapshot files.
- **The existing code path.** The local restore always opens a PageBroker
  transaction first (`agent/internal/executor/restore.go`, `prepareRestoreSource`).
  For a snapshot with no PageBroker GPU data, PageBroker only checks the path
  and holds the directory open.

This holds in direct mode. In staged mode PageBroker first copies the snapshot
from the NVMe into RAM, which defeats the purpose. So the rule is: **a node
cache restore always uses direct mode, whatever `pageBroker.restoreMode`
says.** That setting has no effect on S3 installs today, so an install may
have it set to `staged` without anyone noticing; refusing it in the chart
could break an upgrade.

**One small PageBroker change is needed.** On an S3 install PageBroker registers
only its S3 engine (`agent/pagebroker/broker.cpp:277-282`). The local restore
asks for the file engine, so today PageBroker would reject it. The change:

- When the node cache is on, also register the file engine, pointed at the
  node cache. PageBroker's third start argument is unused on S3 installs today
  and becomes the node cache path.
- On an S3 install the file engine serves direct restore only. Path-based
  checkpoint and staged-restore requests stay rejected, so the node cache can
  never be used as a store.

This is the engine PVC installs already use; no new transfer logic is written.

### 6.3 Preheat

Preheat uses a call that already exists. `StagedArtifactRestore`
(`agent/internal/pagebroker/artifact_client.go:55`) makes PageBroker download a
snapshot from S3 into RAM staging and return the directory. No pod is needed.

**How it is run.** A command in the node agent image, run in the target node's
agent pod with `kubectl exec`, by hand or from a Job. It takes a `PodSnapshot`
namespace and name, preheats every container of that snapshot, and prints the
result: ready, skipped, or failed with a reason. It must run inside the agent
pod, because PageBroker's socket and RAM staging are only visible there.

Steps, once per run:

1. Take the node's preheat lock, so only one preheat runs on a node. If
   another is running, wait for it, up to a timeout.
2. Clear what an earlier cut-off run left behind. For each record in the work
   area (step 5): end its PageBroker transaction, remove its temporary
   directory and any old copy left by an unfinished replace, then remove the
   record. A record is removed only after its transaction has ended or was
   not found.
3. Read the snapshot object and take its store ID and the S3 handle of each
   container. A snapshot that does not exist, or is not Ready, is a failed
   result.

Then, for each container of the snapshot:

4. If its copy exists, passes the restore's check and is not marked bad
   (6.2), skip this container. A "replace" option carries on even when the
   copy is good.
5. Write a record with a new transaction ID into a work area of the node
   cache. The record is outside the copy, so it is never published with it.
6. Ask PageBroker for a staged download under that ID.
7. Check free space against `minFreeSpace`. The size is only known after the
   download.
8. Copy the staged directory into a temporary directory in the node cache,
   keeping file modes, directory modes and empty directories.
9. Check the file list, sizes and modes against the staged directory, and
   flush to disk.
10. End the PageBroker transaction, which frees the RAM. If this fails, the
    copy is still published, and the record is kept so the next run can free
    the RAM (step 2).
11. Publish: rename any old copy aside, remove its marker, then rename the
    new copy into place.
12. Remove the old copy, then remove the record. If step 10 failed, keep the
    record, so the next run can free the RAM.

Why this route: PageBroker already applies the file modes and empty directories
recorded in the S3 index, so the staged directory is in the local layout. The
copy in step 8 must keep those properties. PageBroker's download code is not
touched.

Rules:

- **Safe to run twice.** A container with a good copy is skipped.
- **No half copy.** A copy only appears under its final name after it is
  complete and flushed.
- **No good copy is left marked bad.** If the command stops between the two
  renames of a replace, the node has no copy and no marker, and restores use
  S3. If it stops after the second rename, the node has a good, unmarked copy.
- **Every exit path cleans up.** A failure or a skip after step 6 ends the
  transaction and removes the temporary directory and the record. This
  includes "not enough disk space".
- **Ending a transaction is safe to repeat.** PageBroker answers "transaction
  not found" for a transaction it no longer has, and preheat treats that as
  success. Any other failure keeps the record and is reported in the result;
  the next run retries it (step 2).
- **A crash is recovered by the record.** It is written before the download
  and removed last, so the next run can clear a crash at any step (step 2).
- **One preheat at a time per node.**

**Result for a snapshot with several containers.** Each container has its own
copy and its own transaction. Ready means every container's copy is present.
If one container fails, the copies of the others stay, the command reports
failed and names the container, and a rerun processes any containers still
missing or invalid.

The logic lives in a package, and the command is a thin wrapper. Phase 2 calls
the same package.

### 6.4 Copies placed by another tool

On an S3 install, another tool may fill the node cache itself, if it can write
the local layout. The contract:

1. The snapshot object exists in the cluster and is Ready.
2. Copy to a temporary name, then rename into
   `artifacts/<PodSnapshotContent UID>/containers/<container>` on the same
   filesystem.
3. Never overwrite an existing copy. A rename onto one fails.
4. Do not change the files afterwards.
5. Create the restore pod only after the rename.

Completeness is the tool's responsibility: Snapshot checks only the copy's
identity (6.2).

Note: a plain S3 sync of a snapshot uploaded by Snapshot's own S3 store is not
enough, because file modes and empty directories are kept in `index.json`. A
tool in that position should call the preheat command instead.

### 6.5 Cleanup

On a timer, the node agent:

- removes copies whose UID no snapshot object names;
- removes nothing if it cannot list the snapshot objects.

A bad marker (6.2) does not change this:

- **While its snapshot exists,** a marked copy is kept, because a restore may
  still be reading it. Preheat removes it when it rebuilds the copy (6.3).
- **When its snapshot is deleted,** the copy and its marker are removed like
  any other copy.

This applies to every copy, including ones another tool placed. A snapshot
holds process memory and should not stay on a disk after deletion.

A preheat that would break `minFreeSpace` is skipped, and restores keep
working from S3. Removing live but unused copies by age or size is not in
phase 1.

### 6.6 Limitations of the first version

These are accepted for the first version and must be documented. Each says
where it is lifted.

| # | Limitation | What the admin does | Lifted in |
| --- | --- | --- | --- |
| 1 | Snapshot does not check whether a copy is being read. | Delete a snapshot, or replace its copy, only when no restore of it is running or starting on that node. Rebuilding a copy that is marked bad is a replace. | Hardening (section 7) |
| 2 | A preheat does not yield to restores. It uses PageBroker's RAM staging and transaction budget, so a restore that arrives during one may be refused and must be retried. | Preheat a node before its applications start. | Hardening |
| 3 | A preheat must not be interrupted. If it is cut off, the snapshot stays in RAM until PageBroker's transaction expires (about two hours), the agent pod is restarted, or the next preheat on that node clears it. Clearing it while its download is still running can fail other S3 restores on the node. | Let it finish. After a cut-off, run a preheat again while no restore is running. | Hardening |
| 4 | A preheat is bound by PageBroker's RAM staging size (`pageBroker.staging.sizeLimit`, 56Gi by default). An S3 restore has the same limit today. | Size staging for the largest snapshot. | Later: direct S3-to-NVMe download |
| 5 | A copy placed by another tool is trusted, as a PVC store is today. Only its identity is checked, and there are no checksums. | Make sure the tool writes complete copies. | Later |
| 6 | The directory is created if it is missing. On a node without the NVMe it sits on the root disk, limited only by `minFreeSpace`. | Turn the node cache on only where the path is on the intended disk. | Hardening |
| 7 | Turning the setting off leaves the copies on the node disks. | Remove them by hand; the docs say how. | Later |
| 8 | Every node downloads from S3 itself. Preheating many nodes at once means many parallel downloads. | Stagger preheats if S3 throughput is a concern. | Later: optional sharing tool (section 9) |
| 9 | Nothing shows which nodes hold a copy, and preheat is an admin action (exec rights on a privileged pod). | Track it outside Snapshot. | Phase 2 |
| 10 | S3 installs and a single cluster only. | — | PVC installs: later. Multi-cluster: phase 3. |

### 6.7 What changes, and how big

Line counts are rough estimates from reading the code.

| Piece | Where | Production code |
| --- | --- | --- |
| Helm values, mounts, PageBroker start argument, file engine for direct restore only | `charts/snapshot`, `agent/pagebroker/broker.cpp` | About 120–170 lines |
| Restore lookup, fallback, forced direct mode, bad-copy marker, event | `agent/internal/controller/controller.go` | About 100–140 lines |
| Preheat package and command | Node agent | About 200–280 lines |
| Cleanup timer | Node agent | About 100–140 lines |

About 500–750 lines in total. Tests, docs and e2e usually come to three or four
times that in this repository. The estimate should be confirmed after the
proof in section 12.

Not changed: the CRDs, the operator, the PageBroker protocol, and PageBroker's
S3 download code.

## 7. Phase 2: preheat through the API, and hardening

Adds a proper way to ask for a preheat and to see the result.

| Piece | Description |
| --- | --- |
| A small new object | Names a snapshot and a node selector. |
| Node agent watcher | Each agent preheats when its node matches, using the phase 1 package. |
| Per-node status | Ready or failed for each node. |
| Node label | A node that holds the copy is labeled, so users can schedule pods there with a normal node selector. |

This is an API change and will carry most of the SNEP discussion. It covers
requirement 5 of #644. Roughly 450–600 more lines.

Two open points for the SNEP:

- **Labeling nodes.** The agent cannot label nodes today, because its role has
  no rule for nodes. Either the agent gets that permission, or the operator
  applies the label from the per-node status.
- **Removal from chosen nodes.** In phase 1 a copy stays until its snapshot is
  deleted. With preheat objects, a copy should stay while any object still
  wants it on that node, and be removed when none does. So Snapshot must track
  which objects currently want a copy, not which one created it.

**Hardening, deferred from phase 1.** Each item lifts a limitation in 6.6:

| Item | Lifts |
| --- | --- |
| The agent process runs the preheat and queues requests, so a dropped session cannot cut one off | 3 |
| Track which copies are being read, so cleanup and replace skip them | 1 |
| A rule for sharing PageBroker's RAM between preheat and restores: wait, skip, or reserve headroom | 2 |
| Refuse to write when the expected disk is not mounted | 6 |

## 8. Phase 3: multi-cluster

Phase 3 is intended to reuse phase 1 unchanged, on one condition, and it needs
its own tests across two clusters. The condition:

> Preheat and restore work on any Ready snapshot object that is bound to the
> S3 store. They do not care whether the object came from a checkpoint in this
> cluster or from an import.

So the import must produce such an object. The import commit from Crusoe's fork
(`b74f466`, PR #809) does not: an imported object holds only the UID.

What we need from the import, **pending confirmation from its owner**:

| # | Requirement | In the fork commit today? |
| --- | --- | --- |
| 1 | On an S3 install, the imported object gets the store ID and one S3 handle for each container | No |
| 2 | The original snapshot UID stays reachable | Yes |
| 3 | The object turns Ready only if the snapshot is in the bucket | No |
| 4 | Neither a delete nor the orphan sweep removes data another cluster uses | Partly. Deleting an imported object does not delete the data directly, but the orphan sweep then removes it. |
| 5 | Both clusters are configured with the same store | Configuration |

No new user-facing field is needed; `spec.source.artifact {uid, containers}` is
enough.

**Blocker: cleanup on a shared bucket.** The operator's orphan sweep deletes
every snapshot in the store that no object in its own cluster names
(`operator/internal/maintenance/process.go:75-116`). It has no notion of which
cluster owns a snapshot and no waiting period. So two clusters that share a
bucket and prefix would delete each other's snapshots. This applies to any
shared bucket, not only to imports. Phase 3 needs a rule for it, such as
turning the sweep off on shared stores or recording an owner for each snapshot.

**Open point: who produces the handle.** SNEP-237 says only PageBroker builds
handles. Either PageBroker returns the handle for a UID, or the operator is
allowed to build it.

Until the import delivers this, this plan gives no multi-cluster support. A
copy placed by another tool would restore in a second cluster only if the
fork's import is merged as it is and is allowed on S3 installs, and then with
no preheat by Snapshot and no S3 fallback.

## 9. Third-party tools considered

| Tool | What it gives | Why it is not in phase 1 |
| --- | --- | --- |
| AIStore | An S3-compatible store inside the cluster; S3 is read once per cluster | The data stops at cluster level, not on the node NVMe. It is another storage system to run. |
| Dragonfly | Shares downloads between nodes and writes files to each node's disk | It does not know our S3 layout, needs a script, and brings a manager, scheduler, MySQL and Redis. |
| OME | An LLM serving platform with an agent that downloads models to node disks | It handles model weights only, and it is a whole platform. Its agent is a good design reference for phase 2. |

PageBroker already downloads from S3 and knows our layout, so it is the default
transport. The download step sits behind one small interface, so a tool such as
Dragonfly can be added later as an option for large clusters.

## 10. Risks

Limitations are in 6.6. These are the risks that remain.

| Item | Detail | Handling |
| --- | --- | --- |
| Layout assumption | The plan assumes a directory staged from S3 is accepted by the local restore as it is, including GPU data read straight from the NVMe. | Prove it first (section 12, step 0). |
| Memory during the copy | Copying from RAM to disk adds page cache, charged to the agent container, on top of RAM staging. | Measure; consider direct I/O. |
| Bad copy | A damaged copy makes one restore fail. | Checks when the copy is written; the copy is marked bad, later restores use S3, and the next preheat rebuilds it (6.2). |
| Data on node disks | A copy holds process memory. | Cleanup (6.5); document it. |
| Write access | Anyone who can write to the directory can supply process state. | Treat the directory as privileged; document it. |
| Draft SNEP-248 | It reserves `/checkpoints` for PVC stores. | State the choice in the SNEP (6.1). |

## 11. Decisions requested

1. **Approve the phases:** single cluster first with no API change, then the
   API and hardening, then multi-cluster.
2. **Accept the limitations in 6.6** for the first version.
3. **Mount path:** `/checkpoints` (smaller, recommended) or a dedicated path
   (cleaner).
4. **Cleanup rule:** Snapshot removes any node copy whose snapshot is gone,
   including copies another tool placed.
5. **Bad-copy rule:** a restore that fails after it started reading a node
   copy marks that copy as bad. Nothing is moved or deleted automatically.
6. **Preheat process:** a self-contained command in phase 1; the agent process
   takes it over in phase 2.
7. **Names:** `storage.nodeCache.*`, and the name of the preheat command.
8. **Import:** agree that section 8 is what we ask of the import owner.

## 12. Delivery

**Step 0, before any pull request: prove the main assumption.** Stage a
snapshot from S3, copy it to a node disk, and restore it through the local
path. Include a GPU case, files with restrictive modes, and an empty
directory. Then confirm the estimate in 6.7.

Next: get #644 labelled `approved`, and write one SNEP that covers all three
phases, with phase 2's API marked as to be settled.

| Order | Pull request |
| --- | --- |
| 1 | Helm values, mounts, PageBroker file engine for direct restore on S3 installs. No behavior change. |
| 2 | Restore from the node cache, else S3. |
| 3 | Preheat package and command. |
| 4 | Cleanup. |
| 5 | Docs and e2e. |

Each pull request needs an approved issue, DCO sign-off, SPDX headers and a
clean `make check`.

Docs the change touches, following `docs/development/writing-docs.md`:

| Page | What is added |
| --- | --- |
| `docs/operations/storage.md` | The node cache: what it is, how to turn it on, preheat, cleanup |
| `charts/snapshot/README.md` | The new Helm values |
| `docs/guides/restore.md` | Restore from the node cache, and how to see which source was used |
| `docs/limitations.md` | The limitations in 6.6 |
| `docs/operations/security.md` | The directory holds process memory and is privileged |
| `agent/pagebroker/S3.md` | PageBroker also serves direct restore from the node cache on an S3 install |
| `docs/reference/compatibility.md` | The release that first has the feature |
| `CHANGELOG.md` | One entry |

Tests to cover:

| Area | Cases |
| --- | --- |
| Restore | Copy present; copy absent; symlinked entry; wrong manifest; `restoreMode: staged` is ignored for a node cache restore; a failure before the copy is read leaves it alone; a failure after marks it bad while a second restore of the same copy carries on; a marked copy is skipped and S3 is used; event names the source |
| PageBroker | Direct restore from the node cache works on an S3 install; path-based checkpoint and staged restore are rejected |
| Preheat | Complete copy, every container; file modes and empty directories are kept; run twice; two at once; an invalid or marked copy is rebuilt; replace; a stop between the two renames leaves no copy and no marker; a stop after publishing leaves a good, unmarked copy and is cleared by the next run; no space frees the RAM; ending a transaction PageBroker no longer has counts as success; a failed end keeps the record and the next run frees the RAM; one container failing leaves the others in place and a rerun processes the ones still missing or invalid; missing snapshot fails |
| Cleanup | Keeps named copies; keeps a marked copy while its snapshot exists and removes it when the snapshot is deleted; removes unnamed copies; removes nothing when listing fails |
| e2e | Preheat a node, then restore with no S3 reads; restore on a node with no copy uses S3; a GPU snapshot restores from the node cache |

Measure and report: preheat time, restore time from the NVMe against S3,
memory during a preheat, and what a restore gets when it arrives during a
preheat near the staging limit (limitation 2).

## 13. Verified and not verified

Verified by reading the code or GitHub:

- The node agent already has the staged-download call, and it needs no pod.
- On an S3 install PageBroker registers only its S3 engine, and the local
  restore asks for the file engine.
- In direct mode PageBroker's file engine copies nothing; it checks the path
  and opens the directory.
- The restore code picks the local path when the source has no S3 handle. For
  such a source the compatibility check reads the manifest from disk
  (`agent/internal/controller/compat.go:161`), so a node cache restore makes
  no S3 call.
- The restore resolves the copy's path again after the lookup
  (`agent/internal/executor/restore.go:260`), so moving a copy in between
  would fail that restore.
- The checkpoint path takes a single target container today
  (`singleTargetContainer` in `agent/internal/controller`).
- PageBroker's GPU engine opens snapshot files read-only on restore, so a
  read-only mount is enough for it.
- The agent's role allows it to read `PodSnapshot` and `PodSnapshotContent`
  objects, so the preheat command, run in the agent pod, can read the
  snapshot object.
- A local snapshot counts as ready when its directory exists and
  `manifest.yaml` is at its root. The S3 index requires the same file.
- PageBroker's socket and RAM staging are an in-pod memory volume, so they
  are freed when the agent pod is restarted.
- PageBroker does not cancel a plain download when its client disconnects, so
  a cut-off preheat leaves the snapshot in RAM until the transaction expires.
- Ending a transaction interrupts its running download (`Broker::Abort`), and
  PageBroker's `S3.md` says cancelling an accepted S3 restore can fail
  concurrent restores.
- PageBroker answers "transaction not found" when asked to end a transaction
  it no longer has (`agent/pagebroker/broker.cpp:821-829`).
- On an S3 install the agent has no `/checkpoints` mount, and PageBroker's
  storage-root argument is unused.
- The S3 layout keeps file modes and empty directories only in `index.json`.
- The operator binds new snapshot objects to the configured S3 store, and the
  fork's import commit does not.
- The operator's orphan sweep deletes any snapshot that no object in its own
  cluster names.
- The agent's role has no rule for nodes.
- #644 and #604 are open and labelled `enhancement` only; PR #809 and PR #889
  are open (checked 2026-11-11).

Not verified:

- That a directory staged from S3 restores through the local path end to end,
  including a GPU snapshot.
- Preheat speed, and memory use during the copy.
- What a restore gets when PageBroker's staging is short during a preheat:
  refused, or made to wait.
- How Crusoe's PVC is backed.
- Nothing was built or run for this plan.
