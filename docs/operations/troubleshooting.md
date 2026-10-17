# Troubleshooting

Common issues when running Snapshot, and where to look.

## A checkpoint never becomes Ready

A `PodSnapshot` becomes Ready only after the `snapshot-agent` confirms the
checkpoint contents — writing the checkpoint is not enough on its own. Check the
status and the agent logs:

```bash
kubectl get podsnapshot <name> -n <ns>
kubectl logs daemonset/snapshot-agent -n <ns> --all-containers
```

A common cause is a replica manifest that does not follow the [framework
guides](../guides/README.md) -- missing the control volume, seccomp profile,
or readiness gate Snapshot relies on -- or that omits mounts or secrets the
replica needs to start.

## The agent or a restore pod will not start

The `snapshot-agent` runs privileged with `hostPID`, `hostIPC`, and `hostNetwork`.
If the namespace enforces a restrictive Pod Security level, the agent — or a
restore pod — can be rejected. See [Security](security.md).

## Reading a failure message

Every checkpoint and restore failure names the step it came from, in the form
`while <step>: <cause>`. The step tells you which part of the flow stopped; the
cause is what the tool at that step reported. The sections below are organised
by step.

For a restore, the message is on the pod's `nvidia.com/Restored` condition and
on a `RestoreWorkerFailed` event, one entry per destination container, and ends
with the node whose agent holds the full detail:

```bash
kubectl get pod <restore-pod> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="nvidia.com/Restored")].message}'
kubectl get events -n <ns> --field-selector involvedObject.name=<restore-pod>
```

For a checkpoint, the message is on the `PodSnapshotContent` `Failed` condition
with reason `CheckpointFailed`:

```bash
kubectl get podsnapshotcontent <name> -o jsonpath='{.status.conditions[?(@.type=="Failed")].message}'
```

The agent logs the same failure with a structured `step` key, so one filter
shows every failure at a given step:

```bash
kubectl logs daemonset/snapshot-agent -n <ns> -c agent | grep '"step"'
```

Raise `agent.logLevel` in the chart values when you need more than the error
lines. A restore that failed inside the placeholder also leaves nsrestore's
stderr and the key lines of CRIU's `restore.log` in the agent log.

## Restore fails at a step

### resolving checkpoint artifact

The agent locates the checkpoint directory for the referenced `PodSnapshot` on
this node's checkpoint storage mount and refuses a path that passes through a
symlink.

Likely causes: checkpoint storage is not mounted on this node's agent; the
`PodSnapshot` is not Ready yet or its content was deleted; the storage backing
the checkpoint is not reachable from this node.

```bash
kubectl get podsnapshot <name> -n <ns>
kubectl get podsnapshotcontent
kubectl rollout status daemonset/snapshot-agent -n <ns>
kubectl get pvc -n <ns>
```

### reading checkpoint manifest

The agent reads `manifest.json` from the artifact directory.

Likely causes: a partial or corrupt artifact, or one written by an agent
version this agent cannot read.

Re-take the checkpoint with the agent version that will restore it.

### validating checkpoint manifest against the restore request

The artifact's content UID and container name must match what the restore
annotation asked for, and a GPU checkpoint taken through PageBroker
CustomStorage can only be restored on a node whose agent has CustomStorage
available.

Likely causes: the restore mapping names a container that was not the one
checkpointed; the restore pod landed on a node without CustomStorage support.

Check the container mapping on the restore pod against the `PodSnapshot`, and
the node's agent configuration.

### verifying cuinterpose library identity

For a checkpoint taken with the cuinterpose shim, the libraries bundled in this
agent must be byte-identical to the ones the checkpointed process had loaded.

Likely cause: the agent was upgraded between checkpoint and restore.

Restore with the agent version that took the checkpoint, or re-checkpoint.

### inspecting placeholder container

The agent resolves the placeholder container through the node runtime, reads
its mounts, cgroup root and image, and maps the checkpoint's GPUs onto the GPUs
visible in the placeholder. Mismatches that make the node unable to run the
checkpoint are refused with reason `RestoreIncompatible` rather than failed
here.

Likely causes: the placeholder container exited or restarted before the agent
reached it; the runtime did not answer; the placeholder has fewer GPUs
allocated than the checkpoint used, or none.

```bash
kubectl describe pod <restore-pod> -n <ns>
kubectl exec <restore-pod> -n <ns> -c <container> -- nvidia-smi -L
```

### building GPU context

The agent builds the mapping PageBroker uses to place checkpointed GPU memory
on the placeholder's GPUs.

Likely causes: the placeholder has no visible GPUs; the device mapping recorded
at checkpoint does not match the devices allocated now.

Check `NVIDIA_VISIBLE_DEVICES` and the GPU resource request on the restore pod.

### mounting agent bundle into placeholder

The agent bind-mounts its binaries into the placeholder's mount namespace so
nsrestore, CRIU and the CUDA tools run inside it.

Likely causes: the placeholder's mount namespace went away because the
container restarted; the agent lacks the privileges to mount into it.

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

Likely causes: the `pagebroker` container is not ready; the control socket path
in the agent configuration does not match; the storage backend holding the
artifact is unavailable or rejects the credentials.

```bash
kubectl logs daemonset/snapshot-agent -n <ns> -c pagebroker
```

### mounting PageBroker restore source

