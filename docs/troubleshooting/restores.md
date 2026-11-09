<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshoot Restores

Use this page when a restore is slow, is refused, or fails. It starts with a
restore that is slower than expected, a restore that is refused because a
mount is missing, and restore attempts that fail and retry. Then it lists each
step a restore can fail at, with the likely causes and what to check. To find
which step failed, see [Reading a Failure Message](reading-a-failure-message.md).

## A Restore Is Slower Than Expected

Read the `Restore timing summary` from the node agent on the restore Pod's node.
Check whether CPU restore, CUDA restore, or staging takes the most time. Follow
[Troubleshoot Slow Restores](slow-restores.md) for the next check and
storage-specific tuning.

## A Restore Is Refused with `mount: source <path>, target missing`

The checkpoint recorded a volume mounted at that path in the source Pod, and
the restore Pod has nothing there. The agent refuses the restore before CRIU
runs, with reason `RestoreIncompatible`; nothing was restored and the Pod can
be recreated. Every missing path is listed, so a restore Pod that dropped
several volumes shows them all in one message.

```bash
kubectl get pod <restore-pod> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="nvidia.com/Restored")].message}'
```

Add the volume to the restore Pod at the same `mountPath`. The volume's
backing store can differ; only the path inside the container has to match.

## Restore Attempts Fail with `RestoreAttemptFailed`

With a retry limit above 0, each failed attempt gets a `RestoreAttemptFailed`
warning event. It names the attempt, the destination container, the failure,
and what follows:

- `retrying in place in <backoff>`: the failure left the container untouched,
  and the same container gets the next attempt.
- `not retried: another attempt needs a new container`: the failure changed
  the container, or the container stopped. The restore ends.
- `no retries left` or another `not retried: <reason>`: the restore ends.

The `nvidia.com/restore-retry-status` annotation holds the last failure:

```bash
kubectl get events -n <ns> --field-selector involvedObject.name=<restore-pod>,reason=RestoreAttemptFailed
kubectl get pod <restore-pod> -n <ns> -o jsonpath='{.metadata.annotations.nvidia\.com/restore-retry-status}'
```

