// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "cuda.h"

// A non-CUDA plugin is entitled to export this name with its own behavior.
int cuMemCreate(uint64_t *out, size_t size, const void *prop, uint64_t flags) {
    (void)size; (void)prop; (void)flags;
    *out = 88;
    return 88;
}
