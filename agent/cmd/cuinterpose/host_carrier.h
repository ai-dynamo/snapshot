/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_HOST_CARRIER_H
#define CUINTERPOSE_HOST_CARRIER_H

#include <cuda.h>
#include <stddef.h>
#include <stdint.h>

/*
 * The host-carrier module owns the temporary storage and CUDA copy mechanics
 * used to preserve exportable allocation contents. The caller owns allocation
 * eligibility and lifecycle; pointers let the module publish host addresses
 * and fresh device handles only after a complete batch succeeds.
 */
struct cuinterpose_host_carrier_allocation {
  CUcontext context;
  CUmemAllocationProp properties;
  size_t size;
  CUmemGenericAllocationHandle* device_handle;
  void** host_address;
};

int cuinterpose_host_carrier_save(
    struct cuinterpose_host_carrier_allocation* allocations, size_t count, uint64_t* bytes,
    uint32_t* copy_us, const char** error);
int cuinterpose_host_carrier_load(
    struct cuinterpose_host_carrier_allocation* allocations, size_t count, uint64_t* bytes,
    uint32_t* copy_us, const char** error);

/* Release the restored arena after LOAD_ALLOCATIONS has replied. */
void cuinterpose_host_carrier_release(void);
/* A fork child cannot use inherited CUDA registration; discard CPU state only. */
void cuinterpose_host_carrier_fork_child(void);

#endif