A retry needs no action. When every attempt fails at one step, look up that
step under [Restore Fails at a Step](#restore-fails-at-a-step). When
`lastFailure.snapshotUnusable` is `true`, the snapshot is missing, corrupt, or
unsupported. Restore from another `PodSnapshot`.

## A Restore Ends with `RestoreRetriesExhausted`

The restore used all of its attempts, or `config.restore.timeoutSeconds` ran
out before the next one. The event says which. The `nvidia.com/Restored`
condition then ends with `RestoreFailed` and the last attempt's failure.

Look up the last failure's step under
[Restore Fails at a Step](#restore-fails-at-a-step). Delete the Pod to restore
again.

## Restore Fails at a Step

### resolving checkpoint artifact

The agent locates the snapshot directory for the referenced `PodSnapshot` on
this node's snapshot store mount and refuses a path that passes through a
symlink.

**Likely causes:**

- The snapshot store is not mounted on this node's agent
- The `PodSnapshot` is not Ready yet or its content was deleted
- The storage backing the snapshot is not reachable from this node

```bash
kubectl get podsnapshot <name> -n <ns>
kubectl get podsnapshotcontent
kubectl rollout status daemonset/snapshot-agent -n <ns>
kubectl get pvc -n <ns>
```

### reading checkpoint manifest

The agent reads `manifest.json` from the artifact directory.

**Likely causes:**

- A partial or corrupt artifact
- An artifact written by an agent version this agent cannot read

Re-take the checkpoint with the agent version that will restore it.

### validating checkpoint manifest against the restore request

The artifact's content UID and container name must match what the restore
annotation asked for, and a GPU checkpoint taken through PageBroker
CustomStorage can only be restored on a node whose agent has CustomStorage
available.

**Likely causes:**

- The restore mapping names a container that was not the one checkpointed
- The restore pod landed on a node without CustomStorage support

Check the container mapping on the restore pod against the `PodSnapshot`, and
the node's agent configuration.

### verifying cuinterpose library identity

For a checkpoint taken with the cuinterpose shim, the libraries bundled in this
agent must be byte-identical to the ones the checkpointed process had loaded.

Likely cause: the agent was upgraded between checkpoint and restore.

Restore with the agent version that took the checkpoint, or re-checkpoint.

### inspecting placeholder container

The agent resolves the placeholder container through the node runtime, reads
its mounts, cgroup root, image and security profile, and maps the checkpoint's
GPUs onto the GPUs visible in the placeholder. Mismatches that make the node
unable to run the checkpoint are refused with reason `RestoreIncompatible`
rather than failed here. That includes a placeholder with an AppArmor or SELinux
profile when the checkpoint manifest does not confirm a security label for every
process (check `lsm-labels`). Agents that predate this check write no such
record, so their checkpoints are refused even when the dumped processes carry
labels. Re-checkpoint with an agent that records them.

**Likely causes:**

- The placeholder container exited or restarted before the agent reached it
- The runtime did not answer
- The placeholder has fewer GPUs allocated than the checkpoint used, or none

```bash
kubectl describe pod <restore-pod> -n <ns>
kubectl exec <restore-pod> -n <ns> -c <container> -- nvidia-smi -L
```

### building GPU context

The agent builds the mapping PageBroker uses to place checkpointed GPU memory
on the placeholder's GPUs.

**Likely causes:**

- The placeholder has no visible GPUs
- The device mapping recorded at checkpoint does not match the devices allocated now

Check `NVIDIA_VISIBLE_DEVICES` and the GPU resource request on the restore pod.

A checkpoint reports the same step when the agent builds the context from the
source container's CUDA processes and GPUs. Check that the source pod still
holds its GPU allocation and that its CUDA processes are running; a process
that exited between the checkpoint request and the dump leaves nothing to map.

### mounting agent bundle into placeholder

The agent bind-mounts its binaries into the placeholder's mount namespace so
nsrestore, CRIU and the CUDA tools run inside it.

**Likely causes:**

- The placeholder's mount namespace went away because the container restarted
- The agent lacks the privileges to mount into it

Look for container restarts on the restore pod and for the mount error in the
agent log.

### mounting cuinterpose into placeholder

Same as the bundle mount, for the cuinterpose libraries. Only runs for a
checkpoint taken with the shim.

### preparing PageBroker restore

The agent asks PageBroker to stage the artifact, or to map it directly, for
this restore. The cause carries PageBroker's failure code, such as
`ARTIFACT_NOT_FOUND`, `STORAGE_UNAVAILABLE` or `ACCESS_DENIED`, or a connection
error when PageBroker was not reachable at all.

**Likely causes:**

- The `pagebroker` container is not ready
- The control socket path in the agent configuration does not match
- The storage that holds the artifact is unavailable or rejects the credentials

```bash
kubectl logs $agent -n <snapshot-ns> -c pagebroker
```

### mounting PageBroker restore source

The agent bind-mounts the staged or directly mapped checkpoint into the
placeholder for nsrestore to read. Causes are the same as for the bundle mount.

### running nsrestore

The agent uses nsenter to start nsrestore inside the placeholder's namespaces.
A failure that nsrestore itself reports arrives under one of the six steps
that follow this one; this step is left for a run that never got that far or
ended from outside:

- `nsenter could not start nsrestore inside the placeholder (exit 127, ...)`,
  or `(exit 126, not executable)`: the placeholder's root filesystem could not
  run the binary handed to it.
  nsrestore comes from the agent image, not the workload image, so this is
  usually a placeholder without `/proc` mounted or an unusual root filesystem,
  not a missing binary. nsenter's own message follows the colon.
- `agent binary: open /snapshot-binaries/nsrestore: no such file or directory`:
  the agent image itself is missing the binary. Check the agent image tag.
- `nsrestore was killed (signal: killed)`: nsrestore ended by signal before it
  could report. The agent log holds its last lines.
- `...; the restore timeout expired`: the restore ran past
  `config.restore.timeoutSeconds`. The cause before the semicolon is
  what was running when time ran out, and it may carry one of the steps
  below. Raise the timeout, or find what stalled in the agent log.
- `...; the restore was cancelled`: the restore was withdrawn before nsrestore
  finished. An agent that shuts down leaves the attempt to the next agent,
  which runs it again; see
  [Agent Restarts](../design/restore-retry/README.md#agent-restarts).

During an upgrade, a node whose agent pod has not rolled yet still reports
every nsrestore failure under this step. `kubectl rollout status
daemonset/snapshot-agent -n <snapshot-ns>` shows whether the rollout is
complete.

### checking the staged checkpoint

nsrestore re-reads the manifest from the checkpoint as staged inside the
placeholder and checks that the PageBroker execution handed to it matches what
the checkpoint needs.

**Likely causes:**

- The staged checkpoint is not the one the agent validated, usually a storage mount that changed under it
- An agent and nsrestore from different builds

Compare the snapshot directory on the node with the artifact PageBroker
reports, and check that the agent and nsrestore come from the same image.

### preparing CRIU restore

nsrestore builds CRIU's restore options from the manifest and the placeholder's
security profile.

Likely cause: the manifest records a mount or cgroup layout the restore options
cannot express.

Compare the restore pod's volumes with the source pod's.

### applying rootfs diff

nsrestore extracts the rootfs diff saved at checkpoint over the
placeholder's root filesystem, so files the workload wrote or changed are back
before CRIU recreates the process. The cause carries tar's first message.

**Likely causes:**

- The placeholder image differs from the source image, so a path in the diff cannot be created
- A read-only root filesystem on the restore pod
- A diff entry the placeholder lacks permission to write

Check that the restore pod uses the same image as the checkpointed pod and
that its root filesystem is writable.

### preparing GPU restore

For a GPU checkpoint, nsrestore prepares the CUDA job file and the GPU device
mounts, and opens the CUDA tools it will need after CRIU replaces the mount
namespace.

**Likely causes:**

- A multi-GPU checkpoint with no CUDA job file. The message names it
- GPU device paths that the placeholder cannot see
- A bundle mount that went away because the placeholder restarted

For the first cause, take the checkpoint again with the source launched under
`cuda-checkpoint --launch-job`.

```bash
kubectl exec <restore-pod> -n <ns> -c <container> -- nvidia-smi -L
```

### restoring with CRIU

nsrestore prepares the placeholder's mounts and sentinels for CRIU, runs the
CRIU restore, and reads the restored process table. For a CRIU failure the
cause is the last error CRIU logged and the agent log holds the key lines of
`restore.log`. The other causes name the operation that failed, such as
`failed to unmount /dev/shm before restore`, and `restore.log` has nothing on
them.

**Likely causes:**

- A file, socket or mount the process had open at checkpoint is not present in the placeholder
- A listening port already taken
- A kernel or CRIU version that cannot recreate a resource the dump recorded
- An AppArmor profile CRIU cannot apply

Look past CRIU's nonfatal probes, such as the TUN message above, to the final
errors in the agent log. Check the restore pod against the
[supported frameworks](../using-snapshot/README.md).

### restoring CUDA state

After CRIU, nsrestore maps the checkpointed CUDA PIDs onto the restored ones,
restores GPU device mounts and GPU memory, through the driver's checkpoint tool
or through PageBroker for CustomStorage, and for a multi-GPU workload runs the
cuinterpose restore that rebuilds shared GPU memory.

**Likely causes:**

- The restored process tree does not contain the PIDs the checkpoint recorded
- The restore pod's GPUs do not match the checkpoint's device map
- The driver refused the restore
- PageBroker could not serve the GPU memory
- The cuinterpose restore failed, multi-GPU workloads only, with the coordinator's output in the message after `cuinterpose coordinator:`

The agent log carries the CUDA restore output and, for CustomStorage, the
PageBroker result. Check the restore pod's GPU allocation against the
checkpoint's.

### committing PageBroker restore

After the process is running, the agent tells PageBroker the transaction is
complete so it can release what it staged. A failure here does not undo the
restore: the pod is marked restored, and the failure is reported on a
`RestoreCleanupFailed` warning event and in the agent log.

**Likely causes:**

- PageBroker restarted during the restore (`TRANSACTION_NOT_FOUND`)
- The restore outlived the transaction (`TRANSACTION_EXPIRED`)
- A connection error

Check the `pagebroker` container's logs and restarts. Staged data is released
by PageBroker's own expiry.

### validating restored process

The agent checks that the restored PID is alive and not a zombie inside the
placeholder.

Likely cause: the restored process exited as soon as CRIU resumed it, typically
because something it had open at checkpoint is not there now: a file, a
listening port, a peer, or GPU state.

The agent log carries the process diagnostics and the `restore.log` lines
recorded at the failure. The workload's own output is in the restore pod's
container logs.

### checking the post-restore action

When the checkpoint's container has a `lifecycle.postRestore` action, the agent
checks its settings against the restore Pod's spec before restoring anything.
The Pod's `nvidia.com/Restored` condition becomes `False` with reason
`RestoreFailed`, and nothing is restored.

Likely causes:
- **The path is one of Snapshot's own files** in the snapshot-control volume.
- **The port name is not one of the destination container's ports.**
- **The HTTP path does not parse** as a URL path.

### running the post-restore action

After writing `restore-complete`, the agent notifies the restored application:
it creates the file, or sends the HTTP POST, every `periodSeconds` until it
succeeds or the restore deadline (`config.restore.timeoutSeconds`) passes. The
restore fails, and the restored container is stopped, when no attempt
succeeded.

Likely causes:
- **The file cannot be created.** Its directory does not exist in the restore
  container, or the container's user cannot write it, for example on a
  read-only mount or with `readOnlyRootFilesystem`. File actions run as the
  container's user.
- **The image has no shell.** File actions run a shell command in the
  container, so the image needs one of the shells the
  [Workload Contract](../reference/workload-contract.md#lifecycle-hooks)
  lists.
- **The application does not listen on the port at the restore Pod's IP**, for
  example because it bound only to the source Pod's IP.
- **The endpoint returns a status other than 2xx**, including a redirect, which
  the agent does not follow.
- **A NetworkPolicy blocks the agent.**

The message carries the last attempt's error. The agent log shows every attempt
at debug level.

A file that already exists before the restore does not fail it: the agent takes
it as the notification, so the restore succeeds and the application is never
notified. Use a path that only Snapshot writes, such as one on an `emptyDir`
that the restore Pod mounts for this purpose. A file on a volume that outlives
the Pod, such as a `PersistentVolumeClaim`, stays there from an earlier restore
until the application removes it.
