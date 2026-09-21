<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# CUDA checkpoint helper

This executable wraps the ordinary CUDA checkpoint actions (lock, checkpoint,
restore, unlock) and state queries. It does not own storage or transfer buffers.
PageBroker owns native CustomStorage and its GPU extent manifests.
