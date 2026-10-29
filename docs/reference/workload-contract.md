<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Workload contract

Snapshot checkpoints a running GPU workload and restores it later. Capturing a
workload before it's ready produces a wrong, oversized, or unrestorable
checkpoint, so the workload must cooperate with the checkpoint/restore
lifecycle: reach a state that's safe to capture, signal when it does, and
resume correctly once restored. That cooperation — not any particular image —
is the requirement. This page defines it.

The [usage guides](../guides/README.md) package workloads as a self-contained
custom image. Other packaging methods work too — for example mounting the
entrypoint into a stock image and overriding the command — as long as the
container's entrypoint satisfies this contract; see
[Packaging methods](#packaging-methods).

A snapshot-ready workload has two parts:

- **capture and restore steps** the workload process implements, and
- **pod requirements** that give the process the shared directory and the
  runtime conditions CRIU needs.

## Coordinating with the agent

The workload and the Snapshot node agent coordinate through a shared directory: a
per-pod `emptyDir` the agent mounts into the container. The workload finds it
through an environment variable and signals across it with sentinel files.

| Name | Direction | Meaning |
|------|-----------|---------|
| `SNAPSHOT_CONTROL_DIR` | agent → workload | Path to the shared directory (mounted at `/snapshot-control`). The workload reads it here rather than hard-coding the path. |
| `ready-for-snapshot` | workload writes | The workload is quiesced and safe to checkpoint. The source pod's readiness probe gates on this file. |
| `restore-complete` | agent writes, workload waits | The workload's state is restored; it may resume. |
| `SNAPSHOT_RESTORE_STANDBY` | deprecated | Restore containers no longer run their own command, so nothing has to stay inert. Snapshot never set it. |
| `<framework>-restore-ready` | workload writes | A workload-chosen sentinel meaning "restored and serving." The restore pod's readiness probe gates on it. |

The agent-owned side of this shared directory (and its `cuda-checkpoint-job`
file) is described in
[The snapshot-control volume](api.md#the-snapshot-control-volume).
The restore-pod side — annotations, the restore proxy, startup gate — is the
[Restore Pod contract](restore-pod-contract.md).

## The capture and restore steps

These steps are built from the sentinels above. A sentinel the workload writes
(`ready-for-snapshot`, `<framework>-restore-ready`) is a *promise that a
precondition already holds*. A sentinel the workload waits on
(`restore-complete`) must not be crossed early. The whole contract reduces to
one rule:

> Raise a sentinel only once its precondition is true, and do not proceed past a
> wait until the agent's signal is observed.

Steps marked **MUST** are load-bearing for correctness — violating one produces a
wrong, oversized, or failed checkpoint. Steps marked **SHOULD** keep a correct
workload useful and operable.

### Capture (source workload)

1. **MUST** clear any stale `ready-for-snapshot` before initializing. A leftover
   file from a previous run would signal readiness before the engine is ready.
2. **SHOULD** initialize the engine before signaling readiness.
3. **SHOULD** run at least one real generation to warm the engine up before
   signaling readiness. Lazy CUDA context, autotuning, and graph capture happen
   on first use; a checkpoint taken before them omits that state, so the
   restored replica re-pays the cold start the checkpoint was meant to skip.
   Neither of steps 2-3 is load-bearing for correctness — a checkpoint of a cold
   engine still restores — but skipping them defeats the purpose of
   checkpointing.
4. **MUST** ensure no generation is in flight before signaling: pause it, or
   rely on a synchronous engine having returned.
5. **SHOULD** bring GPU memory to a checkpoint-safe state (park it) before
   signaling, once step 4 holds. Skipping this still produces a working
   checkpoint — it just captures more GPU memory than necessary, making the
   checkpoint larger and slower to restore.
6. **SHOULD** roll back to a running state if the memory release in step 5
   fails, rather than signal readiness.
7. **MUST** write `ready-for-snapshot` only once step 4 holds. This is the
   promise the rest of the system trusts; the agent captures the process as soon
   as the pod reports Ready.

### Restore (restored workload)

8. **MUST NOT** rely on its entrypoint running in the restore container.
   Snapshot replaces the restore container's command with its restore proxy,
   which waits for the restore and then follows the restored process. The
   restored process continues from the checkpoint; nothing the entrypoint does
   at startup happens again. On nodes without the restore proxy, the restore
   Pod's own inert command runs instead; see
   [Nodes without the restore proxy](restore-pod-contract.md#nodes-without-the-restore-proxy).
9. **MUST** wait for `restore-complete` before touching the engine.
10. **MUST** bring the engine back to a serving-ready state in this order: let
    `cuda-checkpoint` restore GPU memory (CUDA contexts, streams, and device
    allocations) before resuming generation, then validate the engine responds
    correctly before serving traffic. Resuming generation before GPU memory is
    restored runs against freed memory.
11. **SHOULD** write the `<framework>-restore-ready` sentinel only after the API
    socket is actually listening, so readiness reflects true serving capacity.

### Matching configuration, per-engine calls

**MUST** keep the capture and restore processes configured identically — the
restored process *is* the captured process; a different configuration is
undefined. This is easy to overlook for loader flags that change what gets
loaded or how, for example vLLM's
[`trust_remote_code`](https://docs.vllm.ai/en/v0.27.1/configuration/engine_args.html#-trust-remote-code-no-trust-remote-code),
which permits executing a model repo's custom Python code during load.

Steps 3-6 (capture: warm up, quiesce) and step 10 (restore: bring the engine
back to serving) each break down into the same sub-requirements across
engines — the table below lists what each engine calls to meet them.
Different frameworks expose different function names for the same
requirement, which is why these steps define the contract in terms of *what
must happen*, not any one engine's API. Tiers carry over from their parent
step: skipping a **MUST** row breaks capture or restore outright (for
example, checkpointing with a generation in flight, or resuming before GPU
memory is restored, fails); skipping the **SHOULD** row still produces a
working checkpoint, just a larger or colder one.

| Requirement | Tier | vLLM | TensorRT-LLM | SGLang |
|------------|------|------|--------------|--------|
| Warm up | SHOULD | one `generate` | `LLM.generate` (two prompts) | one `generate` |
| Stop in-flight work | MUST | `pause_generation()` | synchronous `generate` returns idle | `pause_generation()` |
| Park GPU memory | SHOULD | `release_kv_cache_memory()` (sleep mode) | `_collective_rpc("sleep", (["kv_cache"],))` + `gc.collect()` | `release_memory_occupation(tags=["kv_cache"])` (memory saver) |
| Restore GPU memory | MUST | `wake_up()` | `_collective_rpc("wakeup", (["kv_cache"],))` | `resume_memory_occupation(tags=["kv_cache"])` |
| Resume | MUST | `resume_generation()` + `check_health()` | next `generate` | `continue_generation()` |

The three are the engines the guides document, not the limit of what the
contract admits — any inference server that fills in its own column of the table
and meets the pod and runtime requirements below is snapshot-ready. See
[Support a new inference server](#support-a-new-inference-server).

## Lifecycle hooks

Instead of the readiness gate and `restore-complete`, a workload can name its
own signals in the `PodSnapshot`'s
[container settings](api.md#container-settings):

- `checkpointReadinessProbe`: a file, or an HTTP GET that returns `200`, that
  says the workload is safe to checkpoint. It replaces the Ready condition for
  this purpose, so the readiness probe can mean "ready to serve". Step 7 applies
  to it: succeed only once step 4 holds.
- `lifecycle.postRestore`: after the restore, Snapshot creates a file, or sends
  a POST with an empty body, that tells the workload it may resume. It replaces
  step 9's wait on `restore-complete`.

A workload that uses them:

- **MUST** keep a file on the container's own filesystem or on an `emptyDir`
  volume, on a path with no symlink. Snapshot refuses `hostPath`,
  `PersistentVolumeClaim`, and other volumes.
- **MUST NOT** have the `postRestore` file in place before the restore, not even
  in the checkpointed filesystem: the restore fails if it exists. A path on an
  `emptyDir` that only the restore Pod mounts avoids this. In a restore Pod with
  several destination containers, give each its own path, for example with
  `subPath`.
- **MUST** answer HTTP on all of the container's addresses, not only the source
  Pod's IP: requests go to the Pod IP, which changes on restore. A NetworkPolicy
  must let the node agent reach the port.
- **MUST** answer the `postRestore` POST with `200` or `204` and handle it more
  than once. Snapshot retries until it gets one, and repeats it after an agent
  restart.

## Pod requirements

The source pod gives the workload the shared directory and the conditions
checkpointing needs. The framework capture manifests referenced from the
[usage guides](../guides/README.md) are the complete reference; the load-bearing
fields are:

- the `snapshot-control` `emptyDir`, mounted at `/snapshot-control` with `subPath`
  equal to the container name, and `SNAPSHOT_CONTROL_DIR` set to that mount;
- a seccomp profile that blocks io_uring (`profiles/block-iouring.json`), which
  CRIU cannot checkpoint — see [Security](../operations/security.md); and
- a readiness gate on `/snapshot-control/ready-for-snapshot`, so the pod reports
  Ready only once it is safe to checkpoint.

Restore pods carry different requirements — the `nvidia.com/restore-from`
annotation, the source's command (Snapshot replaces it with its restore proxy),
`restartPolicy: Never`, and the optional startup gate.
Producing them programmatically is the
[Restore Pod contract](restore-pod-contract.md).

## Multi-GPU workloads

A workload that uses several GPUs meets the same contract, with its GPUs on a
single node; multi-node workloads are not supported. Which multi-GPU workloads
can be restored depends on the CUDA user-mode driver version; see
[Compatibility](compatibility.md). The source Pod also enables multi-GPU support
with the `nvidia.com/snapshot-multi-gpu: enabled` annotation. What else it
carries depends on what creates it:

- **A `SnapshotJob`** needs only the annotation, on
  `spec.podTemplate.metadata.annotations`. The controller prepares the source Pod
  for multi-GPU capture.
- **A pod that Snapshot does not create** — such as a Deployment checkpointed
  through a `PodSnapshot` — must prepare itself. It copies the multi-GPU
  libraries from the Snapshot agent image with an init container, and starts the
  workload through the launcher those libraries include:

  ```yaml
  template:
    metadata:
      annotations:
        nvidia.com/snapshot-multi-gpu: enabled
    spec:
      initContainers:
        - name: snapshot-cuda-install
          image: ${SNAPSHOT_AGENT_IMAGE}
          command:
            - /bin/cp
          args:
            - --preserve=mode
            - --
            - /usr/local/lib/snapshot/libcuinterpose.so
            - /usr/local/lib/snapshot/libcuinterpose_core.so
            - /usr/local/bin/cuinterpose-launch
            - /tmp/snapshot-cuda/
          volumeMounts:
            - name: snapshot-cuda
              mountPath: /tmp/snapshot-cuda
      containers:
        - name: main
          command:
            - /tmp/snapshot-cuda/cuinterpose-launch
            - --library
            - /tmp/snapshot-cuda/libcuinterpose.so
            - --
            - python3
            - /snapshot-app/app.py
          volumeMounts:
            - name: snapshot-cuda
              mountPath: /tmp/snapshot-cuda
              readOnly: true
      volumes:
        - name: snapshot-cuda
          emptyDir: {}
  ```

The eight-GPU recipes in the [usage guides](../guides/README.md#choose-your-recipe)
are complete examples of both.

## Runtime compatibility

CRIU restores a process only if everything in it is checkpointable, so the
workload's *environment* — not just its logic — has to cooperate. In the
reference images this is why the build starts from the framework's tested runtime
image and sets a few environment variables. A packaging method that skips the
custom image still has to meet these:

- **glibc floor and architecture.** The restore bundle requires a recent glibc,
  which the reference runtime images already clear. The workload image must
  match the node architecture (x86_64 or arm64).
- **A target CPU with every instruction set the source had.** A process picks
  its code paths for the CPU it starts on. If the restored process runs on a CPU
  without one of those instructions, it dies with `SIGILL`. The agent records
  the instruction sets the source node offers, its CPU flags on x86_64 and its
  hwcaps on arm64, and refuses a restore onto a node missing any of them, naming
  the missing flags (`cpu-features: source avx512f, target missing`). A target
  that offers more is fine. Only the instruction sets in Snapshot's list are
  compared, so a restore onto a CPU missing one it does not list, such as
  `waitpkg` on x86_64, goes ahead. The check needs both lists, so a checkpoint
  taken before the agent recorded them, or a restore onto a node whose agent
  could not read its own, is admitted unchecked.
- **All file handles must be reopenable.** Disable caches that leave handles CRIU
  cannot reopen after restore — for example `HF_HUB_DISABLE_XET=1`, and loading
  models from a local cache with `HF_HUB_OFFLINE=1`.
- **All device mappings must be restorable.** Turn off transports CRIU cannot
  restore — for example TensorRT-LLM's `TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` and
  `UCX_TLS=tcp,self`.
- **`spawn`, not `fork`.** Multiprocess engines start workers with `spawn` (for
  example `VLLM_WORKER_MULTIPROC_METHOD=spawn`). This is a CUDA limitation:
  forking a process that already holds a CUDA context produces a child with an
  unreliable copy of that context, so a worker forked before checkpoint may not
  restore correctly.

## Packaging methods

- **Custom image (reference).** Start from the framework runtime image, add a
  small entrypoint that implements the capture and restore steps, and set it as
  the command. The [usage guides](../guides/README.md) walk through this for
  vLLM, SGLang, and TensorRT-LLM.
- **Any equivalent.** Mounting the entrypoint into a stock image and overriding
  the command, or a framework that implements these steps natively, is equally
  valid — provided the running container satisfies this contract and the runtime
  compatibility constraints above.

## Support a new inference server

The documented engines are examples, not the boundary: any inference server that
satisfies this contract is snapshot-ready, and the node agent checkpoints and
restores it with no Snapshot-side change. To bring one:

1. **Map each requirement to the engine's API** — fill in its own column of the
   [table](#matching-configuration-per-engine-calls): warm up, stop in-flight
   work, park and restore GPU memory, resume. Any mechanism qualifies as long as
   it meets the requirement; an engine with no explicit memory-park call can
   rely on a synchronous request returning idle, as TensorRT-LLM does.
2. **Implement the capture and restore steps over the shared directory** — read
   `SNAPSHOT_CONTROL_DIR`, clear then write `ready-for-snapshot` once quiesced,
   wait on `restore-complete`, and
   write a `<framework>-restore-ready` sentinel once the API is serving.
3. **Clear the [runtime-compatibility](#runtime-compatibility) constraints** and
   **meet the [pod requirements](#pod-requirements)**, then package it by
   either method above.

Nothing about the engine's identity is special to Snapshot; satisfying the
contract is the whole requirement.

## See also

- [Usage guides](../guides/README.md) — a worked example per framework.
- [Restore Pod contract](restore-pod-contract.md) — the restore-pod interface for
  programmatic restore.
- [API reference](api.md#the-snapshot-control-volume) — the control volume and
  sentinel files.
