/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_INTERPOSE_H
#define CUINTERPOSE_INTERPOSE_H

#include <cuda.h>
#include <stdbool.h>

/* Internal entry points shared between the shim's translation units. */

/*
 * Make sure this process has its participant identity and control socket. A
 * child created by fork() inherits neither, so every wrapper and every
 * resolver calls this first; the first CUDA activity in the child creates them.
 */
CUresult cuinterpose_ensure_process_endpoint(void);

/*
 * If `handle` is one of the shim's logical handles for a tracked allocation,
 * store the driver handle behind it in *driver and return true. Used by the
 * multicast wrappers, which must hand the driver a handle it recognizes.
 */
bool cuinterpose_translate_handle(CUmemGenericAllocationHandle handle, CUmemGenericAllocationHandle* driver);

/* True for handles minted by the shim (unicast or multicast), by their tag. */
bool cuinterpose_is_logical_handle(CUmemGenericAllocationHandle handle);

#endif
