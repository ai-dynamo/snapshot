<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# Transfer contracts and layout utilities

This module provides the transfer options, file/range layouts, chunk planning
and cancellation contracts. Its standalone tests exercise option bounds,
layout validation and cancellation. The unavailable adapter implements the
contract for builds without a transfer backend and reports that explicitly.

These contracts are available independently of the production GPU ring. The
persistent ring uses its own contiguous-file interface and node-lifetime
buffers; it does not construct a chunk-plan vector, use these per-operation
limits, or poll this cancellation token. Its cancellation and draining remain
owned by the native session and GPU worker.

`make test-transfer-contracts` runs the layout and cancellation tests without
CUDA. `make check-transfer-contracts-cuda CUDA_ROOT=...` also compiles the
unavailable adapter against CUDA headers. Neither target links these contracts
into the production GPU worker. `make test-storage` includes the CPU tests.
