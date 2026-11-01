<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshoot Checkpoints

## A Checkpoint Never Becomes Ready

A `PodSnapshot` becomes Ready only after the `snapshot-agent` confirms the
checkpoint contents — writing the checkpoint is not enough on its own. Check the
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
guides](../guides/README.md) -- missing the control volume, seccomp profile,
or readiness gate Snapshot relies on -- or that omits mounts or secrets the
replica needs to start.

## Checkpoint Fails at a Step

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

Likely causes: the `pagebroker` container is not ready; the snapshot volume
is full or unavailable.

```bash
kubectl logs $agent -n <snapshot-ns> -c pagebroker
kubectl get pvc -n <snapshot-ns>
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
when the seccomp profile from the supported frameworks is missing, a socket to a
peer outside the container, or an open file on a volume the checkpoint did not
record; missing ptrace permission.

The agent log carries the CRIU error. Check the source pod against the
[supported frameworks](../guides/README.md).

### capturing rootfs diff

After the dump, the agent records the container's upper directory as a rootfs
diff, keeping every file the dump references even when an exclusion pattern
matches it. Other diff problems are logged and the checkpoint goes on without
them; the dump has already stopped the source, so a failure here means
re-taking the checkpoint from a fresh replica.

Likely causes: the dump's image files cannot be read back from the staging
directory; a file the dump references is missing from the upper directory, for
example one deleted or replaced between the dump and the capture.

```bash
kubectl logs $agent -n <snapshot-ns> -c agent | grep -i 'rootfs diff'
```

### committing PageBroker checkpoint

PageBroker publishes the staged artifact to its final location. The dump has
already stopped the source process at this point, so a failure here means
re-taking the checkpoint from a fresh replica.

Likely causes: the final destination ran out of space or became unavailable;
PageBroker restarted (`TRANSACTION_NOT_FOUND`); the transaction expired.

```bash
kubectl logs $agent -n <snapshot-ns> -c pagebroker
```
