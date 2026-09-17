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

There is no public native abort after preparation. A failure returns no successful completion, does not unlock the target, and terminates the worker; the trusted owner must terminate the target. The GPU test harness does this in its cleanup path. This standalone executable is not suitable for untrusted requests or unattended production orchestration.

## Timing and qualification

JSON reports separate initial worker/context admission, native preparation API time, transfer wall time, transfer setup, storage-request service time, COMPLETE, and total operation wall time. Native preparation includes internal driver reconstruction/export work; it is **not** an export-only measurement. Storage-request service times can overlap, so they must not be added to the transfer wall time.

`custom_storage_gpu_test.py` verifies every byte of distinct seeded patterns before capture and after each of two save/load cycles. Run it in a one-GPU container with the worker installed and permission to checkpoint its child:

```sh
PAGEBROKER_ALLOCATION_DIRECT_IO=1 \
  python3 custom_storage_gpu_test.py /checkpoints/private-test 64
```

The size is MiB per allocation. A larger private-memory case can use `32768`, requiring at least 64 GiB of available device and storage capacity. There is no CRIU or process migration in this test: the target survives in its native CHECKPOINTED state, then resumes after native restore.

The installed qualification driver is patched R615. These calls use public APIs and deliberately avoid the patched jobfile-plus-CustomStorage combination, but successful qualification on that installation does not prove compatibility with an unpatched driver.

## Remaining integration boundary

The daemon does not yet admit native CustomStorage sessions, bind them to transactions, or launch this executable. Production integration must supply trusted target identities and storage capabilities, own target termination on failure, and preserve context ownership until target exit. Multi-GPU device mapping, multi-target ordering, and overlapping earlier transfers with later serial native preparations are not implemented here. The transfer ring is pipelined; native process operations in this slice are sequential. No additional wire protocol or speculative backend framework is introduced.
