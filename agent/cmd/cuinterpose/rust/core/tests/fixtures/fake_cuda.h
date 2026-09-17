/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_FAKE_CUDA_H
#define CUINTERPOSE_FAKE_CUDA_H

/*
 * A stateful stand-in libcuda.so.1 for lifecycle tests. There is no GPU
 * behind it; frontend forwarding tests own a separate minimal provider.
 */

#include <cuda.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * A small model of the driver's allocation objects,
 * enough for the shim's bookkeeping to be tested. Handles are reference counts
 * on allocations; export produces a memfd naming the allocation; import of such
 * a memfd adds a reference; import of any other descriptor creates a fresh
 * "foreign" allocation; retain-by-address looks the mapping up. Calls succeed
 * with CUDA_SUCCESS when the modeled operation succeeds.
 */
/* Allocations whose reference count is above zero. */
int fakeLiveAllocations(void);
/* Reference count of the allocation behind handle, or -1 when unknown/freed. */
int fakeAllocationRefs(CUmemGenericAllocationHandle handle);
int fakeExportCalls(void);
int fakeMappedCount(void);
/* Bytes moved by cuMemcpyDtoHAsync / cuMemcpyHtoDAsync in this process. */
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
/* cuDevicePrimaryCtxRetain calls in this process, and retains not yet released. */
int fakePrimaryContextRetainCalls(void);
int fakePrimaryContextsHeld(void);
/*
 * Multicast objects: cuMulticastCreate makes an allocation
 * flagged as multicast whose capacity is rounded up to
 * FAKE_MULTICAST_GRANULARITY (as r615 does), devices must be added before a
 * bind names them, and bindings are checked against the capacity and the
 * member's size. Handles, mappings, export and import work as for any
 * allocation.
 */
#define FAKE_MULTICAST_GRANULARITY (4u << 20)
int fakeMulticastObjects(void);
/* Bindings across all live objects; kind 0 = any, 1 = BindMem, 2 = BindAddr. */
int fakeMulticastBindings(int kind);
/* The fake's own implementation of `symbol`, bypassing any interposer. */
void* fakeOriginal(const char* symbol);
/* Same, for a caller that asked for a specific CUDA version. */
void* fakeOriginalForVersion(const char* symbol, int version);

#ifdef __cplusplus
}
#endif

#endif
