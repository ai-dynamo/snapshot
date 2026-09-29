<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# PageBroker modules

Existing daemon, filesystem, protocol, and transport paths stay in place.
GPU storage code moves from the helper into `gpu_engine/`:

- `storage_manifest.*`: version-4 GPU extent format, sizes, and GPU mapping.
- `checksum/`: SHA-256 and extent digest matching, separate from the manifest.

The unused generic transfer interface, planner, per-operation limits, and
unavailable adapter are removed. Persistent transfers and worker integration
follow in later PRs. The existing helper CLI keeps calling CUDA directly until
the agent is migrated to the GPU engine.

Helm declares capture storage mode and `enableChecksumDigest: false`; later PRs
wire these into execution. No CRD or checkpoint-manifest policy field is added.

`make test-storage` checks manifests and checksums without CUDA.
