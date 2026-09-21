<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# CUDA checkpoint helper

This executable wraps the ordinary CUDA checkpoint actions (lock, checkpoint,
restore, unlock) and state queries. It does not own storage or transfer buffers.
PageBroker owns GPU data transfers and extent manifests.

The `custom_storage` C++ module owns the native CustomStorage driver lifecycle
inside PageBroker's persistent GPU worker. It has no storage configuration,
manifest, hashing, or transfer implementation. PageBroker fills the returned
driver regions in the same process and drains transfers before completion or
abort. The CLI remains the ordinary CUDA action interface.
