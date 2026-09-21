<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# Direct restore transactions

The GPU path uses `DirectRestoreRequest`. It validates a published filesystem artifact and retains a read-only directory descriptor without staging its contents. `DirectRestoreReady` means the source is ready for broker-owned consumers; it does not mean application or GPU state has been restored. Consumers use the retained descriptor to open source files relative to the artifact.

The source must remain immutable and available for the transaction's lifetime. A directory descriptor preserves directory identity across a rename, but does not prevent another process from deleting its children. Snapshot's artifact lifetime management remains responsible for preventing concurrent deletion. Consumers must report missing or invalid content rather than substitute another source.

Commit, abort, expiry, and broker shutdown release the descriptor without modifying or deleting the published artifact. Transaction identity, conflict behavior, and terminal-request idempotency are the same as staged restore. No filesystem staging capacity is reserved; bounded GPU host buffers belong to the persistent GPU worker. The internal `STAGED` state denotes readiness for consumers, not the presence of copied files.

The CRIU path uses the separate `StagedRestoreRequest` contract: it returns a transaction-owned, independently writable directory. Callers that need such a working copy must not treat a direct source as writable staging.

This implementation uses the existing protobuf operations. It provides the source lifetime needed by direct transfer consumers; it does not introduce a GPU transfer engine, native CUDA CustomStorage, or a caller-facing descriptor transport. GPU transfers can still use bounded host buffers: “direct” avoids a filesystem staging copy, and does not imply GPUDirect Storage.

The request’s existing `io_engine` selects the source-directory adapter (`posix_copy` for filesystem artifacts). Opening that source does not copy its contents or choose the GPU data mover. The GPU worker owns its transfer backend; wiring NIXL POSIX into GPU transfers does not change CRIU directory staging.
