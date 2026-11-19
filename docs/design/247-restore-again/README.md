<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restoring a Destination Container Again After It Starts Again

How the node agent will restore a restore Pod's destination container again
when it starts again after the Pod's restore ended. This is the optional part
of the restore retry SNEP in
[#836](https://github.com/ai-dynamo/snapshot/pull/836), and
[#971](https://github.com/ai-dynamo/snapshot/pull/971) implements it. The
tracking issue is [#247](https://github.com/ai-dynamo/snapshot/issues/247).

<!-- toc -->
- [What We Want to Do](#what-we-want-to-do)
- [What Changes](#what-changes)
- [Files That Change](#files-that-change)
  - [<code>restore_again.go</code>, New](#restore_againgo-new)
  - [<code>plugin.go</code> in the NRI Plugin: Container Creation](#plugingo-in-the-nri-plugin-container-creation)
  - [<code>controller.go</code> in the Controller: The Restore Queue Pass](#controllergo-in-the-controller-the-restore-queue-pass)
- [Limitation: Nodes Without NRI](#limitation-nodes-without-nri)
<!-- /toc -->

## What We Want to Do

A restore Pod will get its destination container restored again from the same
`PodSnapshot` each time that container starts again after the Pod's restore
ended. The earlier outcome will not matter: a success whose workload later
exited, a failure the agent killed, and a refusal all lead to a new restore.
Today such a container finds the earlier result in its control directory, its
proxy exits with code 112, and the Pod ends in `CrashLoopBackOff`.

There will be no retry limit, so a restore that fails every time will repeat on
each container start, at kubelet's back-off of up to 5 minutes.

## What Changes

The change has five steps, in the order they run:

1. **Detect the new container at creation.** kubelet creates a destination
   container again only when the Pod's restart policy allows it. The NRI plugin
   will treat that container as starting again when the Pod's restore has ended
   and an earlier container's proxy is still in the destination's control
   directory. A container that is not created again gets no new restore and no
   `RestoreRepeated` event.
2. **Clear the earlier result.** The plugin will remove the earlier
   `restore-complete` or `restore-failed` before the new proxy starts, so the
   proxy waits for a new restore instead of exiting.
3. **Set the restore back in progress inside the container's creation.** The
   `nvidia.com/Restored` condition will return to `RestoreInProgress` and the
   Pod will get a `RestoreRepeated` event, before the container's process
   starts. Any error will fail the creation, and kubelet will retry it.
   Writing the status first is what lets the new restore survive an agent
   restart.
   - The write gets 500 ms of NRI's 2 s budget.
4. **Read the Pod from the API server until the cache catches up.** While its
   cache still shows the restore as ended, the restore queue will read the Pod
   from the API server, so a stale terminal copy does not mark it handled. The
   first pass that sees the restore back in progress will also drop the
   agent's in-memory record that the ended restore was handled.
5. **Run the restore again through the existing recovery path.** The pass will
   check each restored container's `restore-complete`. The container that
   started again had its result cleared, so the agent will restore it again.
   Every other restored container in the Pod still has its `restore-complete`,
   so the agent will leave its process running. For such a container, the
   agent will repeat only the PodSnapshot's `lifecycle.postRestore` action,
   when there is one. It will send the HTTP POST again, or find the file
   already created.

A container that starts again while the Pod's restore still runs will not start
a new restore. If its destination already has a result, its proxy will exit
112, as today, and kubelet will create it again after its back-off. Otherwise
its proxy will wait for the running restore's result, as a first container
does.

## Files That Change

### `restore_again.go`, New

The new file `agent/internal/controller/restore_again.go` will hold three
functions:

- `RestoreEnded` reports, from the Pod's `Restored` condition, whether its
  restore has ended, whatever its outcome.
- `RestoreAgain` sets the Pod's `Restored` condition back to
  `RestoreInProgress` for a destination that started again.
- `latestRestorePod` gives the restore queue the Pod from the API server while
  its cache still shows the restore as ended.

### `plugin.go` in the NRI Plugin: Container Creation

When kubelet creates a destination container, the plugin in
[`agent/internal/nri/plugin.go`](../../../agent/internal/nri/plugin.go) will
check whether the container starts again: the Pod's restore has ended and an
earlier container left the proxy behind. If so, it will clear the earlier
result and have the controller set the restore back in progress, before the
container's process starts. A first container whose restore was refused before
it was created does not count, since no proxy is there yet.

### `controller.go` in the Controller: The Restore Queue Pass

Before acting on a Pod whose restore was set back in progress, the restore
queue pass in
[`agent/internal/controller/controller.go`](../../../agent/internal/controller/controller.go)
will read it from the API server until its cache catches up. For the
destination that started again, it will look for the new container instead of
trusting the Pod status, which can still name the container that stopped. The
API server's copy can name it too, since kubelet updates the status only after
the new container runs.

## Limitation: Nodes Without NRI

Restoring again needs NRI. On a node without it, a container that starts again
will run the inert command that the
[Restore Pod Contract](../../reference/restore-pod-contract.md#nodes-without-the-restore-proxy)
requires there, and will not be restored. The workload will be restored again in
a new restore Pod, such as the one a Deployment creates after this one is
deleted.

Restoring again needs a hook before the container's process starts, which only
the NRI plugin gives. The control volume outlives the container, so the earlier
`restore-complete` is still there when the next container starts. With NRI, the
plugin removes it before the process starts. Without NRI, the agent sees the new
container in the Pod status only after it runs. By then the optional startup
gate, which reads `restore-complete`, has passed, so liveness probes would run
during the second restore and could kill it.
