/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "fake_cuda.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>
#include <string.h>

#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

CUresult CUDAAPI fakeCuGetProcAddress(const char*, void**, int, cuuint64_t);

static struct fake_last_call last;

#define FAKE_MAX_HOST 256

struct fake_host_range {
  int used;
  void* address;
  size_t size;
};

static struct fake_host_range host_ranges[FAKE_MAX_HOST];
static uint64_t copied_to_host;
static uint64_t copied_to_device;
static CUcontext current_context = (CUcontext)(uintptr_t)1;
/* Primary contexts: one per device, handed out by cuDevicePrimaryCtxRetain. */
static int primary_retain_calls;
static int primary_contexts_held;
static char fail_next[64];
static CUdeviceptr next_reservation = 0x7f0000000000ULL;

static int should_fail(const char* function);

void
fakeReset(void)
{
  memset(&last, 0, sizeof(last));
}

const struct fake_last_call*
fakeLastCall(void)
{
  return &last;
}

int
fakeDriverVersion(void)
{
  return CUDA_VERSION;
}

static void
record(const char* function)
{
  memset(&last, 0, sizeof(last));
  last.function = function;
}

/* ---------------------------------------------------------------------- */
/* Tracked mode model.                                                      */
/* ---------------------------------------------------------------------- */

#define FAKE_MAX_ALLOCATIONS 512
#define FAKE_MAX_HANDLES 4096
#define FAKE_MAX_MAPPINGS 4096
#define FAKE_HANDLE_BASE 0x1000
#define FAKE_MAX_MULTICAST_DEVICES 8
#define FAKE_MAX_MULTICAST_BINDINGS 16
#define FAKE_EXPORT_MAGIC "fake-cuda-export:"

/* A multicast object's binding: which device bound which member slice where. */
struct fake_binding {
  int used;
  CUdevice device;
  size_t offset;
  size_t size;
  int member;
  size_t member_offset;
  CUdeviceptr member_address;
  int kind; /* 1: BindMem, 2: BindAddr */
};

struct fake_allocation {
  int used;
  int refs;
  size_t size;
  CUmemAllocationProp properties;
  /* Multicast objects share the handle/mapping/export machinery. */
  int multicast;
  size_t capacity; /* like the real driver, rounded up above the requested size */
  CUmulticastObjectProp multicast_properties;
  CUdevice devices[FAKE_MAX_MULTICAST_DEVICES];
  int device_count;
  struct fake_binding bindings[FAKE_MAX_MULTICAST_BINDINGS];
};

struct fake_handle {
  int used;
  int allocation;
};

struct fake_mapping {
  int used;
  CUdeviceptr address;
  size_t size;
  int allocation;
};

static int tracked;
static struct fake_allocation allocations[FAKE_MAX_ALLOCATIONS];
static struct fake_handle handles[FAKE_MAX_HANDLES];
static struct fake_mapping mappings[FAKE_MAX_MAPPINGS];
static int export_calls;
static int access_calls;
static pthread_mutex_t model_lock = PTHREAD_MUTEX_INITIALIZER;

void
fakeEnableTrackedBehavior(void)
{
  tracked = 1;
}

void
fakeResetModel(void)
{
  current_context = (CUcontext)(uintptr_t)1;
  primary_retain_calls = 0;
  primary_contexts_held = 0;
  pthread_mutex_lock(&model_lock);
  memset(allocations, 0, sizeof(allocations));
  memset(handles, 0, sizeof(handles));
  memset(mappings, 0, sizeof(mappings));
  memset(host_ranges, 0, sizeof(host_ranges));
  export_calls = 0;
  access_calls = 0;
  copied_to_host = 0;
  copied_to_device = 0;
  fail_next[0] = '\0';
  pthread_mutex_unlock(&model_lock);
}

static int
handle_index(CUmemGenericAllocationHandle handle)
{
  if (handle < FAKE_HANDLE_BASE || handle >= FAKE_HANDLE_BASE + FAKE_MAX_HANDLES)
    return -1;
  return (int)(handle - FAKE_HANDLE_BASE);
}

/* Caller holds model_lock. */
static CUmemGenericAllocationHandle
new_handle(int allocation)
{
  int index;

  for (index = 0; index < FAKE_MAX_HANDLES; index++) {
    if (!handles[index].used) {
      handles[index].used = 1;
      handles[index].allocation = allocation;
      allocations[allocation].refs++;
      return (CUmemGenericAllocationHandle)(FAKE_HANDLE_BASE + index);
    }
  }
  return 0;
}

