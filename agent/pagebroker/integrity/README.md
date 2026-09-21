<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# Optional extent integrity

`ContentDigest` incrementally hashes caller-supplied bytes with SHA-256.
`ApplyOrVerifyExtentDigests` maps checkpoint digests into extent order or verifies
restore digests against that order. Integrity metadata is a separate vector;
the core storage manifest does not depend on digests or OpenSSL. A caller using
integrity owns that metadata's persistence and must verify it before resuming a
target. Missing, duplicate or mismatched extent digests fail verification.

This module is available to callers but is not linked into the production GPU
worker. There is no runtime hashing toggle: enabling integrity requires an
explicit integration. The GPU path creates no digest contexts, scans no payload
for hashing, and retains no buffers for integrity work. Its version-4 manifest
continues to record UUIDs, sizes and filenames without digest requirements.

`make test-integrity` builds this module with OpenSSL and runs the content digest
and extent matching tests. `make test-storage` includes these tests. OpenSSL is
a test dependency; the production worker's link target does not include this
module or add a crypto library dependency.
