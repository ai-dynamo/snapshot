/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_SYMBOLS_H
#define CUINTERPOSE_SYMBOLS_H

#include <cuda.h>

/* Returned by a wrapper when the real CUDA entry point cannot be found. */
CUresult cuinterpose_unavailable(void);
/* The real driver (or runtime) function behind `name`, or NULL. */
void* cuinterpose_lookup_real_symbol(const char* name);

#endif
