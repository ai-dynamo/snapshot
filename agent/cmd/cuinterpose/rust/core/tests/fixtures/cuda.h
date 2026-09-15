// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

/* Independent Linux/amd64 ABI subset for the fake provider, not a CUDA SDK. */
#ifndef CUINTERPOSE_FAKE_CUDA_ABI_H
#define CUINTERPOSE_FAKE_CUDA_ABI_H
#include <stddef.h>
#include <stdint.h>
#define CUDAAPI
#define CUDA_VERSION 13010
typedef int CUresult;
typedef int CUdevice;
typedef uint64_t CUdeviceptr;
typedef uint64_t CUmemGenericAllocationHandle;
typedef uint64_t cuuint64_t;
typedef void *CUcontext;
typedef void *CUstream;
typedef unsigned CUmemAllocationHandleType;
typedef unsigned CUmulticastGranularity_flags;
typedef unsigned CUdriverProcAddressQueryResult;
typedef struct { int type; int id; } CUmemLocation;
typedef struct {
  int type;
  unsigned requestedHandleTypes;
  CUmemLocation location;
  void *win32HandleMetaData;
  struct { uint8_t compressionType, gpuDirectRDMACapable; uint16_t usage; uint8_t reserved[4]; } allocFlags;
} CUmemAllocationProp;
typedef struct { CUmemLocation location; uint64_t flags; } CUmemAccessDesc;
typedef struct { unsigned numDevices; size_t size; uint64_t handleTypes, flags; } CUmulticastObjectProp;
enum {
  CUDA_SUCCESS = 0, CUDA_ERROR_INVALID_VALUE = 1, CUDA_ERROR_OUT_OF_MEMORY = 2,
  CUDA_ERROR_INVALID_HANDLE = 400, CUDA_ERROR_NOT_FOUND = 500,
  CUDA_ERROR_HOST_MEMORY_NOT_REGISTERED = 713, CUDA_ERROR_UNKNOWN = 999,
  CU_MEM_LOCATION_TYPE_DEVICE = 1, CU_GET_PROC_ADDRESS_SUCCESS = 0,
  CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND = 1, CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT = 2
};
#endif
