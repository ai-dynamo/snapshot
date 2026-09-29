<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# Shared content hashing

`ContentDigest` incrementally hashes caller-supplied bytes with SHA-256. It has
no dependency on CUDA, storage manifests, PageBroker protocols, or backends.
GPU extent matching lives in `gpu_engine/integrity/`.