/* Caller holds model_lock. Returns the allocation index or -1. */
static int
new_allocation(size_t size, const CUmemAllocationProp* properties)
{
  int index;

  for (index = 0; index < FAKE_MAX_ALLOCATIONS; index++) {
    if (!allocations[index].used) {
      memset(&allocations[index], 0, sizeof(allocations[index]));
      allocations[index].used = 1;
      allocations[index].refs = 0;
      allocations[index].size = size;
      if (properties != NULL)
        allocations[index].properties = *properties;
      else
        memset(&allocations[index].properties, 0, sizeof(allocations[index].properties));
      return index;
    }
  }
  return -1;
}

int
fakeLiveAllocations(void)
{
  int index;
  int live = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_ALLOCATIONS; index++)
    live += allocations[index].used && allocations[index].refs > 0;
  pthread_mutex_unlock(&model_lock);
  return live;
}

int
fakeAllocationRefs(CUmemGenericAllocationHandle handle)
{
  int index = handle_index(handle);
  int refs = -1;

  pthread_mutex_lock(&model_lock);
  if (index >= 0 && handles[index].used)
    refs = allocations[handles[index].allocation].refs;
  pthread_mutex_unlock(&model_lock);
  return refs;
}

int
fakeSameAllocation(CUmemGenericAllocationHandle a, CUmemGenericAllocationHandle b)
{
  int ia = handle_index(a);
  int ib = handle_index(b);
  int same = 0;

  pthread_mutex_lock(&model_lock);
  same = ia >= 0 && ib >= 0 && handles[ia].used && handles[ib].used && handles[ia].allocation == handles[ib].allocation;
  pthread_mutex_unlock(&model_lock);
  return same;
}

int
fakeExportCalls(void)
{
  return export_calls;
}

int
fakeMappedCount(void)
{
  int index;
  int count = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_MAPPINGS; index++)
    count += mappings[index].used;
  pthread_mutex_unlock(&model_lock);
  return count;
}

int
fakeAccessCalls(void)
{
  return access_calls;
}

/*
 * Each entry point exists twice: the exported CUDA name, and a fakeXxx alias
 * that cuGetProcAddress hands out. The alias lets a test tell "the shim's
 * wrapper" apart from "the driver's own function".
 */

CUresult CUDAAPI
fakeCuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  if (tracked) {
    int allocation;
    CUmemGenericAllocationHandle handle = 0;
    record("cuMemCreate");
    last.size = size;
    last.flags = flags;
    last.handle_type = properties != NULL ? (int)properties->requestedHandleTypes : -1;
    if (output == NULL)
      return CUDA_ERROR_INVALID_VALUE;
    if (should_fail("cuMemCreate"))
      return CUDA_ERROR_UNKNOWN;
    pthread_mutex_lock(&model_lock);
    allocation = new_allocation(size, properties);
    if (allocation >= 0)
      handle = new_handle(allocation);
    pthread_mutex_unlock(&model_lock);
    if (handle == 0)
      return CUDA_ERROR_OUT_OF_MEMORY;
    *output = handle;
    return CUDA_SUCCESS;
  }
  record("cuMemCreate");
  last.size = size;
  last.flags = flags;
  last.handle_type = properties != NULL ? (int)properties->requestedHandleTypes : -1;
  if (output != NULL)
    *output = 0xabc;
  return (CUresult)FAKE_RESULT_CREATE;
}
CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  return fakeCuMemCreate(output, size, properties, flags);
}

CUresult CUDAAPI
fakeCuMemRelease(CUmemGenericAllocationHandle handle)
{
  if (tracked) {
    int index = handle_index(handle);
    record("cuMemRelease");
    last.handle = handle;
    pthread_mutex_lock(&model_lock);
    if (index < 0 || !handles[index].used) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    handles[index].used = 0;
    if (--allocations[handles[index].allocation].refs == 0)
      allocations[handles[index].allocation].used = 0;
    pthread_mutex_unlock(&model_lock);
    return CUDA_SUCCESS;
  }
  record("cuMemRelease");
  last.handle = handle;
  return (CUresult)FAKE_RESULT_RELEASE;
}
CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle handle)
{
  return fakeCuMemRelease(handle);
}

CUresult CUDAAPI
fakeCuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  if (tracked) {
    int index;
    CUmemGenericAllocationHandle handle = 0;
    record("cuMemRetainAllocationHandle");
    last.pointer = address;
    if (output == NULL)
      return CUDA_ERROR_INVALID_VALUE;
    pthread_mutex_lock(&model_lock);
    for (index = 0; index < FAKE_MAX_MAPPINGS; index++) {
      if (mappings[index].used && (CUdeviceptr)(uintptr_t)address >= mappings[index].address &&
          (CUdeviceptr)(uintptr_t)address < mappings[index].address + mappings[index].size) {
        handle = new_handle(mappings[index].allocation);
        break;
      }
    }
    pthread_mutex_unlock(&model_lock);
    if (handle == 0)
      return CUDA_ERROR_INVALID_VALUE;
    *output = handle;
    return CUDA_SUCCESS;
  }
  record("cuMemRetainAllocationHandle");
  last.pointer = address;
  if (output != NULL)
    *output = 0xdef;
  return (CUresult)FAKE_RESULT_RETAIN;
}
CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  return fakeCuMemRetainAllocationHandle(output, address);
}

