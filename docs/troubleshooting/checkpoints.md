<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshoot Checkpoints

Use this page when a checkpoint does not finish. It starts with a `PodSnapshot`
that never becomes Ready. Then it lists each step a checkpoint can fail at, with
the likely causes and what to check. To find which step failed, see
[Reading a Failure Message](reading-a-failure-message.md).

## A Snapshot Never Becomes Ready

A `PodSnapshot` becomes Ready only after the `snapshot-agent` confirms the
snapshot contents. Writing the snapshot is not enough on its own. Check the
status and the agent logs:

```bash
kubectl get podsnapshot <name> -n <ns>
node=$(kubectl get pod <source-pod> -n <ns> -o jsonpath='{.spec.nodeName}')
agent=$(kubectl get pods -n <snapshot-ns> -l app.kubernetes.io/component=snapshot-agent \
  --field-selector spec.nodeName=$node -o name)
kubectl logs $agent -n <snapshot-ns> --all-containers
```

The agent that handled the checkpoint is the one on the source pod's node;
reading the DaemonSet picks an arbitrary pod. [Reading a failure
message](reading-a-failure-message.md) explains the lookup.

A common cause is a replica manifest that does not follow the [framework
guides](../using-snapshot/README.md) -- missing the control volume, seccomp profile,
or readiness gate Snapshot relies on -- or that omits mounts or secrets the
replica needs to start.

When the container has a `checkpointReadinessProbe`, the agent waits for that
probe, not for Ready, and the source pod has a `WaitingForCheckpointReadiness`
event. The agent logs each failed check at debug level. Check a file probe from
inside the source container. Check an HTTP probe against the pod's IP and port
from another pod or the node, not against `localhost` inside the container: the
agent sends the request from the node to the pod's IP. The wait has no deadline
of its own, so a probe that never passes waits until you delete the
`PodSnapshot`, or until the source Pod ends: it fails, exits or is deleted, or
a `SnapshotJob`'s `activeDeadlineSeconds` stops it. The `PodSnapshot` then
fails at most five minutes after the Pod ended.

## A Checkpoint Fails with `InvalidCheckpointReadinessProbe`

The agent refused the container's `checkpointReadinessProbe` before running it,
because it can never pass: the container has no port with the probe's port
name, or the probe's HTTP path does not parse as a URL path. The source pod also
gets a Warning event with this reason.

The `PodSnapshot`'s spec cannot change, so create a new one with a fixed probe.
If a `SnapshotJob` created it, create a new `SnapshotJob` with the probe fixed
in `podSnapshotTemplate.containers` instead: the `SnapshotJob` copies its
template into the `PodSnapshot` it creates.

## A Checkpoint Fails with `RetriesExhausted`

The checkpoint was retried as often as its retry limit allows and failed every
time. The limit comes from the request: `spec.checkpointRetryLimit` on a
`PodSnapshot`, or `spec.podSnapshotTemplate.checkpointRetryLimit` on a
`SnapshotJob`. When neither sets one, `config.checkpoint.defaultRetryLimit`
applies.

Only a failure another attempt could have cleared ends under this reason. One
that settled on its own terms keeps its own reason however many retries ran
first, so the reason does not say how many attempts there were. The message
does: any failure that followed at least one retry names the retries it spent
and the last cause, as `after 2 of 2 retries: <cause>`. Treat the cause as the
failure; the retries only say it did not clear on its own.

```bash
kubectl get podsnapshotcontent <name> -o jsonpath='{.status.conditions[?(@.type=="Failed")].message}'
kubectl get events -n <ns> --field-selector involvedObject.name=<source-pod>
```

Each attempt a retry follows emits a `CheckpointAttemptFailed` event on the
source pod, including the repeats of the dump, so the events show what each
attempt hit rather than only how the checkpoint ended. The attempt that ended
it reports under the terminal reason instead, so the last failure is not one of
them.

