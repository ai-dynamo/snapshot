<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Native CustomStorage transfers

`pagebroker-custom-storage-worker` executes native CustomStorage operations inside the PageBroker GPU engine. The agent opts into this path with `nvidia.com/cuinterpose-allocation-storage: custom-storage`, together with enabled cuinterpose and PageBroker. Cuinterpose uses host carriers for shared VMM, multicast members, and adapted memory IPC; private CUDA state uses native CustomStorage. Existing allocation-worker and host-carrier-only modes remain separate.

The pod contract omits `--launch-job` only for this explicit mode. Native memory IPC handles are replaced by the shim's VMM-backed adapter; foreign native IPC handles are not supported. The artifact records the content mode, and restore requires matching PageBroker capabilities rather than falling back to ordinary native restore.

The worker owns the complete CUDA operation. Native CustomStorage returns an aggregate device-memory view and stream for each participating GPU in the calling process. Those pointers cannot be sent to another process. The same worker passes the returned view to PageBroker's existing `TransferBuffers` ring, which pipelines pinned-memory CUDA copies and NIXL POSIX storage requests. It calls `cuCheckpointOperationComplete` only after transfer success. No payload digest or compression is performed.

The qualification scope is intentionally one trusted, quiescent target. Its visible GPU set must match the worker's, including GPUs with no payload in parent processes. `PAGEBROKER_NATIVE_SELECTED_GPU` optionally limits retained contexts to the target's payload GPU without changing CUDA enumeration. `CUDA_CHECKPOINT_JOB_FILE` must be absent unless the explicit jobfile experiment is selected. Native IPC remains outside the jobfile-free qualification: an interposed target may use the legacy-memory adapter only after cuinterpose has removed its managed shared mappings. Private tests allocate through both `cuMemAlloc` and nonexportable `cuMemCreate`; using the former does not itself create legacy IPC.

## Lifecycle

The executable takes a target PID and an existing private absolute storage directory. It retains the GPU's primary context once and accepts `save` and `load` lines on stdin:

1. `save` locks the target, calls native checkpoint with CustomStorage, validates the returned context and aggregate extent, transfers bytes to a private extent file, completes native checkpoint, and publishes the existing version-4 storage manifest.
2. `load` validates the manifest and extent before native restore, obtains fresh aggregate mappings, transfers bytes into them, completes native restore, and unlocks the target.
3. Closing stdin ends the command session, but context ownership remains until the target exits. A pidfd ties that wait to the actual process rather than a reusable PID. This preserves the context-lifetime requirement observed during prior CustomStorage qualification.

One four-slot, 64-MiB-per-slot ring is reused across operations. Storage files are opened by the worker and passed as descriptors to the common transfer engine; the engine does not resolve arbitrary paths. The existing `PAGEBROKER_ALLOCATION_DIRECT_IO=1` setting enables aligned direct I/O for this adapter too.

For controlled multi-target scheduling, `prepare-save` or `prepare-load` performs only native preparation and returns `prepared`. The subsequent `transfer` command copies the aggregate and returns `transferred`; only then can `complete` finish the native operation. Commands that violate this order fail. The original whole-operation commands use the same implementation. These test-control boundaries are not a production RPC contract.

Each target has a separate worker with both processes pinned by GPU UUID through `CUDA_VISIBLE_DEVICES`. This avoids assuming that a target's ordinal agrees with a helper's ordinal. Native preparation calls execute serially across workers. The sequential schedule prepares every target before starting concurrent transfers; the pipeline schedule starts each prepared target's transfer while preparing the next target. Both schedules join every successful transfer before sending any COMPLETE. A failure terminates all test targets rather than completing a partially transferred batch.

There is no public native abort after preparation. A failure returns no successful completion, does not unlock the target, and terminates the worker; the trusted owner must terminate the target. The agent does this through the owning container's CRI identity. The standalone test harness terminates its own targets in cleanup. The executable is not an untrusted request interface.

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

## Transaction and process ownership

The trusted agent admits one native session per captured namespace PID against a staged PageBroker transaction. The daemon pins the target container's PID namespace and a per-target directory beneath that transaction. A bound connection accepts only LOCK, PREPARE, TRANSFER, and COMPLETE; it cannot select another storage path or a process outside that namespace subtree. At the first operation, the agent supplies the target PID as observed from the pinned placeholder namespace. This differs from the restored process's innermost PID when CRIU creates a child namespace: the placeholder and restored root can both have innermost PID 1. The daemon resolves the observed PID at the pinned namespace's depth before spawning its worker, whose target remains fixed for the session. Workers retain the full target GPU visibility and apply the agent's source-to-destination UUID map on restore.

Capture runs shim prepare, locks every native target, prepares native targets serially, transfers concurrently, and completes every target before CRIU. Restore uses the published artifact directly, restores CPU state with CRIU, prepares native targets serially, and overlaps earlier transfers with later preparation. All transfers must succeed before any COMPLETE/unlock. Shim restore then reconstructs shared mappings before the restore-complete sentinel releases application threads.

Unfinished sessions prevent transaction publication. The agent terminates the owning container through CRI on destructive failure. Failed workers are killed and reaped before transaction cleanup; successful workers relinquish storage admission but retain CUDA context ownership until their target exits. This context-lifetime requirement is independent of the artifact transaction lifetime.

The standalone whole-operation and phase commands remain useful for native GPU qualification. They are not the daemon's public protocol and do not substitute for CRIU or cross-node tests. IPC events, memory-pool IPC, and untracked sharing remain outside the supported memory-IPC adapter.