CUresult CUDAAPI
fakeCuMemMap(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  if (tracked) {
    int hindex = handle_index(handle);
    int index;
    record("cuMemMap");
    last.address = address;
    last.size = size;
    last.offset = offset;
    last.handle = handle;
    last.flags = flags;
    pthread_mutex_lock(&model_lock);
    if (hindex < 0 || !handles[hindex].used) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    for (index = 0; index < FAKE_MAX_MAPPINGS; index++) {
      if (!mappings[index].used) {
        mappings[index].used = 1;
        mappings[index].address = address;
        mappings[index].size = size;
        mappings[index].allocation = handles[hindex].allocation;
        /* The mapping keeps the memory alive, like the driver does. */
        allocations[mappings[index].allocation].refs++;
        pthread_mutex_unlock(&model_lock);
        return CUDA_SUCCESS;
      }
    }
    pthread_mutex_unlock(&model_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  record("cuMemMap");
  last.address = address;
  last.size = size;
  last.offset = offset;
  last.handle = handle;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MAP;
}
CUresult CUDAAPI
cuMemMap(CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  return fakeCuMemMap(address, size, offset, handle, flags);
}

CUresult CUDAAPI
fakeCuMemUnmap(CUdeviceptr address, size_t size)
{
  if (tracked) {
    int index;
    int found = 0;
    record("cuMemUnmap");
    last.address = address;
    last.size = size;
    pthread_mutex_lock(&model_lock);
    for (index = 0; index < FAKE_MAX_MAPPINGS; index++) {
      if (mappings[index].used && mappings[index].address >= address &&
          mappings[index].address + mappings[index].size <= address + size) {
        mappings[index].used = 0;
        if (--allocations[mappings[index].allocation].refs == 0)
          allocations[mappings[index].allocation].used = 0;
        found = 1;
      }
    }
    pthread_mutex_unlock(&model_lock);
    return found ? CUDA_SUCCESS : CUDA_ERROR_INVALID_VALUE;
  }
  record("cuMemUnmap");
  last.address = address;
  last.size = size;
  return (CUresult)FAKE_RESULT_UNMAP;
}
CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  return fakeCuMemUnmap(address, size);
}

CUresult CUDAAPI
fakeCuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  if (tracked) {
    record("cuMemSetAccess");
    last.address = address;
    last.size = size;
    last.count = count;
    last.pointer = (void*)descriptors;
    access_calls++;
    return CUDA_SUCCESS;
  }
  record("cuMemSetAccess");
  last.address = address;
  last.size = size;
  last.count = count;
  last.pointer = (void*)descriptors;
  return (CUresult)FAKE_RESULT_ACCESS;
}
CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  return fakeCuMemSetAccess(address, size, descriptors, count);
}

CUresult CUDAAPI
fakeCuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  if (tracked) {
    int index = handle_index(handle);
    char text[64];
    int fd;
    record("cuMemExportToShareableHandle");
    last.pointer = output;
    last.handle = handle;
    last.handle_type = (int)type;
    last.flags = flags;
    if (output == NULL || flags != 0)
      return CUDA_ERROR_INVALID_VALUE;
    pthread_mutex_lock(&model_lock);
    if (index < 0 || !handles[index].used) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    export_calls++;
    /* Enough for a foreign process to model the same object. */
    snprintf(
        text, sizeof(text), FAKE_EXPORT_MAGIC "%d:%d:%zu:%zu:%d", handles[index].allocation,
        allocations[handles[index].allocation].multicast, allocations[handles[index].allocation].size,
        allocations[handles[index].allocation].capacity, allocations[handles[index].allocation].properties.location.id);
    /* The real driver's descriptor holds a reference until it is closed; this
     * model cannot observe close(), so the descriptor is not counted here. */
    pthread_mutex_unlock(&model_lock);
    fd = memfd_create("fake-cuda-export", MFD_CLOEXEC);
    if (fd < 0 || write(fd, text, strlen(text)) != (ssize_t)strlen(text))
      return CUDA_ERROR_UNKNOWN;
    *(int*)output = fd;
    return CUDA_SUCCESS;
  }
  record("cuMemExportToShareableHandle");
  last.pointer = output;
  last.handle = handle;
  last.handle_type = (int)type;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_EXPORT;
}
CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  return fakeCuMemExportToShareableHandle(output, handle, type, flags);
}

