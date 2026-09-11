/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "host_carrier.h"

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>

#include "context.h"
#include "symbols.h"
#include "util/cleanup.h"
#include "util/time.h"

typedef CUresult(CUDAAPI* create_fn)(
    CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
typedef CUresult(CUDAAPI* release_fn)(CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* address_reserve_fn)(CUdeviceptr*, size_t, size_t, CUdeviceptr, unsigned long long);
typedef CUresult(CUDAAPI* address_free_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* map_fn)(
    CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
typedef CUresult(CUDAAPI* unmap_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* access_fn)(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
typedef CUresult(CUDAAPI* host_register_fn)(void*, size_t, unsigned int);
typedef CUresult(CUDAAPI* host_unregister_fn)(void*);
typedef CUresult(CUDAAPI* host_get_flags_fn)(unsigned int*, void*);
typedef CUresult(CUDAAPI* copy_dtoh_async_fn)(void*, CUdeviceptr, size_t, CUstream);
typedef CUresult(CUDAAPI* copy_htod_async_fn)(CUdeviceptr, const void*, size_t, CUstream);
typedef CUresult(CUDAAPI* stream_create_fn)(CUstream*, unsigned int);
typedef CUresult(CUDAAPI* stream_synchronize_fn)(CUstream);
typedef CUresult(CUDAAPI* stream_destroy_fn)(CUstream);

/*
 * One anonymous arena per process is registered once as portable pinned
 * memory. CRIU captures it with the process image. Allocations sharing a CUDA
 * context are staged into one contiguous device VA range and copied on one
 * stream, avoiding per-allocation registration, mapping, and synchronization.
 */
struct carrier_arena {
  void* base;
  size_t size;
  CUcontext context;
  CUdevice device;
};

struct staging_range {
  CUdeviceptr base;
  size_t size;
};

static struct carrier_arena arena;

static CUdevice
fallback_device(const struct cuinterpose_host_carrier_allocation* allocation)
{
  return allocation->properties.location.type == CU_MEM_LOCATION_TYPE_DEVICE
             ? (CUdevice)allocation->properties.location.id
             : CUINTERPOSE_NO_DEVICE;
}

static int
enter_context(
    const struct cuinterpose_host_carrier_allocation* allocation,
    struct cuinterpose_context_scope* scope)
{
  return cuinterpose_enter_context(allocation->context, fallback_device(allocation), scope);
}

static int
compare_by_context(const void* left, const void* right)
{
  uintptr_t a =
      (uintptr_t)((const struct cuinterpose_host_carrier_allocation*)left)->context;
  uintptr_t b =
      (uintptr_t)((const struct cuinterpose_host_carrier_allocation*)right)->context;

  return a < b ? -1 : a > b ? 1 : 0;
}

static int
prepare_allocations(
    struct cuinterpose_host_carrier_allocation* allocations, size_t count,
    uint64_t* total, const char** error)
{
  size_t index;

  *total = 0;
  for (index = 0; index < count; index++) {
    if (allocations[index].size == 0 || allocations[index].device_handle == NULL ||
        allocations[index].host_address == NULL ||
        UINT64_MAX - *total < allocations[index].size) {
      *error = "invalid host carrier allocation";
      return -1;
    }
    *total += allocations[index].size;
  }
  if (*total > SIZE_MAX) {
    *error = "host carrier arena is too large";
    return -1;
  }
  qsort(allocations, count, sizeof(*allocations), compare_by_context);
  return 0;
}

static size_t
context_batch_end(
    const struct cuinterpose_host_carrier_allocation* allocations, size_t count,
    size_t begin, size_t* bytes)
{
  size_t end = begin;

  *bytes = 0;
  while (end < count && allocations[end].context == allocations[begin].context) {
    *bytes += allocations[end].size;
    end++;
  }
  return end;
}

/*
 * Map a context batch at consecutive offsets and grant access from each
 * allocation's original location. Nothing remains mapped on failure.
 */
static CUresult
map_staging_range(
    const struct cuinterpose_host_carrier_allocation* allocations,
    const CUmemGenericAllocationHandle* handles, size_t count, size_t total,
    struct staging_range* range)
{
  address_reserve_fn reserve =
      (address_reserve_fn)cuinterpose_lookup_real_symbol("cuMemAddressReserve");
  address_free_fn free_address =
      (address_free_fn)cuinterpose_lookup_real_symbol("cuMemAddressFree");
  map_fn map = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  access_fn set_access =
      (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  size_t offset = 0;
  size_t mapped = 0;
  size_t index;
  CUresult result;

  memset(range, 0, sizeof(*range));
  if (reserve == NULL || free_address == NULL || map == NULL || unmap == NULL ||
      set_access == NULL)
    return CUDA_ERROR_NOT_INITIALIZED;
  result = reserve(&range->base, total, 0, 0, 0);
  if (result != CUDA_SUCCESS) {
    range->base = 0;
    return result;
  }
  for (index = 0; index < count; index++) {
    CUmemAccessDesc access;

    result = map(range->base + offset, allocations[index].size, 0, handles[index], 0);
    if (result != CUDA_SUCCESS)
      break;
    mapped = offset + allocations[index].size;
    memset(&access, 0, sizeof(access));
    access.location = allocations[index].properties.location;
    access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    result = set_access(range->base + offset, allocations[index].size, &access, 1);
    if (result != CUDA_SUCCESS)
      break;
    offset = mapped;
  }
  if (result != CUDA_SUCCESS) {
    if (mapped != 0)
      (void)unmap(range->base, mapped);
    (void)free_address(range->base, total);
    range->base = 0;
    return result;
  }
  range->size = total;
  return CUDA_SUCCESS;
}

static void
unmap_staging_range(struct staging_range* range)
{
  unmap_fn unmap;
  address_free_fn free_address;

  if (range->base == 0)
    return;
  unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  free_address = (address_free_fn)cuinterpose_lookup_real_symbol("cuMemAddressFree");
  if (unmap != NULL)
    (void)unmap(range->base, range->size);
  if (free_address != NULL)
    (void)free_address(range->base, range->size);
  memset(range, 0, sizeof(*range));
}

struct save_operation {
  struct cuinterpose_host_carrier_allocation* allocations;
  size_t count;
  host_unregister_fn unregister_host;
  stream_destroy_fn stream_destroy;
  struct cuinterpose_context_scope scope;
  struct staging_range range;
  CUmemGenericAllocationHandle* handles;
  CUstream stream;
  void* base;
  uint64_t total;
  bool entered;
  bool registered;
  bool rollback;
  bool committed;
};

static void
save_operation_release(struct save_operation* operation)
{
  size_t index;

  unmap_staging_range(&operation->range);
  if (operation->stream != NULL && operation->stream_destroy != NULL)
    (void)operation->stream_destroy(operation->stream);
  if (operation->rollback && !operation->committed) {
    if (operation->registered) {
      if (!operation->entered && operation->count != 0 &&
          enter_context(&operation->allocations[0], &operation->scope) == 0)
        operation->entered = true;
      (void)operation->unregister_host(operation->base);
    }
    if (operation->base != MAP_FAILED)
      munmap(operation->base, (size_t)operation->total);
    for (index = 0; index < operation->count; index++)
      *operation->allocations[index].host_address = NULL;
  }
  if (operation->entered)
    (void)cuinterpose_leave_context(&operation->scope);
  free(operation->handles);
}

struct load_operation {
  struct cuinterpose_host_carrier_allocation* allocations;
  size_t count;
  release_fn release;
  stream_destroy_fn stream_destroy;
  struct cuinterpose_context_scope scope;
  struct staging_range range;
  CUmemGenericAllocationHandle* fresh;
  CUstream stream;
  bool entered;
  bool committed;
};

static void
load_operation_release(struct load_operation* operation)
{
  size_t index;

  unmap_staging_range(&operation->range);
  if (operation->stream != NULL && operation->stream_destroy != NULL)
    (void)operation->stream_destroy(operation->stream);
  if (!operation->committed && operation->fresh != NULL) {
    for (index = 0; index < operation->count; index++) {
      if (operation->fresh[index] != 0)
        (void)operation->release(operation->fresh[index]);
    }
  }
  if (operation->entered)
    (void)cuinterpose_leave_context(&operation->scope);
  free(operation->fresh);
}

int
cuinterpose_host_carrier_save(
    struct cuinterpose_host_carrier_allocation* allocations, size_t count,
    uint64_t* bytes, uint32_t* copy_us, const char** error)
{
  host_register_fn register_host =
      (host_register_fn)cuinterpose_lookup_real_symbol("cuMemHostRegister_v2");
  host_unregister_fn unregister_host =
      (host_unregister_fn)cuinterpose_lookup_real_symbol("cuMemHostUnregister");
  copy_dtoh_async_fn copy =
      (copy_dtoh_async_fn)cuinterpose_lookup_real_symbol("cuMemcpyDtoHAsync_v2");
  stream_create_fn stream_create =
      (stream_create_fn)cuinterpose_lookup_real_symbol("cuStreamCreate");
  stream_synchronize_fn stream_synchronize =
      (stream_synchronize_fn)cuinterpose_lookup_real_symbol("cuStreamSynchronize");
  stream_destroy_fn stream_destroy =
      (stream_destroy_fn)cuinterpose_lookup_real_symbol("cuStreamDestroy_v2");
  CUINTERPOSE_CLEANUP(save_operation_release) struct save_operation operation = {
      .allocations = allocations,
      .count = count,
      .unregister_host = unregister_host,
      .stream_destroy = stream_destroy,
      .base = MAP_FAILED,
  };
  size_t placed = 0;
  size_t begin;
  size_t end;
  size_t index;
  double copy_time = 0.0;

  *bytes = 0;
  *copy_us = 0;
  *error = "host carrier symbols are unavailable";
  if (register_host == NULL || unregister_host == NULL || copy == NULL ||
      stream_create == NULL || stream_synchronize == NULL || stream_destroy == NULL)
    return -1;
  if (count == 0) {
    *error = NULL;
    return 0;
  }
  if (arena.base != NULL) {
    *error = "a host carrier arena already exists";
    return -1;
  }
  if (prepare_allocations(allocations, count, &operation.total, error) != 0)
    return -1;
  for (index = 0; index < count; index++) {
    if (*allocations[index].device_handle == 0) {
      *error = "creator allocation has no device handle";
      return -1;
    }
  }
  operation.handles = calloc(count, sizeof(*operation.handles));
  if (operation.handles == NULL) {
    *error = "cannot allocate host carrier handles";
    return -1;
  }
  operation.rollback = true;
  operation.base = mmap(
      NULL, (size_t)operation.total, PROT_READ | PROT_WRITE,
      MAP_PRIVATE | MAP_ANONYMOUS | MAP_POPULATE, -1, 0);
  if (operation.base == MAP_FAILED) {
    *error = "cannot allocate host carrier memory";
    return -1;
  }

  for (begin = 0; begin < count; begin = end) {
    struct timespec started;
    struct timespec finished;
    size_t batch_bytes;
    size_t offset = 0;

    end = context_batch_end(allocations, count, begin, &batch_bytes);
    if (enter_context(&allocations[begin], &operation.scope) != 0) {
      *error = "cannot enter the allocation's CUDA context";
      return -1;
    }
    operation.entered = true;
    if (!operation.registered) {
      if (register_host(operation.base, (size_t)operation.total, CU_MEMHOSTREGISTER_PORTABLE) !=
          CUDA_SUCCESS) {
        *error = "cannot pin host carrier memory";
        return -1;
      }
      operation.registered = true;
    }
    for (index = begin; index < end; index++)
      operation.handles[index] = *allocations[index].device_handle;
    if (map_staging_range(
            &allocations[begin], &operation.handles[begin], end - begin, batch_bytes,
            &operation.range) != CUDA_SUCCESS) {
      *error = "cannot map creator allocations for the host copy";
      return -1;
    }
    if (stream_create(&operation.stream, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
      operation.stream = NULL;
      *error = "cannot create the host carrier stream";
      return -1;
    }
    clock_gettime(CLOCK_MONOTONIC, &started);
    for (index = begin; index < end; index++) {
      void* host = (char*)operation.base + placed;

      if (copy(host, operation.range.base + offset, allocations[index].size, operation.stream) !=
          CUDA_SUCCESS) {
        *error = "device-to-host copy failed";
        return -1;
      }
      *allocations[index].host_address = host;
      placed += allocations[index].size;
      offset += allocations[index].size;
    }
    if (stream_synchronize(operation.stream) != CUDA_SUCCESS) {
      *error = "device-to-host copies did not complete";
      return -1;
    }
    clock_gettime(CLOCK_MONOTONIC, &finished);
    copy_time += elapsed_milliseconds(&started, &finished);
    *bytes += batch_bytes;
    unmap_staging_range(&operation.range);
    (void)stream_destroy(operation.stream);
    operation.stream = NULL;
    operation.entered = false;
    if (cuinterpose_leave_context(&operation.scope) != 0) {
      *error = "cannot leave the allocation's CUDA context";
      return -1;
    }
  }
  arena.base = operation.base;
  arena.size = (size_t)operation.total;
  arena.context = allocations[0].context;
  arena.device = fallback_device(&allocations[0]);
  operation.committed = true;
  *copy_us = (uint32_t)(copy_time * 1e3 + 0.5);
  *error = NULL;
  return 0;
}

int
cuinterpose_host_carrier_load(
    struct cuinterpose_host_carrier_allocation* allocations, size_t count,
    uint64_t* bytes, uint32_t* copy_us, const char** error)
{
  create_fn create = (create_fn)cuinterpose_lookup_real_symbol("cuMemCreate");
  release_fn release =
      (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  host_register_fn register_host =
      (host_register_fn)cuinterpose_lookup_real_symbol("cuMemHostRegister_v2");
  host_get_flags_fn host_flags =
      (host_get_flags_fn)cuinterpose_lookup_real_symbol("cuMemHostGetFlags");
  copy_htod_async_fn copy =
      (copy_htod_async_fn)cuinterpose_lookup_real_symbol("cuMemcpyHtoDAsync_v2");
  stream_create_fn stream_create =
      (stream_create_fn)cuinterpose_lookup_real_symbol("cuStreamCreate");
  stream_synchronize_fn stream_synchronize =
      (stream_synchronize_fn)cuinterpose_lookup_real_symbol("cuStreamSynchronize");
  stream_destroy_fn stream_destroy =
      (stream_destroy_fn)cuinterpose_lookup_real_symbol("cuStreamDestroy_v2");
  CUINTERPOSE_CLEANUP(load_operation_release) struct load_operation operation = {
      .allocations = allocations,
      .count = count,
      .release = release,
      .stream_destroy = stream_destroy,
  };
  uint64_t total = 0;
  size_t begin;
  size_t end;
  size_t index;
  double copy_time = 0.0;
  bool registration_checked = false;

  *bytes = 0;
  *copy_us = 0;
  *error = "host carrier symbols are unavailable";
  if (create == NULL || release == NULL || register_host == NULL || copy == NULL ||
      stream_create == NULL || stream_synchronize == NULL || stream_destroy == NULL)
    return -1;
  if (count == 0) {
    *error = NULL;
    return 0;
  }
  if (prepare_allocations(allocations, count, &total, error) != 0)
    return -1;
  if (arena.base == NULL || arena.size != total) {
    *error = "host carrier arena is missing";
    return -1;
  }
  for (index = 0; index < count; index++) {
    if (*allocations[index].host_address == NULL) {
      *error = "host carrier allocation is missing";
      return -1;
    }
  }
  operation.fresh = calloc(count, sizeof(*operation.fresh));
  if (operation.fresh == NULL) {
    *error = "cannot allocate fresh handles";
    return -1;
  }

  for (begin = 0; begin < count; begin = end) {
    struct timespec started;
    struct timespec finished;
    size_t batch_bytes;
    size_t offset = 0;

    end = context_batch_end(allocations, count, begin, &batch_bytes);
    if (enter_context(&allocations[begin], &operation.scope) != 0) {
      *error = "cannot enter the allocation's CUDA context";
      return -1;
    }
    operation.entered = true;
    if (!registration_checked) {
      unsigned int flags = 0;

      if (host_flags == NULL ||
          host_flags(&flags, arena.base) != CUDA_SUCCESS) {
        if (register_host(
                arena.base, arena.size, CU_MEMHOSTREGISTER_PORTABLE) !=
            CUDA_SUCCESS) {
          *error = "cannot pin host carrier memory for the copy back";
          return -1;
        }
        fprintf(
            stderr,
            "cuinterpose: host carrier registration did not survive restore; "
            "re-registered\n");
      }
      registration_checked = true;
      arena.context = allocations[begin].context;
      arena.device = fallback_device(&allocations[begin]);
    }
    for (index = begin; index < end; index++) {
      if (create(
              &operation.fresh[index], allocations[index].size,
              &allocations[index].properties, 0) != CUDA_SUCCESS) {
        operation.fresh[index] = 0;
        *error = "cannot create fresh device memory for a creator allocation";
        return -1;
      }
      if (operation.fresh[index] == 0) {
        CUmemGenericAllocationHandle replacement = 0;

        /* r615 can return zero after restore, but the shim reserves it as
         * its absent-handle sentinel. Keep it live until a distinct handle
         * has been allocated. */
        if (create(
                &replacement, allocations[index].size,
                &allocations[index].properties, 0) != CUDA_SUCCESS ||
            replacement == 0 || release(operation.fresh[index]) != CUDA_SUCCESS) {
          if (replacement != 0)
            (void)release(replacement);
          *error = "cannot replace a zero-valued restored allocation handle";
          return -1;
        }
        operation.fresh[index] = replacement;
      }
    }
    if (map_staging_range(
            &allocations[begin], &operation.fresh[begin], end - begin, batch_bytes,
            &operation.range) != CUDA_SUCCESS) {
      *error = "cannot map fresh device memory for the copy back";
      return -1;
    }
    if (stream_create(&operation.stream, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
      operation.stream = NULL;
      *error = "cannot create the host carrier stream";
      return -1;
    }
    clock_gettime(CLOCK_MONOTONIC, &started);
    for (index = begin; index < end; index++) {
      if (copy(
              operation.range.base + offset, *allocations[index].host_address,
              allocations[index].size, operation.stream) != CUDA_SUCCESS) {
        *error = "host-to-device copy failed";
        return -1;
      }
      offset += allocations[index].size;
    }
    if (stream_synchronize(operation.stream) != CUDA_SUCCESS) {
      *error = "host-to-device copies did not complete";
      return -1;
    }
    clock_gettime(CLOCK_MONOTONIC, &finished);
    copy_time += elapsed_milliseconds(&started, &finished);
    *bytes += batch_bytes;
    unmap_staging_range(&operation.range);
    (void)stream_destroy(operation.stream);
    operation.stream = NULL;
    operation.entered = false;
    if (cuinterpose_leave_context(&operation.scope) != 0) {
      *error = "cannot leave the allocation's CUDA context";
      return -1;
    }
  }

  /* Publish fresh handles only after every copy completed. */
  for (index = 0; index < count; index++) {
    *allocations[index].device_handle = operation.fresh[index];
    operation.fresh[index] = 0;
    *allocations[index].host_address = NULL;
  }
  operation.committed = true;
  *copy_us = (uint32_t)(copy_time * 1e3 + 0.5);
  *error = NULL;
  return 0;
}

void
cuinterpose_host_carrier_release(void)
{
  host_unregister_fn unregister_host =
      (host_unregister_fn)cuinterpose_lookup_real_symbol("cuMemHostUnregister");
  struct cuinterpose_context_scope scope;
  struct carrier_arena local = arena;

  memset(&arena, 0, sizeof(arena));
  if (local.base == NULL)
    return;
  if (unregister_host != NULL &&
      cuinterpose_enter_context(local.context, local.device, &scope) == 0) {
    (void)unregister_host(local.base);
    (void)cuinterpose_leave_context(&scope);
  }
  munmap(local.base, local.size);
}

void
cuinterpose_host_carrier_fork_child(void)
{
  if (arena.base != NULL)
    munmap(arena.base, arena.size);
  memset(&arena, 0, sizeof(arena));
}
