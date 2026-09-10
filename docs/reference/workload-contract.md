<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Workload contract

Snapshot checkpoints a running GPU workload and restores it later. For that to be
correct, the workload has to cooperate with the checkpoint/restore lifecycle:
reach a state that is safe to capture, signal when it is there, and resume from
the restored state on the other side. That cooperation — not any particular
image — is the requirement. This page defines it.

Building a custom image is the *reference way to package* a compliant workload,
and the [usage guides](../guides/README.md) use it because it is self-contained.
It is one method, not the requirement: any packaging that makes the container's
entrypoint satisfy this contract works.

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
| `SNAPSHOT_RESTORE_STANDBY` | producer → workload | When `1`, this process is a restore placeholder: the workload must stay inert and not initialize. |
| `<framework>-restore-ready` | workload writes | A workload-chosen sentinel meaning "restored and serving." The restore pod's readiness probe gates on it. |

The agent-owned side of this shared directory (and its `cuda-checkpoint-job`
file) is described in
[The snapshot-control volume](api.md#the-snapshot-control-volume).
The restore-pod side — annotations, standby, startup gate — is the
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

8. **MUST**, when `SNAPSHOT_RESTORE_STANDBY=1`, have the workload's entrypoint
   skip its normal initialization and idle instead (for example, sleep without
   starting the engine). The agent restores the checkpointed process into this
   container as a sibling PID via CRIU, outside the entrypoint's control; an
   entrypoint that initializes anyway starts a second, competing copy of the
   model in the same container.
9. **MUST** wait for `restore-complete` before touching the engine.
10. **MUST** bring the engine back to a serving-ready state in this order: let
    `cuda-checkpoint` restore GPU memory (CUDA contexts, streams, and device
    allocations) before resuming generation, then validate the engine responds
    correctly before serving traffic. Resuming generation before GPU memory is
    restored runs against freed memory.
11. **SHOULD** write the `<framework>-restore-ready` sentinel only after the API
    socket is actually listening, so readiness reflects true serving capacity.

### Matching configuration, per-engine calls

**MUST** keep the capture and restore processes configured identically — model,
dtype, tensor-parallel size, engine sizing, and any loader flags that change what
gets loaded or how (for example, vLLM's
[`trust_remote_code`](https://docs.vllm.ai/en/v0.27.1/configuration/engine_args.html#-trust-remote-code-no-trust-remote-code),
which permits executing a model repo's custom Python code during load). The
restored process *is* the captured process; a different configuration is
undefined.

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
| Park GPU memory | SHOULD | `sleep()` (sleep mode) | `gc.collect()`; state stays resident | `release_memory_occupation()` (memory saver) |
| Restore GPU memory | MUST | `wake_up()` | — (resident) | `resume_memory_occupation()` |
| Resume | MUST | `resume_generation()` + `check_health()` | next `generate` | `continue_generation()` |

The three are the engines the guides document, not the limit of what the
contract admits — any inference server that fills in its own column of the table
and meets the pod and runtime requirements below is snapshot-ready. See
[Support a new inference server](#support-a-new-inference-server).

## Pod requirements

The source pod gives the workload the shared directory and the conditions
checkpointing needs. The framework `deployment.yaml` files referenced from the
[usage guides](../guides/README.md) are the complete reference; the load-bearing
fields are:

- the `snapshot-control` `emptyDir`, mounted at `/snapshot-control` with `subPath`
  equal to the container name, and `SNAPSHOT_CONTROL_DIR` set to that mount;
- a seccomp profile that blocks io_uring (`profiles/block-iouring.json`), which
  CRIU cannot checkpoint — see [Security](../operations/security.md);
- the `nvidia.com/snapshot-is-checkpoint-source: "true"` label; and
- a readiness gate on `/snapshot-control/ready-for-snapshot`, so the pod reports
  Ready only once it is safe to checkpoint.

Restore pods carry different requirements — the `nvidia.com/restore-from`
annotation, an inert placeholder command, and the optional standby and
startup-gate settings.
Producing them programmatically is the
[Restore Pod contract](restore-pod-contract.md).

## Runtime compatibility

CRIU restores a process only if everything in it is checkpointable, so the
workload's *environment* — not just its logic — has to cooperate. In the
reference images this is why the build starts from the framework's tested runtime
image and sets a few environment variables. A packaging method that skips the
custom image still has to meet these:

- **glibc floor and `x86_64`.** The restore bundle requires a recent glibc, which
  the reference runtime images already clear. Snapshot is x86_64-only today.
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
   honor `SNAPSHOT_RESTORE_STANDBY`, wait on `restore-complete`, and
   write a `<framework>-restore-ready` sentinel once the API is serving.
3. **Clear the [runtime-compatibility](#runtime-compatibility) constraints** and
   **meet the [pod requirements](#pod-requirements)**, then package it by
   either method above.

Nothing about the engine's identity is special to Snapshot; satisfying the
contract is the whole requirement.

## See also

- [Usage guides](../guides/README.md) — the custom-image method, per framework.
- [Restore Pod contract](restore-pod-contract.md) — the restore-pod interface for
  programmatic restore.
- [API reference](api.md#the-snapshot-control-volume) — the control volume and
  sentinel files.