CUresult CUDAAPI
fakeCuMemImportFromShareableHandle(
    CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  if (tracked) {
    char text[128] = {0};
    int allocation = -1;
    int multicast = 0;
    int location = 0;
    size_t size = 0;
    size_t capacity = 0;
    CUmemGenericAllocationHandle handle = 0;
    record("cuMemImportFromShareableHandle");
    last.pointer = os_handle;
    last.handle_type = (int)type;
    if (output == NULL)
      return CUDA_ERROR_INVALID_VALUE;
    if (pread((int)(uintptr_t)os_handle, text, sizeof(text) - 1, 0) > 0 &&
        strncmp(text, FAKE_EXPORT_MAGIC, strlen(FAKE_EXPORT_MAGIC)) == 0 &&
        sscanf(text + strlen(FAKE_EXPORT_MAGIC), "%d:%d:%zu:%zu:%d", &allocation, &multicast, &size, &capacity,
               &location) != 5)
      allocation = -1;
    pthread_mutex_lock(&model_lock);
    if (allocation < 0 || allocation >= FAKE_MAX_ALLOCATIONS || !allocations[allocation].used) {
      /* Not one of ours: model the other process's object from its description. */
      allocation = new_allocation(size, NULL);
      if (allocation >= 0) {
        allocations[allocation].multicast = multicast;
        allocations[allocation].capacity = capacity;
        allocations[allocation].properties.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
        allocations[allocation].properties.location.id = location;
      }
    }
    if (allocation >= 0)
      handle = new_handle(allocation);
    pthread_mutex_unlock(&model_lock);
    if (handle == 0)
      return CUDA_ERROR_OUT_OF_MEMORY;
    *output = handle;
    return CUDA_SUCCESS;
  }
  record("cuMemImportFromShareableHandle");
  last.pointer = os_handle;
  last.handle_type = (int)type;
  if (output != NULL)
    *output = 0x123;
  return (CUresult)FAKE_RESULT_IMPORT;
}
CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  return fakeCuMemImportFromShareableHandle(output, os_handle, type);
}

CUresult CUDAAPI
fakeCuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  if (tracked) {
    int index = handle_index(handle);
    record("cuMemGetAllocationPropertiesFromHandle");
    last.pointer = properties;
    last.handle = handle;
    pthread_mutex_lock(&model_lock);
    if (index < 0 || !handles[index].used) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    if (properties != NULL)
      *properties = allocations[handles[index].allocation].properties;
    pthread_mutex_unlock(&model_lock);
    return CUDA_SUCCESS;
  }
  record("cuMemGetAllocationPropertiesFromHandle");
  last.pointer = properties;
  last.handle = handle;
  return (CUresult)FAKE_RESULT_PROPERTIES;
}
CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  return fakeCuMemGetAllocationPropertiesFromHandle(properties, handle);
}

/* Caller holds model_lock. The multicast allocation behind a handle, or -1. */
static int
multicast_index(CUmemGenericAllocationHandle handle)
{
  int index = handle_index(handle);

  if (index < 0 || !handles[index].used || !allocations[handles[index].allocation].multicast)
    return -1;
  return handles[index].allocation;
}

static int
multicast_has_device(const struct fake_allocation* object, CUdevice device)
{
  int index;

  for (index = 0; index < object->device_count; index++) {
    if (object->devices[index] == device)
      return 1;
  }
  return 0;
}

/* Caller holds model_lock. Records a binding after the same checks the driver makes. */
static CUresult
multicast_bind(
    int object_index, CUdevice device, size_t offset, int member, size_t member_offset, CUdeviceptr member_address,
    size_t size, int kind)
{
  struct fake_allocation* object = &allocations[object_index];
  int index;

  if (!multicast_has_device(object, device))
    return CUDA_ERROR_INVALID_VALUE; /* the driver: the device must be attached first */
  if (size == 0 || offset > object->capacity || size > object->capacity - offset)
    return CUDA_ERROR_INVALID_VALUE;
  if (member < 0 || !allocations[member].used || member_offset > allocations[member].size ||
      size > allocations[member].size - member_offset)
    return CUDA_ERROR_INVALID_VALUE;
  for (index = 0; index < FAKE_MAX_MULTICAST_BINDINGS; index++) {
    if (!object->bindings[index].used) {
      object->bindings[index].used = 1;
      object->bindings[index].device = device;
      object->bindings[index].offset = offset;
      object->bindings[index].size = size;
      object->bindings[index].member = member;
      object->bindings[index].member_offset = member_offset;
      object->bindings[index].member_address = member_address;
      object->bindings[index].kind = kind;
      return CUDA_SUCCESS;
    }
  }
  return CUDA_ERROR_OUT_OF_MEMORY;
}