The agent bind-mounts the staged or directly mapped checkpoint into the
placeholder for nsrestore to read. Causes are the same as for the bundle mount.

### running nsrestore

The agent uses nsenter to start nsrestore inside the placeholder's namespaces.
nsrestore applies the rootfs diff, runs the CRIU restore and restores CUDA
state. The cause tells you which of those stopped:

- `nsenter could not start nsrestore inside the placeholder (exit 127, ...)`:
  the placeholder's root filesystem could not run the binary handed to it.
  nsrestore comes from the agent image, not the workload image, so this is
  usually a placeholder without `/proc` mounted or an unusual root filesystem,
  not a missing binary. nsenter's own message follows the colon.
- `agent binary: open /snapshot-binaries/nsrestore: no such file or directory`:
  the agent image itself is missing the binary. Check the agent image tag.
- `CRIU restore failed: ...`: the last error CRIU logged. The agent log holds
  the key lines of `restore.log`.
- `nsrestore was killed (signal: killed) after the restore timeout expired`:
  the restore ran past `agent.restore.restoreTimeoutSeconds`. Raise it, or find
  what stalled in the agent log.

### committing PageBroker restore

After the process is running, the agent tells PageBroker the transaction is
complete so it can release what it staged. A failure here does not undo the
restore: the pod is marked restored, and the failure is reported on a
`RestoreCleanupFailed` warning event and in the agent log.

Likely causes: PageBroker restarted during the restore
(`TRANSACTION_NOT_FOUND`); the restore outlived the transaction
(`TRANSACTION_EXPIRED`); a connection error.

Check the `pagebroker` container's logs and restarts. Staged data is released
by PageBroker's own expiry.

### validating restored process

The agent checks that the restored PID is alive and not a zombie inside the
placeholder.

Likely cause: the restored process exited as soon as CRIU resumed it, typically
because something it had open at checkpoint is not there now: a file, a
listening port, a peer, or GPU state.

The agent log carries the process diagnostics and the `restore.log` lines
captured at the failure. The workload's own output is in the restore pod's
container logs.

## Checkpoint fails at a step

### resolving checkpoint artifact path

The agent computes where the artifact will live under the storage base path
from the content UID and container name.

Likely causes: a `PodSnapshotContent` with an empty or malformed content UID;
a misconfigured storage base path.

### inspecting source container

The agent resolves the container's PID and OCI spec through the node runtime,
reads its rootfs, overlay upper directory, mounts and network namespace, and
discovers its CUDA processes and GPUs.

Likely causes: the container exited before the agent reached it; the runtime
uses a snapshotter without an overlay upper directory; CUDA processes sit in
nested PID namespaces; GPU discovery failed inside the container.

```bash
kubectl get pod <source-pod> -n <ns> -o jsonpath='{.status.containerStatuses}'
```

### preparing PageBroker checkpoint

The agent asks PageBroker to open a transaction and provide the staging or
destination directory. The cause carries PageBroker's failure code, such as
`INSUFFICIENT_STORAGE` or `STORAGE_ERROR`, or a connection error.

Likely causes: the `pagebroker` container is not ready; the checkpoint volume
is full or unavailable.

```bash
kubectl logs daemonset/snapshot-agent -n <ns> -c pagebroker
kubectl get pvc -n <ns>
```

### inspecting cuinterpose

When the workload opted into the cuinterpose shim, every CUDA process must have
loaded it, its delivery mount must be read-only, and the coordinator must
answer before capture becomes destructive.

Likely causes: a CUDA process started without the shim; the delivery mount is
writable; the coordinator is not running.

### staging CUDA job file

For a driver-managed GPU checkpoint, the agent copies the CUDA checkpoint job
file out of the container and checks that it lists every GPU.

Likely causes: the workload has not reached its quiescent point, so the job
file is missing or stale; the GPU count in the file differs from the devices
discovered.

### configuring CRIU dump and writing checkpoint manifest

The agent builds CRIU's dump options from the inspected container and writes
`manifest.json` into the staging directory.

Likely causes: a mount layout CRIU options cannot express; a staging directory
that is not writable.

### checkpointing GPU state

The agent checkpoints GPU memory for each CUDA process, through the driver's
checkpoint tool or through PageBroker for CustomStorage, after preparing
cuinterpose when the shim is in use. A failure here, or in the CRIU dump that
follows, leaves the source process unsafe to resume, so the agent terminates
it.

Likely causes: a kernel still running or a stream still busy when the lock was
taken; a driver without checkpoint support for this GPU; another process
holding the device, such as a GPU monitoring agent.

### dumping with CRIU

CRIU dumps the process tree into the staging directory. The cause is the error
CRIU logged.

Likely causes: a file descriptor CRIU cannot dump, such as an io_uring instance
when the seccomp profile from the framework guides is missing, a socket to a
peer outside the container, or an open file on a volume the checkpoint did not
record; missing ptrace permission.

The agent log carries the CRIU error. Check the source pod against the
[framework guides](../guides/README.md).

### committing PageBroker checkpoint

PageBroker publishes the staged artifact to its final location. The dump has
already stopped the source process at this point, so a failure here means
re-taking the checkpoint from a fresh replica.

Likely causes: the final destination ran out of space or became unavailable;
PageBroker restarted (`TRANSACTION_NOT_FOUND`); the transaction expired.

```bash
kubectl logs daemonset/snapshot-agent -n <ns> -c pagebroker
```
