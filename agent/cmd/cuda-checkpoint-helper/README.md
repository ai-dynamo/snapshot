<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# CUDA checkpoint helper

The CLI retains its arguments, output, state queries, and legacy job-file
handling. Ordinary CUDA calls are shared through
`../../pagebroker/gpu_engine/cuda_checkpoint/driver_ops.c`.

GPU storage manifests and hashing now live under PageBroker. The standalone
CLI does not own storage or transfer buffers.