/* Caller holds model_lock. The mapping holding [address, address + size), or -1. */
static int
mapping_containing(CUdeviceptr address, size_t size, size_t* offset_in_mapping)
{
  int index;

  for (index = 0; index < FAKE_MAX_MAPPINGS; index++) {
    if (mappings[index].used && address >= mappings[index].address && size <= mappings[index].size &&
        address - mappings[index].address <= mappings[index].size - size) {
      *offset_in_mapping = (size_t)(address - mappings[index].address);
      return index;
    }
  }
  return -1;
}

static CUresult
tracked_bind_mem(
    CUmemGenericAllocationHandle multicast, CUdevice device, int device_explicit, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size)
{
  int object;
  int member;
  CUresult result;

  pthread_mutex_lock(&model_lock);
  object = multicast_index(multicast);
  member = handle_index(memory);
  member = member >= 0 && handles[member].used ? handles[member].allocation : -1;
  if (object < 0 || member < 0) {
    pthread_mutex_unlock(&model_lock);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  if (!device_explicit)
    device = allocations[member].properties.location.id;
  result = multicast_bind(object, device, multicast_offset, member, memory_offset, 0, size, 1);
  pthread_mutex_unlock(&model_lock);
  return result;
}

static CUresult
tracked_bind_address(
    CUmemGenericAllocationHandle multicast, CUdevice device, int device_explicit, size_t multicast_offset,
    CUdeviceptr memory, size_t size)
{
  int object;
  int mapping;
  size_t offset_in_mapping = 0;
  CUresult result;

  pthread_mutex_lock(&model_lock);
  object = multicast_index(multicast);
  mapping = mapping_containing(memory, size, &offset_in_mapping);
  if (object < 0) {
    pthread_mutex_unlock(&model_lock);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  if (mapping < 0) {
    pthread_mutex_unlock(&model_lock);
    return CUDA_ERROR_INVALID_VALUE;
  }
  if (!device_explicit)
    device = allocations[mappings[mapping].allocation].properties.location.id;
  result = multicast_bind(
      object, device, multicast_offset, mappings[mapping].allocation, offset_in_mapping, memory, size, 2);
  pthread_mutex_unlock(&model_lock);
  return result;
}

int
fakeMulticastObjects(void)
{
  int index;
  int count = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_ALLOCATIONS; index++)
    count += allocations[index].used && allocations[index].multicast;
  pthread_mutex_unlock(&model_lock);
  return count;
}

int
fakeMulticastDevices(void)
{
  int index;
  int count = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_ALLOCATIONS; index++) {
    if (allocations[index].used && allocations[index].multicast)
      count += allocations[index].device_count;
  }
  pthread_mutex_unlock(&model_lock);
  return count;
}

int
fakeMulticastBindings(int kind)
{
  int index;
  int binding;
  int count = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_ALLOCATIONS; index++) {
    if (!allocations[index].used || !allocations[index].multicast)
      continue;
    for (binding = 0; binding < FAKE_MAX_MULTICAST_BINDINGS; binding++) {
      if (allocations[index].bindings[binding].used && (kind == 0 || allocations[index].bindings[binding].kind == kind))
        count++;
    }
  }
  pthread_mutex_unlock(&model_lock);
  return count;
}

CUresult CUDAAPI
fakeCuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  if (tracked) {
    int allocation;
    CUmemGenericAllocationHandle handle = 0;

    record("cuMulticastCreate");
    if (output == NULL || properties == NULL || properties->size == 0)
      return CUDA_ERROR_INVALID_VALUE;
    if (should_fail("cuMulticastCreate"))
      return CUDA_ERROR_UNKNOWN;
    pthread_mutex_lock(&model_lock);
    allocation = new_allocation(properties->size, NULL);
    if (allocation >= 0) {
      allocations[allocation].multicast = 1;
      allocations[allocation].multicast_properties = *properties;
      /* Like r615, which rounds a multicast object's capacity up. */
      allocations[allocation].capacity =
          (properties->size + FAKE_MULTICAST_GRANULARITY - 1) / FAKE_MULTICAST_GRANULARITY * FAKE_MULTICAST_GRANULARITY;
      handle = new_handle(allocation);
    }
    pthread_mutex_unlock(&model_lock);
    if (handle == 0)
      return CUDA_ERROR_OUT_OF_MEMORY;
    *output = handle;
    return CUDA_SUCCESS;
  }
  record("cuMulticastCreate");
  last.pointer = (void*)properties;
  last.size = properties != NULL ? properties->size : 0;
  if (output != NULL)
    *output = 0x456;
  return (CUresult)FAKE_RESULT_MULTICAST_CREATE;
}
CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  return fakeCuMulticastCreate(output, properties);
}

