<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Explicit allocation transfer sessions

PageBroker can save and load externally supplied CUDA VMM backing without
enabling native CUDA CustomStorage. This is a broker-side building block:
the Snapshot agent and Rust cuinterpose caller are not connected to it yet.
Ordinary PageBroker filesystem transactions do not require CUDA.

## Session boundary

The trusted agent creates a staged checkpoint or restore transaction through
the existing broker protocol. On a new connection it sends
`Request.bind_allocations` with the transaction ID, a lowercase 128-bit
participant ID, and `SAVE` or `LOAD`. The broker verifies the transaction's
direction and starts a CUDA worker before acknowledging the binding.

The connected socket is now an allocation-only capability. It accepts
`AllocationSessionRequest`, not general broker requests. The agent can pass
this descriptor into the target namespace; the node-wide broker socket must
not be mounted into the workload. A participant can bind only once per
transaction. Independent participants have independent connections and workers.

Frames use the existing four-byte, big-endian protobuf length prefix and
64-KiB size limit. All descriptors accompany the first prefix bytes through
`SCM_RIGHTS`; descriptors in the body, truncated control messages, and excess
rights are rejected and closed. Each batch contains at most 32 allocations
with exactly one CUDA export descriptor per allocation, in order. A session
accepts at most 65,536 allocations and a 16-MiB manifest.

The broker chooses all storage paths. A client supplies only allocation ID,
size, and a 16-byte CUDA device UUID. On restore the UUID identifies the
destination device; the manifest preserves the captured source UUID.
The caller must establish the source-to-destination device mapping.

## Worker and completion

`--allocation-worker /absolute/path/to/pagebroker-allocation-worker` enables
session binding. Omitting it leaves the existing filesystem daemon behavior
unchanged and allocation binding unavailable. There is no fallback to host
carriers or another transfer engine.

Workers are started with `posix_spawn`, never by forking a CUDA-initialized
broker. The private connection occupies FD 3; unrelated inherited descriptors
are closed. The worker initializes CUDA before announcing readiness.

The worker receives the same protobuf extent schema plus the direction.
Attached descriptors are ordered as `[CUDA exports..., storage files...]`.
Storage files are opened by the broker and checked again by the transfer
adapter, rather than reopened from workload-provided paths.

For each allocation the worker selects a visible device by UUID, imports the
VMM handle, checks its pinned/device properties, maps a worker-local address,
and calls the transfer-neutral `TransferExtent` interface. It reuses the
bounded POSIX transfer-ring design from the PageBroker CUDA foundation.
The backend pipelines D2H/H2D operations and storage I/O through pinned host
buffers and computes SHA-256 during the sole content pass. It is not a
GPU-direct storage backend.

Allocations within one batch are processed serially, with a 64-MiB transfer
slot; participant sessions run concurrently. This deliberately bounds memory
without introducing a worker pool. Grouping small allocations and reusing
contexts/streams is a future performance improvement at the same interface.

A batch succeeds only after transfers and CUDA cleanup complete and the
worker and broker drop their temporary export FDs. Failed operations terminate
and reap the disposable worker before admission can be released. No failed
load is retried on partially populated backing. A five-minute socket timeout
bounds stalled exchanges; the transfer loop also checks a four-minute
cooperative deadline. Reaping a killed worker still waits for OS process exit:
transaction cleanup must not race a process retaining CUDA references.

## Artifact and transaction lifetime

Content lives at:

```text
<transaction staging>/allocations/<participant-id>/<allocation-id>
<transaction staging>/allocations/<participant-id>/manifest.pb
```

`AllocationManifest` version 1 records the participant, allocation IDs,
sizes, source device UUIDs, and digests. Save batches fsync content; `finish`
atomically publishes and fsyncs the complete participant manifest. Load binding
validates the manifest and regular-file sizes before accepting allocation FDs.
Load completion requires exact coverage of the saved allocation set. Digest
verification occurs during transfer, so corruption may be detected **after
GPU writes**; the caller must keep the workload parked and fail closed.

Transaction mutexes protect short admission/state transitions, not the GPU
transfer. Commit refuses active or failed/incomplete sessions. Abort returns
`TRANSACTION_CONFLICT` while sessions are active: close the bound connections,
wait for their workers to exit, then retry Abort. Expiry skips active sessions.
Disconnect before `finish` makes publication invalid, even if previous batches
succeeded. The trusted caller remains responsible for binding and completing
every expected participant before requesting transaction commit.

## Build and validation

The CPU daemon keeps its existing build and runtime dependencies:

```sh
make -C agent/pagebroker daemon test
```

Build the optional worker with public CUDA headers and driver-link stubs:

```sh
make -C agent/pagebroker allocation-worker \
  CUDA_ROOT=/usr/local/cuda CUDA_LIB_DIR=/usr/local/cuda/lib64/stubs
```

It also needs protobuf and OpenSSL development libraries. At runtime the
worker needs the NVIDIA driver library and access to the selected GPUs.
It does not call or link native checkpoint/CustomStorage operations. The
default PageBroker image does not yet package or enable this worker.

`make -C agent/pagebroker test-allocations` requires Python protobuf bindings
and exercises actual broker connections, spawned fake workers, FD transport,
save/load manifests, and transaction cleanup. Real CUDA worker compilation
and physical GPU qualification are separate checks.
