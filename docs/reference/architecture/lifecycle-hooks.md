<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Lifecycle hooks

How the node agent runs a container's lifecycle hooks: the
`checkpointReadinessProbe` before a checkpoint, and `lifecycle.postRestore`
after a restore. The fields are in the
[API reference](../api.md#container-settings), and what a workload must do is in
the [workload contract](../workload-contract.md#lifecycle-hooks). This page is
for contributors changing the mechanism. For where it fits, see the
[architecture overview](README.md).

## Where the settings come from

The operator copies a `SnapshotJob`'s `containers` into the `PodSnapshot` it
creates, and a `PodSnapshot`'s into its `PodSnapshotContent`. The API server
fills in the timing defaults first, so the content holds the full settings.
Its spec is immutable, so every restore of a checkpoint uses the settings the
checkpoint was taken with. The agent reads them only from the content.

## The checkpoint readiness probe

The probe replaces the Ready check at the agent's readiness gate in
`reconcileCapture`. For a `SnapshotJob`, the operator then keeps the target
container's own readiness probe instead of injecting the `ready-for-snapshot`
one (`SourceJobOptions.HasCheckpointReadinessProbe`), so Ready can mean what
the workload says.

- **What a check is.** A file check passes when a regular file exists at the
  path in the source container. An HTTP check passes when a GET to the Pod IP
  returns a 2xx status. A named port is resolved against the source container's
  ports.
- **Pacing.** The first check runs `initialDelaySeconds` after the container's
  `startedAt`. After each check the agent requeues the work order for
  `periodSeconds` later. Pod events and the content resync also reconcile the
  work order, so a check only runs once its time is due.
- **Counting.** The agent checkpoints after `successThreshold` checks in a row
  pass. A failed check resets the count, and the agent keeps waiting; there is
  no failure threshold, and the container is never restarted. A new container
  ID, which a restart gives the container, also resets it, initial delay
  included.
- **State.** The count lives in the agent's memory, per work order, and is
  dropped when the work order ends. An agent restart starts the count over,
  which delays a checkpoint but never takes one early.
- **Refused probes.** An HTTP probe on a port name the container doesn't have
  can never pass. The agent fails the work order with
  `InvalidCheckpointReadinessProbe` before the first check, and posts a
  Warning event with that reason on the source Pod.
- **Reporting.** The agent posts a `WaitingForCheckpointReadiness` event on the
  source Pod when the wait starts, and logs failed checks at debug level. The
  event is posted once per work order; after an agent restart it can come
  again.

The wait has no deadline of its own: `checkpointTimeoutSeconds` bounds only the
dump, and a `SnapshotJob` is bounded by its `activeDeadlineSeconds`.

## The post-restore action

`runRestore` runs the action for each destination container:

1. **Before CRIU.** The agent checks that the action can run: for a file, a
   path that is not one of Snapshot's own control files, with no file there
   yet; for HTTP, a port the destination container has. A failed check fails
   the restore before any CRIU work.
2. **After CRIU.** Before it writes `restore-complete`, the agent checks the
   file again: it must still be absent, and its directory must exist and be
   writable by the container's user. The rootfs diff can bring the file back
   from the source, for example when the source was itself restored and
   notified, and it can also bring the directory, which is why the directory
   is checked only now. A failed check stops the restore before the restored
   process is released.
3. **After `restore-complete`.** The agent creates the file, or sends the POST,
   every `periodSeconds`, with `timeoutSeconds` per attempt, until it succeeds.
   It retries every failure, since a `404` or a missing directory may be an
   application that is still starting.

One deadline, `restoreTimeoutSeconds`, covers the restore and the action. When
no attempt succeeds by then, or a check fails, the restore fails as any restore
does: the agent writes `restore-failed` and kills the destination container,
even though the restored process was already released. The steps in the failure
are `checking the post-restore action` and `running the post-restore action`.

### Recovery after an agent restart

The `nvidia.com/Restored` condition becomes `True` only after the action
succeeds, so a restart in between leaves it `RestoreInProgress`. On recovery,
`restore-complete` shows that CRIU finished, and the agent runs only the action
again, never CRIU or the checks:

- An existing file counts as the agent's own: it was checked absent before
  `restore-complete`, and only the agent creates it afterwards.
- An HTTP action is sent again. Applications must handle a repeated
  notification.
- The action gets a full `restoreTimeoutSeconds` again, so a restart can extend
  the deadline by one timeout.

## Safety of the actions

Each action can do only what the application itself could:

- **Files** are checked and created by a short `/bin/sh` script that the agent
  runs in the container through CRI `ExecSync`, the way kubelet runs an `exec`
  probe. The script runs as the container's user, with its capabilities,
  seccomp and LSM profile, inside its mounts, so the kernel decides what it may
  read or write. The path is passed as an argument, never as part of the
  script. A marker is created with `set -C`, so it never overwrites a file, and
  its directory is never created. The image needs `/bin/sh`.
- **HTTP** requests go only to the Pod IP and the resolved port, as kubelet's
  `httpGet` probes do; a host-network Pod's IP is the node's. The URL is built
  like kubelet's probe URL, so a path cannot name another host. The agent does
  not follow redirects, so a redirect fails, does not use proxies from its
  environment, and reads at most 10 KiB of a response without logging it.

## Code map

| Path | What |
|---|---|
| `api/v1alpha1/container_config_types.go` | The settings types and their validation. |
| `operator/internal/controller` | Copying the settings: `snapshotjob_podsnapshot.go`, `podsnapshot_reconciler.go`, `container_settings.go`. |
| `agent/internal/lifecyclehooks` | The actions: `FileExists`, `CreateMarker`, `CheckMarkerTarget`, `CheckWritePath`, `ResolvePort`, `HTTPGet`, `HTTPPost`. |
| `agent/internal/runtime` | `ExecSync`, which runs a file action's command in the container through CRI. |
| `agent/internal/controller/checkpoint_readiness.go` | The probe at the readiness gate: `fileProbe` and `httpProbe`. |
| `agent/internal/controller/post_restore.go` | The post-restore action: `filePostRestore` and `httpPostRestore`, their checks, and recovery. |
| `api/podcontract` | `IsReservedControlFile`, Snapshot's own control files. |
