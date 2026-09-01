/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSER_SYMBOLS_H
#define CUINTERPOSER_SYMBOLS_H

#include <cuda.h>

CUresult cuinterposer_unavailable(void);
void* cuinterposer_lookup_real_symbol(const char* name);

#endif
