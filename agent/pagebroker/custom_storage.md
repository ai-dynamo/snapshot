<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Persistent native CustomStorage engine

PageBroker owns a long-lived `pagebroker-gpu-engine` process. Before the CPU broker serves requests, this process initializes CUDA, retains one primary context per visible GPU, and allocates and registers one NIXL transfer ring per GPU. Contexts and rings remain alive across checkpoint and restore sessions. Each ring uses 32 slots of 32 MiB (1 GiB of pinned memory per GPU). The daemon needs memory for those rings in addition to its other resources.

The agent uses native CustomStorage when a workload opts into PageBroker and cuinterpose and has CUDA processes. The shim still uses host carriers for shared creator allocations; it receives no PageBroker connections or storage settings. This native path is separate from the descriptor-based allocation-session worker.

The checkpoint manifest records `cuda.customStorage: true`. Restore reads the native payload through DirectRestore instead of copying it into staging. Before CRIU, the agent binds transaction-scoped sessions for captured CUDA PIDs. The CPU broker pins each target PID namespace and storage directory. After CRIU recreates the target, it resolves the target's host PID inside that namespace or its descendants and passes the engine a session socket and directory descriptor. The engine pins the target with a pidfd. Native aggregate pointers and streams stay in that process: it performs native preparation, transfer, and COMPLETE together.

Native preparation is serial across targets. Each prepared target begins its transfer while subsequent targets are prepared. All transfers must succeed before any target receives COMPLETE or unlock. The cuinterpose coordinator then reconstructs shared allocations and multicast state. Different GPUs transfer concurrently; sessions sharing a GPU lease the same context-specific ring. File registrations and operation handles are session resources; primary contexts, pinned buffers, CUDA events, NIXL agents, and buffer registrations are engine resources.

## Cancellation and lifetime

The agent closes session sockets before Abort. The CPU broker explicitly requests session drain and keeps transaction admission until the engine acknowledges it. A completed session releases its operation resources while the engine retains contexts. Cancelling a touched native target terminates that target through its pidfd, waits for exit, and invokes COMPLETE to destroy the abandoned caller-side native operation. It never unlocks a partially restored target. Transfers finish draining before cancellation can release their mappings or files.

There is no public native abort API. The qualified patched R615 driver destroys the caller-side operation on COMPLETE even when the dead target rejects completion. This behavior is a driver assumption of this implementation and is exercised by prepared SAVE and LOAD cancellation tests. Other live targets must remain byte-correct after cancellation.

A missing drain acknowledgment is an engine-wide fault: the CPU broker terminates and reaps the GPU engine before releasing transaction admission. An uncertain CUDA DMA result or stuck NIXL backend likewise terminates the engine. Context lifetime is therefore the daemon lifetime; an engine failure or restart can affect previously restored targets. The engine is not restarted underneath existing sessions.

Abort stops new admission and publication, then waits up to four seconds for admitted sessions to drain. That wait releases the transaction mutex. If sessions have not drained, Abort returns a conflict and preserves staging; a later Abort can finish after drain. Receiving socket EOF alone does not permit storage deletion.

## GPU identity and storage

The engine sees the node's assigned GPU set. A target may see a subset, in a different ordinal order. Native operation ownership is matched by GPU UUID and CUDA context. Restore supplies explicit source/destination UUID pairs. Empty native manifests need no new GPU resources. `CUDA_CHECKPOINT_JOB_FILE` must be absent.

Native IPC remains outside the jobfile-free qualification: cuinterpose must remove its managed shared mappings before native checkpoint. Private-memory tests allocate through both `cuMemAlloc` and nonexportable `cuMemCreate`; the former alone does not create legacy IPC.

The pinned directory confines extent resolution to the admitted transaction. The transfer adapter receives file descriptors. `PAGEBROKER_ALLOCATION_DIRECT_IO=1` enables aligned direct I/O for the NIXL POSIX adapter. No payload digest or compression is performed.

## Timing and qualification

Engine startup reports context/ring initialization separately from restore. Native session reports include native preparation API time, transfer intervals, transfer setup, storage-request service time, CUDA wait, COMPLETE, unlock, and total operation time. Native preparation includes driver reconstruction/export work. Storage-request service intervals overlap and must not be added to transfer wall time. The agent reports the full restore separately, including CRIU and shim reconstruction.

Generate Python bindings and run the real-driver test in a privileged GPU container with the engine installed:

```sh
make generate-python
PAGEBROKER_ALLOCATION_DIRECT_IO=1 \
  python3 custom_storage_gpu_test.py /checkpoints/private-test 64 --targets 3
```

The size is MiB per allocation. Targets use distinct patterns, checked over every byte after three save/restore cycles. Each target sees one GPU; the engine sees the full assigned set. Using three targets on two visible GPUs also exercises shared-ring serialization. The test then cancels prepared SAVE and LOAD operations, confirms target termination and engine survival, and verifies the remaining targets again. This test has no CRIU or process migration. Full-model agent integration separately qualifies cross-node UUID remapping, CRIU, shared-memory reconstruction, and fresh coherent inference.

Qualification uses the installed patched R615 driver; it does not establish compatibility with an unpatched driver.
