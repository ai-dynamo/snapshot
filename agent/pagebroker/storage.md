<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# Native GPU extent storage

PageBroker owns one private directory per CUDA participant. Its version-4
manifest records each extent's source GPU UUID, byte length, and deterministic
filename. Restore matches extents to destination GPU UUIDs and verifies the
file lengths before transferring data. Earlier manifest versions are rejected.

Checkpoint storage is trusted and immutable during restore. Production payloads have no
content hashes: layout and size validation do not detect same-size corruption.
The manifest is written once in a fresh private participant directory, after
the extent files are durable. PageBroker publishes the enclosing transaction
only after every participant completes.
The transaction owner exclusively controls the participant directory throughout
capture, restore, and cleanup.

`make -C agent/pagebroker test-storage` checks GPU remapping, manifest parsing,
file sizes, publication, and cleanup without CUDA or a GPU.

## Optional modules

The SHA-256 implementation and extent digest matching live in
[integrity/](integrity/README.md). Digest metadata is separate from the core
manifest; the production GPU worker does not link or call this module. Its
transfer path performs no hashing and requires no digest fields.

[transfer_contracts/](transfer_contracts/README.md) provides layout planning,
option validation, cancellation and the unavailable-backend adapter. These
utilities remain independently tested and available without coupling their
per-operation policies to the persistent GPU ring.

## CUDA transfer ring

PageBroker transfers a single file to or from one contiguous CUDA extent.
The caller supplies an open, correctly sized file and serializes access to the
ring. It owns the CUDA context and stream for the duration of the transfer.
There is no allocation manifest, chunk-plan vector, hash pass, or per-operation
buffer allocation in this interface.

Each device ring initializes once, before accepting work. CUDA host-NUMA VMM
allocations place staging memory near that GPU; a non-NUMA host uses node zero.
The production ring has 32 slots of 128 MiB (4 GiB per GPU). The storage-only `NixlTransfer` adapter accepts ordinary host buffers and file
ranges; it contains no CUDA or CRIU types. NIXL registers these buffers once, uses its POSIX asynchronous backend, and registers only the current
file per transfer. Direct I/O is an engine policy, not negotiated per request.

LOAD primes all reads, then overlaps subsequent reads with H2D copies. SAVE
primes D2H copies and overlaps writes with the next copies. Both storage and CUDA
must drain before a slot can be reused. An uncertain drain terminates the GPU
engine; it cannot safely recycle the ring. SAVE fsyncs the file before returning.
Storage service time sums overlapping requests and is not wall-clock time.

`make test-cuda-transfer CUDA_ROOT=... NIXL_ROOT=...` runs the real NIXL POSIX
backend with mocked CUDA memory operations. Omitting NIXL_ROOT selects a
synchronous POSIX implementation for CPU tests, not a runtime fallback. Tests
cover multi-ring round trips, file changes, partial final chunks, failed-copy
and failed-read reuse, and cleanup after each host-NUMA allocation failure.
Real GPU/driver performance requires cluster qualification.

## Persistent GPU worker and CUDA helper

The CPU broker owns one `pagebroker-gpu-engine` process. Its CUDA helper module
owns the CustomStorage driver lifecycle and target cleanup; PageBroker owns
storage, transfer scheduling, pinned buffers, copies and events. Both modules
run in this process because driver regions and streams are process-local. The engine retains all
visible GPU primary contexts, initializes the host-NUMA rings and NIXL agents,
and only then reports readiness. A session receives a socket and an open artifact
directory. Session commands use protobuf frames with SCM_RIGHTS; no CUDA exports
or workload-facing allocation protocol is involved.

SAVE follows LOCK → PREPARE → TRANSFER → COMPLETE. LOAD follows PREPARE → TRANSFER
→ COMPLETE, then unlocks the target. PREPARE obtains the driver's CustomStorage
view. TRANSFER uses its device pointer and stream directly, with no extra device
allocation. Per-device mutexes serialize ring use; different GPUs can transfer
concurrently. The engine does not release its primary contexts between sessions.

An incomplete session drains after the synchronous command finishes. If native
preparation began, the engine kills the target through its pinned pidfd and calls
COMPLETE on the abandoned operation. This requires the qualified CustomStorage
driver's cleanup semantics. A drain transport failure terminates and reaps the
engine before the CPU broker releases admission. No public CUDA abort API is
assumed. Successful sessions retain the engine for subsequent restores.

## Transaction admission

`--gpu-engine /absolute/path` enables native sessions explicitly. The CPU broker
waits for that executable's ready reply before serving requests. The executable
path is not inferred from a second worker's path.

BindNative pins the transaction storage and target PID namespace before CRIU.
The first operation identifies the live target in that namespace or its CRIU
child namespace. Each captured PID can be admitted once. Commit requires every
session to complete and drain; Abort waits for disconnected sessions to drain
before deleting staging. Live sessions also prevent transaction expiry from
removing their backing files. Failed sessions permit Abort but prevent Commit.

The Go client returns only the bound session socket for nsrestore to inherit.
The workload has no general broker connection. The session direction is native
SAVE or LOAD, independent of cuinterpose's host-carrier protocol.
