<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore Retry

How the node agent retries a failed restore attempt: when it retries, and how
it keeps count across agent restarts. The settings are in the
[API Reference](../../reference/api.md#restore). This page is for contributors
changing the mechanism. For where it fits, see the
[Architecture](../../overview/architecture-overview.md#restore-flow).

<!-- toc -->
- [Attempts and the Deadline](#attempts-and-the-deadline)
- [Recovery After a Failed Attempt](#recovery-after-a-failed-attempt)
- [Waiting for the Next Attempt](#waiting-for-the-next-attempt)
- [Agent Restarts](#agent-restarts)
- [Retry Status](#retry-status)
- [Code Map](#code-map)
<!-- /toc -->

## Attempts and the Deadline

The retry limit is how many times the agent retries a destination after its
first attempt fails. It defaults to 0. All attempts of a destination share one
deadline: the start of the first attempt plus `restore.timeoutSeconds`. With a
limit above 0, the agent keeps the count in the destination's
`restore-attempts` record.

## Recovery After a Failed Attempt

The agent decides what follows a failed attempt from the failure and from the
attempts and time left. It checks these in order:

| The failed attempt | What follows |
| --- | --- |
| Ran out the deadline, failed a check that no attempt can change, or got a PageBroker code that names the snapshot or the request | The restore ends |
| Changed the container | The restore ends |
| Was the last attempt, or left no time | `RestoreRetriesExhausted`, and the restore ends |
| Left the container untouched | A retry in the same container, after a backoff |

The checks that no attempt can change are the manifest validated against the
request, the cuinterpose library identity, and a PID 1 that is not the restore
proxy. An abort that left a PageBroker transaction open also ends the restore.
The PageBroker codes that end it are `ARTIFACT_NOT_FOUND`, `ARTIFACT_CORRUPT`,
`UNSUPPORTED_ARTIFACT`, `ACCESS_DENIED`, `STORE_MISMATCH`, and
`INVALID_REQUEST`.

An attempt changed the container when any of these happened:

- nsrestore reported that it began changing the container. It prints this
  progress message before it applies the rootfs diff.
- The agent could not remove a mount it added to the container.
- The container stopped during the attempt, with a limit above 0.

Another attempt after such a failure needs a new container, and the agent
does not start one, so the restore ends. The `RestoreAttemptFailed` event says
`another attempt needs a new container`, which tells this ending apart from a
failure that no attempt can fix.

The Pod's `restartPolicy` does not change which failures are retried. Retries
also work on nodes without the restore proxy.

The decision covers attempts only. A refusal by the compatibility gates is
reported as before and removes any record. A failed preflight, a failed write
of `restore-complete`, or a failed post-restore action ends the restore.

With a limit of 0, every failure ends the restore, as it did before retries.
The agent keeps no record, and emits no `RestoreAttemptFailed` or
`RestoreRetriesExhausted` event.

## Waiting for the Next Attempt

A retry does not run in the pass that saw the failure. The agent saves the
retry in the record, emits `RestoreAttemptFailed`, and leaves the destination
pending. The Pod's `nvidia.com/Restored` condition stays `RestoreInProgress`.
The next pass comes with the Pod's next update, or within 30 seconds.

A retry in the same container waits for its backoff: 30 seconds after the
first failed attempt, doubling up to 5 minutes. A pass that comes earlier does
nothing for that destination. The time left must cover the backoff, or the
restore ends as exhausted.

While an attempt runs, the agent's NRI plugin reports the container stopping.
With a limit above 0, that ends the attempt at once, and the failure counts as
one that changed the container. A failed attempt whose container the runtime
no longer runs counts the same way. With a limit of 0, the attempt runs as
before.

## Agent Restarts

A pass after an agent restart first checks for `restore-complete` through the
container's process, as before retries. When it is there, the pass runs only
the post-restore action, with a full `restore.timeoutSeconds` again. CRIU does
not run again.

A record that shows an attempt with no retry saved is an attempt that no agent
saw finish. The attempt runs again within the same deadline and keeps its
number, as it did before retries. It gets no `RestoreAttemptFailed` event,
since no agent saw it fail. This includes an attempt whose failure ended the
restore just before the agent stopped, before the Pod's status said so. With a
limit of 0 there is no record, so the attempt starts over with a full
`restore.timeoutSeconds`, as before retries.

An agent that is stopping does not decide anything for a failed attempt. It
writes nothing and stops no container, and the next agent finds the attempt
unfinished.

## Retry Status

After each pass, the agent writes the `nvidia.com/restore-retry-status`
annotation from the records of all the Pod's destinations. The destinations
restore in parallel, so one write per pass keeps every entry. It is a
server-side apply on the Pod with the field manager `snapshot-agent-restore`,
and it is skipped when the value would not change. The agent never reads the
annotation back. With a limit of 0 there are no records, so the agent writes no
annotation. Its fields are in the [API Reference](../../reference/api.md#restore).

## Code Map

The code that implements restore retry is in these packages and files:

| Path | Holds |
| --- | --- |
| `agent/internal/controller/restore_attempts.go` | The attempt record, the retry limit, the deadline, and the start of each attempt |
| `agent/internal/controller/restore_recovery.go` | The recovery decision, the backoff, the check that the container stopped, and the events |
| `agent/internal/controller/restore_retry_status.go` | The retry status annotation |
| `agent/internal/controller/controller.go` | `runRestore`, which runs one attempt |
| `agent/internal/executor/restore_failure.go` | `RestoreFailure`, which says where an attempt failed and whether it changed the container |
| `agent/internal/executor/nsrestore_output.go` | Reading nsrestore's progress message and its PageBroker code |
| `agent/internal/nri/plugin.go` | The report that a destination container stopped |
| `agent/internal/runtime/container_state.go` | Whether the runtime still runs a container |
| `api/podcontract/restore_status.go` | The retry status annotation and its type |
| `api/podcontract/protocol.go` | The retry limit annotation and the `restore-attempts` file name |
