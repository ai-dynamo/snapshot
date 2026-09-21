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
