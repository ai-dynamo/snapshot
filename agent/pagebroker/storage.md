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