Whether the workload is still running is reported separately. See
[After a Failed Checkpoint, Is the Workload Still Running?](#after-a-failed-checkpoint-is-the-workload-still-running).

## A Checkpoint Fails with `ImageTooLarge`, `StagingUnavailable`, or `StoreExhausted`

Three different shortages, which need different fixes:

| Reason | What ran out | What to do |
| --- | --- | --- |
| `StagingUnavailable` | The node's staging volume was occupied when the checkpoint needed it. An image the size check could not prove too large lands here too, since that check reads a floor rather than the final size. Retried only when the retry limit allows one, which by default it does not | Take the checkpoint again once staging frees. If it keeps happening, stagger the checkpoints sharing that node, or raise the retry limit so the agent waits for them: `spec.checkpointRetryLimit` on the `PodSnapshot` or `SnapshotJob`'s `podSnapshotTemplate`, or `config.checkpoint.defaultRetryLimit` for requests that set none. If it keeps happening against an empty volume, treat it as `ImageTooLarge`: the image does not fit after all |
| `ImageTooLarge` | The image cannot fit the node's staging volume, however empty it is. The message gives the shortfall in bytes | Raise `pageBroker.staging.sizeLimit`. The chart lowers a cap its writers have no room under, to the smaller of `daemonset.resources.limits.memory` and `pageBroker.resources.limits.memory` less a reserve, so raise both of those with it. See [How the Cap Is Chosen](../operate/configure-storage.md#how-the-cap-is-chosen) |
| `StoreExhausted` | The destination store is full. No retry on this node frees room there | Free space in the store, or point the checkpoint at one with room |

`ImageTooLarge` is reported two ways: refused before the dump starts, from a
floor on what the image must write, or after a dump has run out of room
part-written. Both name the same shortfall.

Whether the workload is still running after any of these: see
[After a Failed Checkpoint, Is the Workload Still Running?](#after-a-failed-checkpoint-is-the-workload-still-running).

## After a Failed Checkpoint, Is the Workload Still Running?

A failure after the workload was suspended terminates the source Pod, unless the
agent resumes it for another attempt. The outcome is reported on that pod:
`SourcePodResumed` when the agent put it back for another attempt,
`SourcePodTerminated` when the checkpoint ended, and `SourcePodTerminationFailed`
when it could not be stopped.

```bash
kubectl get events -n <ns> --field-selector involvedObject.name=<source-pod>
```

## A `SnapshotJob` Fails with `JobNameConflict`

A `SnapshotJob` runs its source Pod in a Job that has the name of the
`SnapshotJob`. The `SnapshotJob` fails with `JobNameConflict` when a Job that it
did not create has that name. The operator does not change or delete that Job.

Kubernetes deletes the source Job a moment after its `SnapshotJob`. A
`SnapshotJob` that you create again under the same name can find that earlier
Job. The new `SnapshotJob` then waits until Kubernetes deletes the Job, for at
most two minutes after its creation. During the wait, its `Running` condition
is `False` with reason `WaitingForPreviousJobDeletion`. If the earlier Job is
still there after the wait, the new `SnapshotJob` fails with `JobNameConflict`.

To see what keeps the Job, read its owner references and its finalizers:

```bash
kubectl get job <name> -n <ns> -o jsonpath='{.metadata.ownerReferences}{"\n"}{.metadata.finalizers}{"\n"}'
```

Delete the Job, or remove what keeps it, such as a finalizer. Then delete the
failed `SnapshotJob` and create it again. A failed `SnapshotJob` does not start
again.

## Checkpoint Fails at a Step

### resolving checkpoint artifact path

The agent computes where the artifact will live under the storage base path
from the content UID and container name.

**Likely causes:**

- A `PodSnapshotContent` with an empty or malformed content UID
- A misconfigured storage base path

### inspecting source container

The agent resolves the container's PID and OCI spec through the node runtime,
reads its rootfs, overlay upper directory, mounts and network namespace, and
discovers its CUDA processes and GPUs.

**Likely causes:**

- The container exited before the agent reached it
- The runtime uses a snapshotter without an overlay upper directory
- CUDA processes sit in nested PID namespaces
- GPU discovery failed inside the container

```bash
kubectl get pod <source-pod> -n <ns> -o jsonpath='{.status.containerStatuses}'
```

If the cause is `unsupported NVIDIA_VISIBLE_DEVICES selection` with
`/var/run/nvidia-container-devices` or `/run/nvidia-container-devices`, the
agent lacks support for the NVIDIA device plugin's `volume-mounts` strategy.
Upgrade the node agent to a release containing the GPU discovery fix; no
workload or device-plugin configuration changes are required.

### preparing PageBroker checkpoint

The agent asks PageBroker to open a transaction and provide the staging or
destination directory. The cause carries PageBroker's failure code, such as
`INSUFFICIENT_STORAGE` or `STORAGE_ERROR`, or a connection error.

**Likely causes:**

- The `pagebroker` container is not ready
- The snapshot volume is full or unavailable

```bash
kubectl logs $agent -n <snapshot-ns> -c pagebroker
kubectl get pvc -n <snapshot-ns>
```

### inspecting cuinterpose

When the workload opted into the cuinterpose shim, every CUDA process must have
loaded it, its delivery mount must be read-only, and the coordinator must
answer before the checkpoint becomes destructive.

**Likely causes:**

- A CUDA process started without the shim
- The delivery mount is writable
- The coordinator is not running

### admitting the pod group

For a member of a pod group, the agent publishes the Pod's IP, then waits for
every other member's and for every member to accept. The cuInterpose coordinator
of every Pod that runs one inspects as its Pod's relay in the group, and the
lowest-ranked relay checks the group's topology. Nothing has changed in any
workload yet, so the attempt ends as `Refused` and every member is left as
Snapshot found it. A member that fails at any step fails the attempt for every
other member. Each of them then fails at the step it was waiting in, with the
reason the first member gave, such as `Refused` or `MemberFailed`.

**Likely causes:**

- The Pod has no IP yet
- Another member refused first
- The attempt's deadline passed before every member arrived
- The cuInterpose relay refused the group's topology, for example memory one Pod imports that no member exported, which its error names

### staging CUDA job file

For a driver-managed GPU checkpoint, the agent copies the CUDA checkpoint job
file out of the container and checks that it lists every GPU.

**Likely causes:**

- The workload has not reached its quiescent point, so the job file is missing or stale
- The GPU count in the file differs from the devices discovered

### configuring CRIU dump and writing checkpoint manifest

The agent builds CRIU's dump options from the inspected container and writes
`manifest.json` into the staging directory.

**Likely causes:**

- A mount layout CRIU options cannot express
- A staging directory that is not writable

### checkpointing GPU state

The agent checkpoints GPU memory for each CUDA process, through the driver's
checkpoint tool or through PageBroker for CustomStorage, after preparing
cuinterpose when the shim is in use. A failure here, or in the CRIU dump that
follows, leaves the source process unsafe to resume, so the agent terminates
it.

**Likely causes:**

- A kernel still running or a stream still busy when the lock was taken
- A driver without checkpoint support for this GPU
- Another process holding the device, such as a GPU monitoring agent

### waiting for the pod group's other members

Once its GPU state is checkpointed, a member of a pod group waits at the
`net-freeze` barrier until every other member arrives, so no member's network
freezes while another member still needs it. The step fails when the attempt
ends first. The source has stopped by then.

**Likely causes:**

- Another member failed, and its agent's log carries the step
- The attempt's deadline passed
- The coordinator was lost (`CoordinatorLost`)

```bash
kubectl logs $agent -n <snapshot-ns> -c agent | grep -i 'pod group'
```

### dumping with CRIU

CRIU dumps the process tree into the staging directory. The cause is the error
CRIU logged.

**Likely causes:**

- A file descriptor CRIU cannot dump, such as an io_uring instance when the seccomp profile from the supported frameworks is missing, a socket to a peer outside the container, or an open file on a volume the checkpoint did not record
- Missing ptrace permission

The agent log carries the CRIU error. Check the source pod against the
[supported frameworks](../using-snapshot/README.md).

### capturing rootfs diff

After the dump, the agent records the container's upper directory as a rootfs
diff, keeping every file the dump references even when an exclusion pattern
matches it. Other diff problems are logged and the checkpoint goes on without
them; the dump has already stopped the source, so a failure here means
re-taking the checkpoint from a fresh replica.

**Likely causes:**

- The dump's image files cannot be read back from the staging directory
- A file the dump references is missing from the upper directory, for example one deleted or replaced between the dump and the diff

```bash
kubectl logs $agent -n <snapshot-ns> -c agent | grep -i 'rootfs diff'
```

### committing PageBroker checkpoint

PageBroker publishes the staged artifact to its final location. The dump has
already stopped the source process at this point, so a failure here means
re-taking the checkpoint from a fresh replica.

**Likely causes:**

- The final destination ran out of space or became unavailable
- PageBroker restarted (`TRANSACTION_NOT_FOUND`)
- The transaction expired

```bash
kubectl logs $agent -n <snapshot-ns> -c pagebroker
```