CUresult CUDAAPI
fakeCuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  if (tracked) {
    int object;
    struct fake_allocation* allocation;

    record("cuMulticastAddDevice");
    pthread_mutex_lock(&model_lock);
    object = multicast_index(multicast);
    if (object < 0) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    allocation = &allocations[object];
    if (multicast_has_device(allocation, device) || allocation->device_count == FAKE_MAX_MULTICAST_DEVICES) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_VALUE;
    }
    allocation->devices[allocation->device_count++] = device;
    pthread_mutex_unlock(&model_lock);
    return CUDA_SUCCESS;
  }
  record("cuMulticastAddDevice");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  return (CUresult)FAKE_RESULT_MULTICAST_ADD_DEVICE;
}
CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  return fakeCuMulticastAddDevice(multicast, device);
}

CUresult CUDAAPI
fakeCuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  if (tracked) {
    record("cuMulticastBindMem");
    (void)flags;
    return tracked_bind_mem(multicast, 0, 0, multicast_offset, memory, memory_offset, size);
  }
  record("cuMulticastBindMem");
  last.handle = multicast;
  last.offset = multicast_offset;
  last.memory = memory;
  last.memory_offset = memory_offset;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_MEM;
}
CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  return fakeCuMulticastBindMem(multicast, multicast_offset, memory, memory_offset, size, flags);
}

CUresult CUDAAPI
fakeCuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  if (tracked) {
    record("cuMulticastBindAddr");
    (void)flags;
    return tracked_bind_address(multicast, 0, 0, multicast_offset, memory, size);
  }
  record("cuMulticastBindAddr");
  last.handle = multicast;
  last.offset = multicast_offset;
  last.address = memory;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_ADDRESS;
}
CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return fakeCuMulticastBindAddr(multicast, multicast_offset, memory, size, flags);
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
fakeCuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  if (tracked) {
    record("cuMulticastBindMem_v2");
    (void)flags;
    return tracked_bind_mem(multicast, device, 1, multicast_offset, memory, memory_offset, size);
  }
  record("cuMulticastBindMem_v2");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = multicast_offset;
  last.memory = memory;
  last.memory_offset = memory_offset;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_MEM_V2;
}
CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  return fakeCuMulticastBindMem_v2(multicast, device, multicast_offset, memory, memory_offset, size, flags);
}

CUresult CUDAAPI
fakeCuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  if (tracked) {
    record("cuMulticastBindAddr_v2");
    (void)flags;
    return tracked_bind_address(multicast, device, 1, multicast_offset, memory, size);
  }
  record("cuMulticastBindAddr_v2");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = multicast_offset;
  last.address = memory;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_ADDRESS_V2;
}
CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return fakeCuMulticastBindAddr_v2(multicast, device, multicast_offset, memory, size, flags);
}
#endif

CUresult CUDAAPI
fakeCuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  record("cuMulticastGetGranularity");
  last.pointer = (void*)properties;
  last.flags = (unsigned long long)option;
  if (granularity != NULL)
    *granularity = 4096;
  return (CUresult)FAKE_RESULT_MULTICAST_GRANULARITY;
}
CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  return fakeCuMulticastGetGranularity(granularity, properties, option);
}

CUresult CUDAAPI
fakeCuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  if (tracked) {
    int object;
    int index;
    CUresult result = CUDA_ERROR_INVALID_VALUE;

    record("cuMulticastUnbind");
    pthread_mutex_lock(&model_lock);
    object = multicast_index(multicast);
    if (object < 0) {
      pthread_mutex_unlock(&model_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    for (index = 0; index < FAKE_MAX_MULTICAST_BINDINGS; index++) {
      struct fake_binding* binding = &allocations[object].bindings[index];
      if (binding->used && binding->device == device && binding->offset == offset && binding->size == size) {
        binding->used = 0;
        result = CUDA_SUCCESS;
        break;
      }
    }
    pthread_mutex_unlock(&model_lock);
    return result;
  }
  record("cuMulticastUnbind");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = offset;
  last.size = size;
  return (CUresult)FAKE_RESULT_MULTICAST_UNBIND;
}
CUresult CUDAAPI
cuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  return fakeCuMulticastUnbind(multicast, device, offset, size);
}


/* ---------------------------------------------------------------------- */
/* Tracked mode: staging, host carriers, streams, contexts.                 */
/* ---------------------------------------------------------------------- */


void
fakeFailNext(const char* function)
{
  snprintf(fail_next, sizeof(fail_next), "%s", function);
}

/* True (and consumed) when this call was armed to fail. */
static int
should_fail(const char* function)
{
  if (fail_next[0] != '\0' && strcmp(fail_next, function) == 0) {
    fail_next[0] = '\0';
    return 1;
  }
  return 0;
}

