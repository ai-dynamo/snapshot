<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Native CustomStorage transfer qualification

`pagebroker-custom-storage-worker` is a standalone PageBroker GPU-engine qualification executable, not an agent-enabled storage mode or a daemon RPC endpoint. It establishes the native CustomStorage-to-transfer boundary without involving the shim, legacy IPC, CUDA jobfiles, or CRIU. Existing allocation-worker and host-carrier paths are unchanged.

The worker owns the complete CUDA operation. Native CustomStorage returns an aggregate device-memory view and stream for each participating GPU in the calling process. Those pointers cannot be sent to another process. The same worker passes the returned view to PageBroker's existing `TransferBuffers` ring, which pipelines pinned-memory CUDA copies and NIXL POSIX storage requests. It calls `cuCheckpointOperationComplete` only after transfer success. No payload digest or compression is performed.

The qualification scope is intentionally one trusted, quiescent target with one visible GPU and private allocations. `CUDA_CHECKPOINT_JOB_FILE` must be absent, and the caller must not create IPC/multicast resources or use `cuda-checkpoint --launch-job`. Tests allocate through both `cuMemAlloc` and nonexportable `cuMemCreate`; using the former does not itself create legacy IPC.

## Lifecycle

The executable takes a target PID and an existing private absolute storage directory. It retains the GPU's primary context once and accepts `save` and `load` lines on stdin:

1. `save` locks the target, calls native checkpoint with CustomStorage, validates the returned context and aggregate extent, transfers bytes to a private extent file, completes native checkpoint, and publishes the existing version-4 storage manifest.
2. `load` validates the manifest and extent before native restore, obtains fresh aggregate mappings, transfers bytes into them, completes native restore, and unlocks the target.
3. Closing stdin ends the command session, but context ownership remains until the target exits. A pidfd ties that wait to the actual process rather than a reusable PID. This preserves the context-lifetime requirement observed during prior CustomStorage qualification.

One four-slot, 64-MiB-per-slot ring is reused across operations. Storage files are opened by the worker and passed as descriptors to the common transfer engine; the engine does not resolve arbitrary paths. The existing `PAGEBROKER_ALLOCATION_DIRECT_IO=1` setting enables aligned direct I/O for this adapter too.

For controlled multi-target scheduling, `prepare-save` or `prepare-load` performs only native preparation and returns `prepared`. The subsequent `transfer` command copies the aggregate and returns `transferred`; only then can `complete` finish the native operation. Commands that violate this order fail. The original whole-operation commands use the same implementation. These test-control boundaries are not a production RPC contract.

Each target has a separate worker with both processes pinned by GPU UUID through `CUDA_VISIBLE_DEVICES`. This avoids assuming that a target's ordinal agrees with a helper's ordinal. Native preparation calls execute serially across workers. The sequential schedule prepares every target before starting concurrent transfers; the pipeline schedule starts each prepared target's transfer while preparing the next target. Both schedules join every successful transfer before sending any COMPLETE. A failure terminates all test targets rather than completing a partially transferred batch.

There is no public native abort after preparation. A failure returns no successful completion, does not unlock the target, and terminates the worker; the trusted owner must terminate the target. The GPU test harness does this in its cleanup path. This standalone executable is not suitable for untrusted requests or unattended production orchestration.

## Timing and qualification

JSON reports separate initial worker/context admission, native preparation API time, transfer wall time, transfer setup, storage-request service time, COMPLETE, and total operation wall time. Native preparation includes internal driver reconstruction/export work; it is **not** an export-only measurement. Storage-request service times can overlap, so they must not be added to the transfer wall time.

`custom_storage_gpu_test.py` verifies every byte of distinct seeded patterns before capture and after each of two save/load cycles. Run it in a one-GPU container with the worker installed and permission to checkpoint its child:

```sh
PAGEBROKER_ALLOCATION_DIRECT_IO=1 \
  python3 custom_storage_gpu_test.py /checkpoints/private-test 64
```

The size is MiB per allocation. A larger private-memory case can use `32768`, requiring at least 64 GiB of available device and storage capacity. There is no CRIU or process migration in this test: the target survives in its native CHECKPOINTED state, then resumes after native restore.

In a container assigned multiple GPUs, `--targets 2` (or `--targets 8`) runs independent private-memory targets, one per GPU. LOAD schedules run in sequential/pipeline/pipeline/sequential order to balance first-touch effects. SAVE always uses the same prepare-all schedule. Each cycle verifies every byte in every target, using distinct per-rank patterns. Reports contain native and transfer intervals on the shared monotonic clock, measured overlap, and the actual batch wall time; overlapping rank times must not be summed. Admission is reported separately and includes target allocation/seeding as well as worker startup.

The installed qualification driver is patched R615. These calls use public APIs and deliberately avoid the patched jobfile-plus-CustomStorage combination, but successful qualification on that installation does not prove compatibility with an unpatched driver.

### Patched-driver workload experiment

The explicit `--jobfile-experiment` argument permits a nonempty `CUDA_CHECKPOINT_JOB_FILE` for real-model qualification on the patched driver. It is not a production compatibility mode: the default invocation still rejects any jobfile environment. All peers remain visible to the helper for legacy IPC resolution. `PAGEBROKER_NATIVE_SELECTED_GPU` can restrict the helper's retained primary context to a known target GPU UUID; targets without a selected GPU retain all visible contexts. The helper initializes its own CUDA contexts before joining the target's checkpoint job, so helper contexts do not become members of that job.

The test owner uses `lock` on every target before issuing any `prepare-save`, keeps native preparation calls serial, and waits for every transfer before completing targets in child-before-parent order. The existing shim can use host carriers to remove shared VMM/multicast state before native capture and reconstruct it after native restore; bulk private bytes still use CustomStorage and the PageBroker engine, not the shim's allocation-storage path. This experiment does not establish stock-driver compatibility or production supervision.

## Remaining integration boundary

The daemon does not yet admit native CustomStorage sessions, bind them to transactions, or launch this executable. Production integration must supply trusted target identities and storage capabilities, own target termination on failure, and preserve context ownership until target exit. The multi-target harness qualifies independent, single-GPU targets only: a single process spanning multiple GPUs, cross-node UUID remapping, IPC dependencies, CRIU, and production failure supervision remain outside this slice. Native preparation remains serial even when transfers overlap it. No additional wire protocol or speculative backend framework is introduced.
