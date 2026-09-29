<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# PageBroker modules

The existing daemon, filesystem storage, protocol, and transport paths stay in
place. GPU-specific code lives under `gpu_engine/`:

- `cuda_checkpoint/driver_ops.c`: ordinary driver calls shared with the helper CLI.
- `storage_manifest.*`: version-4 GPU extent format, size checks, and GPU mapping.
- `integrity/`: GPU extent digest matching, separate from the core manifest.

Top-level `integrity/` contains reusable SHA-256 hashing. The unused generic
transfer interface, planner, per-operation limits, and unavailable adapter have
been removed. Persistent transfers and worker integration follow separately.

Helm declares engine storage mode and integrity policy; subsequent integration
activates those options. Integrity defaults to false. No CRD fields are added.

`make test-storage` validates storage and hashing without CUDA.
