<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Explicit allocation transfer sessions

PageBroker can save and load externally supplied CUDA VMM backing without
enabling native CUDA CustomStorage. This broker-side building block has no
dependency on an interposer implementation. A caller supplies the backing
and owns CUDA topology reconstruction; PageBroker owns content transfer.
Ordinary PageBroker filesystem transactions do not require CUDA.

This implementation consolidates the allocation-transfer use case of the
experimental `hannahz/pagebroker-nixl-prototype` branch with the bounded
asynchronous NIXL transfer work in commits `a79ec05` and `e1a06b2`. It does
not import the native CustomStorage orchestration or C interposer from
`hannahz/pagebroker-cuda-foundation-integration`. The NIXL adapter is currently
filesystem-specific, not a generic FD-only contract for future storage backends.
It does not depend on the CUDA helper command codec proposed in PR #312.

## Session boundary

The trusted agent creates a staged checkpoint transaction for SAVE or a
`DirectRestore` transaction for LOAD through the existing broker protocol.
Direct restore retains a read-only published source without staging its bytes.
On a new connection the agent sends
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
and transfers through a reusable ring. It reuses the
bounded POSIX transfer-ring design from the PageBroker CUDA foundation.
The backend pipelines D2H/H2D operations and storage I/O through pinned host
buffers and computes SHA-256 during the sole content pass. It is not a
GPU-direct storage backend.

Allocations within one batch are processed serially; participant sessions run
concurrently. Each worker retains a context, stream, and four 64-MiB transfer
slots per used GPU across batches. The GPU image uses NIXL POSIX asynchronous
I/O to overlap storage requests with DMA. The CPU-test implementation uses
POSIX reads and writes; it is not a runtime fallback if NIXL fails.

A batch succeeds only after transfers and CUDA cleanup complete and the
worker and broker drop their temporary export FDs. Failed operations terminate
and reap the disposable worker before admission can be released. No failed
load is retried on partially populated backing. A five-minute socket timeout
bounds stalled exchanges; the transfer loop also checks a four-minute
cooperative deadline. Reaping a killed worker still waits for OS process exit:
transaction cleanup must not race a process retaining CUDA references.

## Artifact and transaction lifetime

During capture, content lives at:

```text
<transaction staging>/allocations/<participant-id>/<allocation-id>
<transaction staging>/allocations/<participant-id>/manifest.pb
```

`AllocationManifest` version 1 records the participant, allocation IDs,
sizes, source device UUIDs, and digests. Save batches fsync content; `finish`
atomically publishes and fsyncs the complete participant manifest. Load binding
validates the manifest and regular-file sizes before accepting allocation FDs.
LOAD reads the published `allocations/` files relative to the direct
transaction's retained source descriptor. Commit, abort, and expiry release
that descriptor without deleting published content. The caller must keep
the artifact and its children available; an open directory FD alone does not
prevent another process from deleting its files.
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
default CPU-only PageBroker image does not enable this worker. The optional
`Dockerfile.gpu` image packages the worker and pinned NIXL POSIX backend:

```sh
make -C agent/pagebroker image-gpu GPU_IMAGE=<registry>/<image>:<tag>
```

Set the chart's `pageBroker.allocationWorker` to
`/usr/local/bin/pagebroker-allocation-worker` and select that GPU image to
enable it. The worker's environment variable
`PAGEBROKER_ALLOCATION_DIRECT_IO=1` opts into aligned direct file I/O;
unsupported direct I/O fails rather than silently reverting to buffered I/O.

`make -C agent/pagebroker test-allocations` requires Python protobuf bindings
and exercises actual broker connections, spawned fake workers, FD transport,
save/load manifests, and transaction cleanup. Real CUDA worker compilation
and physical GPU qualification are separate checks.