uint64_t
fakeCopiedToHost(void)
{
  return copied_to_host;
}

uint64_t
fakeCopiedToDevice(void)
{
  return copied_to_device;
}

int
fakeRegisteredHostRanges(void)
{
  int index;
  int count = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_HOST; index++)
    count += host_ranges[index].used;
  pthread_mutex_unlock(&model_lock);
  return count;
}

void
fakeForgetHostRegistrations(void)
{
  pthread_mutex_lock(&model_lock);
  memset(host_ranges, 0, sizeof(host_ranges));
  pthread_mutex_unlock(&model_lock);
}

CUcontext
fakeCurrentContext(void)
{
  return current_context;
}

CUresult CUDAAPI
cuCtxSetCurrent(CUcontext context)
{
  current_context = context;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemAddressReserve(CUdeviceptr* address, size_t size, size_t alignment, CUdeviceptr fixed, unsigned long long flags)
{
  (void)alignment;
  (void)fixed;
  (void)flags;
  if (should_fail("cuMemAddressReserve"))
    return CUDA_ERROR_UNKNOWN;
  pthread_mutex_lock(&model_lock);
  *address = next_reservation;
  next_reservation += (size + 0xfffff) & ~(CUdeviceptr)0xfffff;
  pthread_mutex_unlock(&model_lock);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemAddressFree(CUdeviceptr address, size_t size)
{
  (void)address;
  (void)size;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemHostRegister_v2(void* address, size_t size, unsigned int flags)
{
  int index;

  (void)flags;
  if (should_fail("cuMemHostRegister_v2"))
    return CUDA_ERROR_UNKNOWN;
  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_HOST; index++) {
    if (!host_ranges[index].used) {
      host_ranges[index].used = 1;
      host_ranges[index].address = address;
      host_ranges[index].size = size;
      pthread_mutex_unlock(&model_lock);
      return CUDA_SUCCESS;
    }
  }
  pthread_mutex_unlock(&model_lock);
  return CUDA_ERROR_OUT_OF_MEMORY;
}

CUresult CUDAAPI
cuMemHostUnregister(void* address)
{
  int index;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_HOST; index++) {
    if (host_ranges[index].used && host_ranges[index].address == address) {
      host_ranges[index].used = 0;
      pthread_mutex_unlock(&model_lock);
      return CUDA_SUCCESS;
    }
  }
  pthread_mutex_unlock(&model_lock);
  return CUDA_ERROR_HOST_MEMORY_NOT_REGISTERED;
}

CUresult CUDAAPI
cuMemHostGetFlags(unsigned int* flags, void* address)
{
  int index;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_HOST; index++) {
    if (host_ranges[index].used && host_ranges[index].address == address) {
      pthread_mutex_unlock(&model_lock);
      *flags = 0;
      return CUDA_SUCCESS;
    }
  }
  pthread_mutex_unlock(&model_lock);
  return CUDA_ERROR_INVALID_VALUE;
}

/* The fake has no device memory: copies only count bytes. A staging mapping
 * must exist for the device address, as the driver would require. */
static int
staged(CUdeviceptr address, size_t size)
{
  int index;
  int ok = 0;

  pthread_mutex_lock(&model_lock);
  for (index = 0; index < FAKE_MAX_MAPPINGS; index++) {
    if (mappings[index].used && address >= mappings[index].address &&
        address + size <= mappings[index].address + mappings[index].size)
      ok = 1;
  }
  pthread_mutex_unlock(&model_lock);
  return ok;
}

