/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_FAKE_CUDA_H
#define CUINTERPOSE_FAKE_CUDA_H

/*
 * A stand-in libcuda.so.1 for unit tests. Every entry point records the
 * arguments it was called with and returns a distinct, recognizable error code,
 * so a test can prove that the shim forwarded the call and its arguments
 * unchanged. There is no GPU behind it.
 */

#include <cuda.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Return codes, one per entry point, chosen far away from real CUresults. */
enum fake_result {
  FAKE_RESULT_CREATE = 101,
  FAKE_RESULT_RELEASE,
  FAKE_RESULT_RETAIN,
  FAKE_RESULT_MAP,
  FAKE_RESULT_UNMAP,
  FAKE_RESULT_ACCESS,
  FAKE_RESULT_EXPORT,
  FAKE_RESULT_IMPORT,
  FAKE_RESULT_PROPERTIES,
  FAKE_RESULT_MULTICAST_CREATE,
  FAKE_RESULT_MULTICAST_ADD_DEVICE,
  FAKE_RESULT_MULTICAST_BIND_MEM,
  FAKE_RESULT_MULTICAST_BIND_ADDRESS,
  FAKE_RESULT_MULTICAST_BIND_MEM_V2,
  FAKE_RESULT_MULTICAST_BIND_ADDRESS_V2,
  FAKE_RESULT_MULTICAST_GRANULARITY,
  FAKE_RESULT_MULTICAST_UNBIND,
};

/* The most recent call's arguments, flattened. */
struct fake_last_call {
  const char* function; /* entry point name, or NULL after fakeReset */
  CUmemGenericAllocationHandle handle;
  CUmemGenericAllocationHandle memory;
  CUdeviceptr address;
  size_t size;
  size_t offset;
  size_t memory_offset;
  unsigned long long flags;
  CUdevice device;
  int has_device; /* set by the entry points that take a device */
  int handle_type;
  size_t count;
  void* pointer;
};

void fakeReset(void);
const struct fake_last_call* fakeLastCall(void);
/* The fake's own implementation of `symbol`, bypassing any interposer. */
void* fakeOriginal(const char* symbol);
/* Same, for a caller that asked for a specific CUDA version. */
void* fakeOriginalForVersion(const char* symbol, int version);
/* CUDA version the fake pretends to be; cuGetProcAddress refuses newer requests. */
int fakeDriverVersion(void);

#ifdef __cplusplus
}
#endif

#endif
