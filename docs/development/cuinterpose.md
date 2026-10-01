<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CuInterpose

CuInterpose adds opt-in checkpoint and restore for CUDA memory shared between
processes on one node: POSIX-exported VMM, supported synchronous memory IPC, and
multicast objects. It preserves application virtual addresses and sharing by
saving shared creator bytes in host memory captured by CRIU, then reconstructing
shared resources after native CUDA restore. Private allocations stay on the
native CUDA checkpoint path.

For a SnapshotJob, annotate its source Pod template and set the target
container's `command` explicitly:

```yaml
metadata:
  annotations:
    nvidia.com/cuinterpose-enabled: "true"
spec:
  containers:
    - name: workload
      command: ["python3", "/app/main.py"]
```

The operator installs both libraries and a static launcher. It prefixes the
command with the launcher, leaving `args` and `env` unchanged. At startup the
launcher prepends the shim to the runtime-resolved `LD_PRELOAD`, preserving
image, ConfigMap, Secret, and explicit environment precedence, then executes
the workload without a shell or parent process. Image-default entrypoints need
an explicit command. Ordinary Pods require delivery before startup: place both
libraries at `/tmp/snapshot-cuda` and preload
`/tmp/snapshot-cuda/libcuinterpose.so` yourself; an annotation alone cannot load it.

Applications must finish all CUDA calls and GPU work, keep a fixed group of fully
interposed peers, keep library files stable, and remain parked until restore
completes. Capture checks the libraries and coordinator endpoint in every CUDA
participant before preparation; removing the annotation does not disable an
active shim. Existing native CUDA jobfiles remain supported; verified shim
activation permits one to be absent.

The [workload contract](../reference/workload-contract.md#cuinterpose-synchronization-and-lifetime) collects the synchronization and lifetime requirements, including the shim's behavior when an application violates them.

Restore requires the captured SHA-256 hashes of both libraries to match the
restore agent's copies, even when compatibility checks are skipped. Use a
matching shim bundle or recreate the checkpoint after a shim upgrade. Earlier
draft checkpoints containing `cuinterpose: true` must be recreated.

Creators must retain a generic allocation handle or local mapping while their
exported descriptors or imported allocations remain usable; a virtual shareable
FD alone does not retain backing. Shared pinned VMM allocations at DEVICE and
HOST_NUMA locations use the creator's host carrier. Device bytes use asynchronous
CUDA copies; HOST_NUMA bytes use CPU copies through a temporary host-accessible
VMM alias. Restore preserves the allocation's NUMA placement separately from
the CUDA device used for an operational context.

Context destruction/reset and final primary-context release clean up converted
malloc allocations and imported IPC mappings while preserving explicit VMM
allocations. Multicast participants must add and bind their device in the same
process; device ordinals are local to each participant.

The memory-IPC adapter supports one GPU per process. Converted malloc pointers are accessible only from their allocating device; native `cuCtxEnablePeerAccess` does not grant another device VMM access. Their backing is rounded to the device's minimum VMM allocation granularity, so small allocations can consume more GPU memory than native malloc.

CUDA Runtime 11 is unsupported. Runtime driver-entry lookup requires CUDA
Runtime 12.0 or newer and fails closed when the version cannot be verified.

See [SNEP-295](../proposals/295-cuinterpose/README.md) for the motivation, supported
scope, component responsibilities, interception and sharing protocols,
capture/restore ordering, failure behavior, security, and validation plan.
See the [Rust component README](../../agent/cmd/cuinterpose/rust/README.md) for
build and test commands. [Issue #295](https://github.com/ai-dynamo/snapshot/issues/295)
tracks the implementation; current physical-GPU and cross-node qualification
remain outstanding.