CUresult CUDAAPI
cuMemcpyDtoHAsync_v2(void* host, CUdeviceptr device, size_t size, CUstream stream)
{
  (void)host;
  (void)stream;
  if (should_fail("cuMemcpyDtoHAsync_v2"))
    return CUDA_ERROR_UNKNOWN;
  if (!staged(device, size))
    return CUDA_ERROR_INVALID_VALUE;
  copied_to_host += size;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemcpyHtoDAsync_v2(CUdeviceptr device, const void* host, size_t size, CUstream stream)
{
  (void)host;
  (void)stream;
  if (should_fail("cuMemcpyHtoDAsync_v2"))
    return CUDA_ERROR_UNKNOWN;
  if (!staged(device, size))
    return CUDA_ERROR_INVALID_VALUE;
  copied_to_device += size;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuStreamCreate(CUstream* stream, unsigned int flags)
{
  (void)flags;
  *stream = (CUstream)(uintptr_t)0x5eed;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuStreamSynchronize(CUstream stream)
{
  (void)stream;
  return should_fail("cuStreamSynchronize") ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}

CUresult CUDAAPI
cuStreamDestroy_v2(CUstream stream)
{
  (void)stream;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuCtxGetCurrent(CUcontext* context)
{
  *context = current_context;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuDevicePrimaryCtxRetain(CUcontext* context, CUdevice device)
{
  if (should_fail("cuDevicePrimaryCtxRetain"))
    return CUDA_ERROR_UNKNOWN;
  *context = (CUcontext)(uintptr_t)(0x100 + device);
  pthread_mutex_lock(&model_lock);
  primary_retain_calls++;
  primary_contexts_held++;
  pthread_mutex_unlock(&model_lock);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuDevicePrimaryCtxRelease_v2(CUdevice device)
{
  (void)device;
  pthread_mutex_lock(&model_lock);
  primary_contexts_held--;
  pthread_mutex_unlock(&model_lock);
  return CUDA_SUCCESS;
}

int
fakePrimaryContextRetainCalls(void)
{
  return primary_retain_calls;
}

int
fakePrimaryContextsHeld(void)
{
  return primary_contexts_held;
}

/*
 * cuGetProcAddress in the real driver returns the entry point for the ABI the
 * caller asked for. This fake does the same for the two functions that have a
 * second ABI, and refuses versions newer than itself, like the real driver.
 */
void*
fakeOriginalForVersion(const char* symbol, int version)
{
  static const struct {
    const char* name;
    void* function;
  } table[] = {
      {"cuMemCreate", (void*)&fakeCuMemCreate},
      {"cuMemRelease", (void*)&fakeCuMemRelease},
      {"cuMemRetainAllocationHandle", (void*)&fakeCuMemRetainAllocationHandle},
      {"cuMemMap", (void*)&fakeCuMemMap},
      {"cuMemUnmap", (void*)&fakeCuMemUnmap},
      {"cuMemSetAccess", (void*)&fakeCuMemSetAccess},
      {"cuMemExportToShareableHandle", (void*)&fakeCuMemExportToShareableHandle},
      {"cuMemImportFromShareableHandle", (void*)&fakeCuMemImportFromShareableHandle},
      {"cuMemGetAllocationPropertiesFromHandle", (void*)&fakeCuMemGetAllocationPropertiesFromHandle},
      {"cuMulticastCreate", (void*)&fakeCuMulticastCreate},
      {"cuMulticastAddDevice", (void*)&fakeCuMulticastAddDevice},
      {"cuMulticastBindMem", (void*)&fakeCuMulticastBindMem},
      {"cuMulticastBindAddr", (void*)&fakeCuMulticastBindAddr},
#if CUDA_VERSION >= 13010
      {"cuMulticastBindMem_v2", (void*)&fakeCuMulticastBindMem_v2},
      {"cuMulticastBindAddr_v2", (void*)&fakeCuMulticastBindAddr_v2},
#endif
      {"cuMulticastGetGranularity", (void*)&fakeCuMulticastGetGranularity},
      {"cuMulticastUnbind", (void*)&fakeCuMulticastUnbind},
      {"cuCtxGetCurrent", (void*)&cuCtxGetCurrent},
      /* A local alias: a reference to the global name would bind to the shim. */
      {"cuGetProcAddress", (void*)&fakeCuGetProcAddress},
  };
  size_t index;

#if CUDA_VERSION >= 13010
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindMem") == 0)
    return (void*)&fakeCuMulticastBindMem_v2;
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindAddr") == 0)
    return (void*)&fakeCuMulticastBindAddr_v2;
#else
  (void)version;
#endif
  for (index = 0; index < sizeof(table) / sizeof(table[0]); index++) {
    if (strcmp(symbol, table[index].name) == 0)
      return table[index].function;
  }
  return NULL;
}

void*
fakeOriginal(const char* symbol)
{
  return fakeOriginalForVersion(symbol, CUDA_VERSION);
}

CUresult CUDAAPI
fakeCuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  (void)flags;
  if (version > CUDA_VERSION) {
    *output = NULL;
    return CUDA_ERROR_NOT_FOUND;
  }
  *output = fakeOriginalForVersion(symbol, version);
  return *output == NULL ? CUDA_ERROR_NOT_FOUND : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  return fakeCuGetProcAddress(symbol, output, version, flags);
}

CUresult CUDAAPI
cuGetProcAddress_v2(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  CUresult result = fakeCuGetProcAddress(symbol, output, version, flags);
  if (status != NULL) {
    if (result == CUDA_SUCCESS)
      *status = CU_GET_PROC_ADDRESS_SUCCESS;
    else
      *status = version > CUDA_VERSION ? CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT
                                       : CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  }
  return result;
}

CUresult CUDAAPI
cuGetProcAddress_v2_ptsz(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  return cuGetProcAddress_v2(symbol, output, version, flags, status);
}
