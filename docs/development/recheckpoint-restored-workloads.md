<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Checkpointing restored workloads: draft implementation

Tracking issue: [Support checkpointing restored workloads #447](https://github.com/ai-dynamo/snapshot/issues/447).

The intended behavior is full-container C→R→C→R→C→R: capture A,
restore B, capture B, restore C, capture C, restore D. Every capture must include
the workload main process, workers, and helpers. Verify output and preserved CPU
and GPU contents after every restore using Snapshot's existing workload contract.
This draft does not establish full-container repeated restore support.

## Implemented foundations

- Capture and restore CUDA PID mapping uses validated, nonempty `NSpid` chains
  without a fixed namespace depth. Restore mapping stays within the subtree
  rooted at CRIU's restored PID. Invalid identities, ambiguous matches, and
  manifest PIDs absent from that subtree fail.
- The executor pins the destination PID namespace alongside the mount
  namespace already owned by the mount engine. It passes both descriptors to
  `nsrestore` explicitly and retains descriptor-backed execution of its binary.
- `nsrestore` owns its inherited descriptors through CRIU restore and CUDA
  restore/unlock. CRIU registers an owned duplicate under `extPodPidNs` only
  when `criuDump.external` declares `pid[INODE]:extPodPidNs`; existing
  checkpoints need no PID-namespace metadata or descriptor.
- Socket-image preparation uses the pinned destination mount namespace inode.
  The existing mount-engine selection, GPU-device aliasing and pinning, CRIU
  cleanup after CUDA unlock, and lifecycle sentinels are preserved.
- External-PID images containing PID 1 are rejected while building restore
  options, before rootfs replay or CUDA/CRIU restore. The destination placeholder
  occupies PID 1, and this draft does not replace it.

## Unfinished external capture patch

[recheckpoint-external-pidns.patch](patches/recheckpoint-external-pidns.patch)
records the remaining external-PID dump plumbing against this branch. It is
**not applied to production code**. It adds an internal namespace inode to the
inspection result and an optional CRIU external declaration, without a public
option. It deliberately does not populate that inode in `inspectContainer`:
there is no validated automatic workload selection that can safely enable it.
Even applying the patch alone leaves ordinary capture unchanged. The patch uses
zero-context insertion hunks; check it with:

```sh
git apply --check --unidiff-zero docs/development/patches/recheckpoint-external-pidns.patch
```

A complete solution must first handle the captured-PID-1 conflict under the
existing workload contract. Selecting a child-only workload to avoid the conflict
would fail the full-container acceptance requirement. No manual PID selection,
PID-counter advancement, extra session commands, or new standby requirements
are part of this draft.

The descriptor plumbing is a foundation, not proof that CRIU can directly
restore any full-container image. In particular, validate real namespace entry,
helper execution across procfs/mount replay, and PID/session collisions before
enabling external-PID capture. The POC's path-based helper execution and
experimental CRIU changes are not ported.

## Automatic workload-root selection still required

`inspectContainer` still begins at the runtime's container PID. On a restored
container this can select the idle placeholder, rather than the restored tree,
or include restore infrastructure. `ProcessTreePIDs` can enumerate workers and
helpers once the correct root is known, but does not choose that root.

The restore result already reports CRIU's restored PID. Further integration must
associate the restored root with the exact container incarnation and namespace,
validate that identity at subsequent capture, and select the complete workload
tree automatically. It must survive agent restarts and reject stale or ambiguous
identity. Design and persistence of that association are outstanding; this draft
introduces no new annotation or workload API. Command names and GPU ownership
are not reliable root selectors.

## Verification

Focused tests cover namespace depths one through four, multiple workers,
invalid/missing PID information, ambiguous mappings, matching PIDs outside the
workload tree, missing manifest PIDs, inherited child FD numbers, metadata-driven
CRIU registration, legacy checkpoints, and PID-1 rejection. A mock CRIU RPC
service worker exercises actual go-criu FD registration and success/failure
cleanup; it does not restore a workload or GPU state.

Run from the repository root:

```sh
go test ./agent/internal/runtime ./agent/internal/criu ./agent/internal/executor ./agent/cmd/nsrestore
make -C agent test
make check TOOLS_BIN_DIR=/tmp/snapshot-recheckpoint-tools
```

The isolated tools directory is a local validation choice, not a repository
requirement. It avoids older installed `protoc` and Go-built analysis tools.
Test and gate results are recorded in the draft PR.

## Full-container acceptance test: outstanding

1. Start a workload with workers, helpers, and known CPU/GPU contents using the
   existing workload contract.
2. Capture the complete container workload, including its main process.
3. Restore into another container and verify the process tree, correct output,
   CPU state, and GPU contents.
4. Repeat capture and restore twice more, verifying after every restore.
5. Inspect namespace ancestry after every restore; nesting must not increase.
6. Verify subsequent captures do not accumulate placeholders or restore helpers.
7. Check that capture and restore lifecycle signals retain their existing meaning
   and that no manual PID management or process-selection annotations are needed.

Full-container GPU validation has not been run for this branch. Keep the PR in
**draft** until this complete three-cycle integration test passes.
