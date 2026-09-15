// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Optional SDK-backed compile test; the normal headless suite uses an
// independently declared ABI so developers do not need a CUDA installation.
#include <cuda.h>
#undef CUDA_VERSION
#include "../../core_abi.h"
#include <type_traits>

static_assert(sizeof(AllocationProp) == sizeof(CUmemAllocationProp));
static_assert(alignof(AllocationProp) == alignof(CUmemAllocationProp));
static_assert(offsetof(AllocationProp, flags) == offsetof(CUmemAllocationProp, allocFlags));
static_assert(sizeof(Access) == sizeof(CUmemAccessDesc));
static_assert(offsetof(Access, flags) == offsetof(CUmemAccessDesc, flags));
static_assert(sizeof(MulticastProp) == sizeof(CUmulticastObjectProp));
static_assert(offsetof(MulticastProp, size) == offsetof(CUmulticastObjectProp, size));
static_assert(offsetof(MulticastProp, handle_types) == offsetof(CUmulticastObjectProp, handleTypes));
static_assert(sizeof(Location) == sizeof(CUmemLocation));
static_assert(sizeof(CUmemGenericAllocationHandle) == sizeof(uint64_t));
static_assert(sizeof(CUdeviceptr) == sizeof(uint64_t));
using BindMemV2 = CUresult (CUDAAPI *)(CUmemGenericAllocationHandle, CUdevice,
    size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
using BindAddrV2 = CUresult (CUDAAPI *)(CUmemGenericAllocationHandle, CUdevice,
    size_t, CUdeviceptr, size_t, unsigned long long);
static_assert(std::is_same_v<decltype(&cuMulticastBindMem_v2), BindMemV2>);
static_assert(std::is_same_v<decltype(&cuMulticastBindAddr_v2), BindAddrV2>);
