<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0 -->

# GPU extent integrity

Extent digest matching uses separate digest metadata and shared content hashing
in `../../integrity/`. It is independent of the core version-4 GPU manifest.
A subsequent transfer implementation supplies persistence and runtime policy.
