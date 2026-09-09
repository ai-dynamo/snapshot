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

- a **lifecycle protocol** the workload process implements, and
- a **pod shape** that gives the process the control channel and the runtime
  conditions CRIU needs.

## The control channel

The workload and the Snapshot node agent coordinate through a shared directory: a
per-pod `emptyDir` the agent mounts into the container. The workload finds it
through an environment variable and signals across it with sentinel files.

| Name | Direction | Meaning |
|------|-----------|---------|
| `SNAPSHOT_CONTROL_DIR` | agent → workload | Path to the control directory (mounted at `/snapshot-control`). The workload reads it here rather than hard-coding the path. |
| `ready-for-snapshot` | workload writes | "I am quiesced and safe to checkpoint." The source pod's readiness probe gates on this file. |
| `restore-complete` | agent writes, workload waits | "Your state is restored; you may resume." |
| `SNAPSHOT_RESTORE_STANDBY` | producer → workload | When `1`, this process is a restore placeholder: stay inert and do not initialize. |
| `<framework>-restore-ready` | workload writes | A workload-chosen sentinel meaning "restored and serving." The restore pod's readiness probe gates on it. |

The agent-owned side of this channel (and its `cuda-checkpoint-job` file) is
described in [The snapshot-control volume](api.md#the-snapshot-control-volume).
The restore-pod side — annotations, standby, startup gate — is the
[Restore Pod contract](restore-pod-contract.md).

## The lifecycle protocol

The protocol is a sequence of **barriers**. Each up-signal the workload raises is
a *promise that a precondition already holds*; each down-signal it waits on is a
*barrier it must not cross early*. The whole contract reduces to one rule:

> Raise a sentinel only once its precondition is true, and do not proceed past a
> wait until you observe the agent's signal.

Steps marked **MUST** are load-bearing for correctness — violating one produces a
wrong, oversized, or failed checkpoint. Steps marked **SHOULD** keep a correct
workload useful and operable.

### Capture (source workload)

1. **MUST** clear any stale `ready-for-snapshot` before initializing. A leftover
   file from a previous run would signal readiness before the engine is ready.
2. **MUST** initialize the engine, and **SHOULD** run at least one real
   generation to warm it up. Lazy CUDA context, autotuning, and graph capture
   happen on first use; a checkpoint taken before them omits that state, so the
   restored replica re-pays the cold start the checkpoint was meant to skip.
3. **MUST** quiesce before signaling: ensure no generation is in flight (pause
   it, or rely on a synchronous engine having returned), then bring GPU memory to
   a checkpoint-safe state. Where both apply, stop work before releasing memory,
   and **SHOULD** roll back to a running state if the release fails rather than
   signal readiness.
4. **MUST** write `ready-for-snapshot` only once step 3 holds. This is the
   promise the rest of the system trusts; the agent captures the process as soon
   as the pod reports Ready.

### Restore (restored workload)

5. **MUST**, when `SNAPSHOT_RESTORE_STANDBY=1`, block without initializing. The
   agent injects the restored process into this container as a sibling; a process
   that initializes anyway loads a second copy of the model and collides with it.
6. **MUST** wait for `restore-complete` before touching the engine.
7. **MUST** rehydrate before serving, in order: restore GPU memory (wake), then
   resume generation, then validate. Resuming generation before memory is mapped
   runs against freed memory.
8. **SHOULD** write the `<framework>-restore-ready` sentinel only after the API
   socket is actually listening, so readiness reflects true serving capacity.

### Config parity and mechanism

**MUST** keep the capture and restore processes configured identically — model,
dtype, tensor-parallel size, `trust_remote_code`, and engine sizing. The restored
process *is* the captured process; a different configuration is undefined.

Steps 2, 3, and 7 are *obligations*, not specific calls. The three reference
workloads meet the same obligations through different framework mechanisms —
which is why the protocol, not any one engine's API, is the contract:

| Obligation | vLLM | TensorRT-LLM | SGLang |
|------------|------|--------------|--------|
| Warm up | one `generate` | `LLM.generate` (two prompts) | one `generate` |
| Stop in-flight work | `pause_generation()` | synchronous `generate` returns idle | `pause_generation()` |
| Park GPU memory | `sleep()` (sleep mode) | `gc.collect()`; state stays resident | `release_memory_occupation()` (memory saver) |
| Restore GPU memory | `wake_up()` | — (resident) | `resume_memory_occupation()` |
| Resume | `resume_generation()` + `check_health()` | next `generate` | `continue_generation()` |

## Pod requirements

The source pod gives the workload the control channel and the conditions
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

Restore pods carry a different shape — the `nvidia.com/restore-from` annotation,
an inert placeholder command, and the optional standby and startup-gate settings.
Producing them programmatically is the
[Restore Pod contract](restore-pod-contract.md).

## Runtime compatibility

CRIU restores a process only if nothing in it is un-checkpointable, so the
workload's *environment* — not just its logic — has to cooperate. In the
reference images this is why the build starts from the framework's tested runtime
image and sets a few environment variables. A packaging method that skips the
custom image still has to meet these:

- **glibc floor and `x86_64`.** The restore bundle requires a recent glibc, which
  the reference runtime images already clear. Snapshot is x86_64-only today.
- **No un-reopenable file handles.** Disable caches that leave handles CRIU
  cannot reopen after restore — for example `HF_HUB_DISABLE_XET=1`, and loading
  models from a local cache with `HF_HUB_OFFLINE=1`.
- **No un-restorable device mappings.** Turn off transports CRIU cannot restore —
  for example TensorRT-LLM's `TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` and
  `UCX_TLS=tcp,self`.
- **`spawn`, not `fork`.** Multiprocess engines start workers with `spawn` (for
  example `VLLM_WORKER_MULTIPROC_METHOD=spawn`); `fork` is unreliable across
  checkpoint/restore.

## Packaging methods

- **Custom image (reference).** Start from the framework runtime image, add a
  small entrypoint that implements the lifecycle protocol, and set it as the
  command. The [usage guides](../guides/README.md) walk through this for vLLM,
  SGLang, and TensorRT-LLM.
- **Any equivalent.** Mounting the entrypoint into a stock image and overriding
  the command, or a framework that implements the protocol natively, is equally
  valid — provided the running container satisfies this contract and the runtime
  compatibility constraints above.

## See also

- [Usage guides](../guides/README.md) — the custom-image method, per framework.
- [Restore Pod contract](restore-pod-contract.md) — the restore-pod interface for
  programmatic restore.
- [API reference](api.md#the-snapshot-control-volume) — the control volume and
  sentinel files.
