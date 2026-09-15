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

/*
 * Tracked mode: a small but honest model of the driver's allocation objects,
 * enough for the shim's bookkeeping to be tested. Handles are reference counts
 * on allocations; export produces a memfd naming the allocation; import of such
 * a memfd adds a reference; import of any other descriptor creates a fresh
 * "foreign" allocation; retain-by-address looks the mapping up. Calls succeed
 * with CUDA_SUCCESS instead of returning the forwarding codes above.
 */
void fakeEnableTrackedBehavior(void);
/* Forget every modeled allocation and mapping (tracked mode stays on). */
void fakeResetModel(void);
/* Allocations whose reference count is above zero. */
int fakeLiveAllocations(void);
/* Reference count of the allocation behind handle, or -1 when unknown/freed. */
int fakeAllocationRefs(CUmemGenericAllocationHandle handle);
/* True when two handles refer to the same modeled allocation. */
int fakeSameAllocation(CUmemGenericAllocationHandle a, CUmemGenericAllocationHandle b);
int fakeExportCalls(void);
int fakeMappedCount(void);
/* Bytes moved by cuMemcpyDtoHAsync / cuMemcpyHtoDAsync since the last model reset. */
uint64_t fakeCopiedToHost(void);
uint64_t fakeCopiedToDevice(void);
/* Host ranges currently registered with cuMemHostRegister. */
int fakeRegisteredHostRanges(void);
/* Make cuMemHostGetFlags report every range as unregistered (simulates a
 * registration that did not survive restore). */
void fakeForgetHostRegistrations(void);
/* Fail the next call to the named entry point once, with CUDA_ERROR_UNKNOWN. */
void fakeFailNext(const char* function);
/* The fake's notion of the current context, changed by cuCtxSetCurrent. */
CUcontext fakeCurrentContext(void);
/* cuDevicePrimaryCtxRetain calls since the last model reset, and retains not yet released. */
int fakePrimaryContextRetainCalls(void);
int fakePrimaryContextsHeld(void);
/* Number of cuMemSetAccess calls since the last reset. */
int fakeAccessCalls(void);
/*
 * Multicast objects in tracked mode: cuMulticastCreate makes an allocation
 * flagged as multicast whose capacity is rounded up to
 * FAKE_MULTICAST_GRANULARITY (as r615 does), devices must be added before a
 * bind names them, and bindings are checked against the capacity and the
 * member's size. Handles, mappings, export and import work as for any
 * allocation.
 */
#define FAKE_MULTICAST_GRANULARITY (4u << 20)
int fakeMulticastObjects(void);
/* Devices attached across all live multicast objects. */
int fakeMulticastDevices(void);
/* Bindings across all live objects; kind 0 = any, 1 = BindMem, 2 = BindAddr. */
int fakeMulticastBindings(int kind);
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
