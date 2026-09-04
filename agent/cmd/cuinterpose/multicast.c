/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "multicast.h"

#include <cuda.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/un.h>
#include <unistd.h>

#include "context.h"
#include "export.h"
#include "export_cache.h"
#include "interpose.h"
#include "symbols.h"
#include "table.h"
#include "util.h"

/* cuda.h maps these names to versioned entry points; the shim wraps the names. */
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

typedef CUresult(CUDAAPI* release_fn)(CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* retain_fn)(CUmemGenericAllocationHandle*, void*);
typedef CUresult(CUDAAPI* map_fn)(CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
typedef CUresult(CUDAAPI* unmap_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* access_fn)(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
typedef CUresult(CUDAAPI* export_fn)(void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
typedef CUresult(CUDAAPI* import_fn)(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
typedef CUresult(CUDAAPI* properties_fn)(CUmemAllocationProp*, CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* context_device_fn)(CUdevice*);
typedef CUresult(CUDAAPI* create_fn)(CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
typedef CUresult(CUDAAPI* add_device_fn)(CUmemGenericAllocationHandle, CUdevice);
typedef CUresult(CUDAAPI* bind_mem_fn)(
    CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
typedef CUresult(CUDAAPI* bind_address_fn)(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
#if CUDA_VERSION >= 13010
typedef CUresult(CUDAAPI* bind_mem_v2_fn)(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
typedef CUresult(CUDAAPI* bind_address_v2_fn)(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t, unsigned long long);
#endif
typedef CUresult(CUDAAPI* granularity_fn)(size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
typedef CUresult(CUDAAPI* unbind_fn)(CUmemGenericAllocationHandle, CUdevice, size_t, size_t);

struct multicast;

struct multicast_handle {
  CUmemGenericAllocationHandle logical;
  struct multicast* multicast;
};

struct multicast_binding {
  uint8_t member_id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  CUdeviceptr member_address; /* BIND_ADDR only */
  size_t multicast_offset;
  size_t member_offset;
  size_t size;
  unsigned long long flags;
  CUdevice device;
  uint8_t kind; /* enum cuinterpose_multicast_binding_kind */
  uint8_t api_version; /* 1: device taken from the member; 2: device-explicit (_v2) */
  bool checkpointed; /* unbound by PREPARE_MULTICAST, to be bound again on restore */
};

struct multicast_mapping {
  CUdeviceptr address;
  size_t size;
  size_t offset;
  unsigned long long flags;
  CUmemAccessDesc access[CUINTERPOSE_MAX_ACCESS];
  size_t access_count;
  bool access_unknown; /* a cuMemSetAccess failed part-way; the record cannot be trusted */
  bool mapped;
  bool checkpointed;
  struct multicast* multicast;
};

struct multicast {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint8_t authorization[CUINTERPOSE_TOKEN_SIZE];
  char creator_participant[CUINTERPOSE_ID_SIZE];
  char creator_endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
  CUmulticastObjectProp properties; /* as passed to cuMulticastCreate; replayed on restore */
  size_t effective_size; /* the largest extent the driver accepted; see observe_extent */
  CUcontext context;
  CUmemGenericAllocationHandle driver; /* the one driver handle, 0 while checkpointed */
  bool creator;
  bool shared; /* a ticket was issued (creator) or imported (importer) */
  bool checkpointed;
  unsigned live_handles;
  CUdevice* devices; /* devices this process attached */
  size_t device_count;
  struct multicast_binding* bindings;
  size_t binding_count;
  struct multicast_mapping** mappings;
  size_t mapping_count;
};

static struct cuinterpose_multicast_callbacks ops;
static const char* participant_id;
static const char* endpoint_path;
static struct cuinterpose_table handles; /* logical handle -> struct multicast_handle */
static struct cuinterpose_table objects; /* id -> struct multicast */
static struct cuinterpose_ranges mapped; /* live mappings by address -> struct multicast_mapping */
static atomic_bool fabric_logged;
static atomic_bool untracked_member_logged;

/* ------------------------------------------------------------------------- */
/* Bookkeeping                                                               */
/* ------------------------------------------------------------------------- */

static struct multicast*
find_object(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  return cuinterpose_table_get(&objects, cuinterpose_key_bytes(id));
}

static struct multicast_handle*
find_handle(CUmemGenericAllocationHandle logical)
{
  return cuinterpose_table_get(&handles, cuinterpose_key_u64((uint64_t)logical));
}

static struct multicast_mapping*
mapping_at(CUdeviceptr address)
{
  struct cuinterpose_range* range = cuinterpose_ranges_at(&mapped, (uint64_t)address);

  return range != NULL ? range->value : NULL;
}

/* An object is alive while the application holds a handle or a mapping of it;
 * bindings alone do not keep it, exactly as in the driver. */
static bool
active(const struct multicast* multicast)
{
  size_t index;

  if (multicast->live_handles != 0)
    return true;
  for (index = 0; index < multicast->mapping_count; index++) {
    if (multicast->mappings[index]->mapped)
      return true;
  }
  return false;
}

/* Grows a plain array by one element; returns the new element or NULL. */
static void*
push(void** array, size_t* count, size_t element_size)
{
  void* grown = realloc(*array, (*count + 1) * element_size);

  if (grown == NULL)
    return NULL;
  *array = grown;
  (*count)++;
  return (char*)grown + (*count - 1) * element_size;
}

/*
 * The driver may give a multicast object more capacity than cuMulticastCreate
 * asked for (r615 rounds up to 512 MiB), and the application may then bind and
 * map beyond the requested size. INSPECT reports the largest extent actually
 * used so the coordinator's bounds checks accept it; `properties` keeps the
 * requested size for replay and for ticket identity.
 */
static void
observe_extent(struct multicast* multicast, size_t offset, size_t size)
{
  if (size <= SIZE_MAX - offset && multicast->effective_size < offset + size)
    multicast->effective_size = offset + size;
}

static void
adopt_context(struct multicast* multicast)
{
  if (multicast->context == NULL)
    cuinterpose_capture_context(&multicast->context);
}

/* An object that never had a context is handled in the primary context of the
 * first device it was attached to. */
static int
enter(const struct multicast* multicast, struct cuinterpose_context_scope* scope)
{
  CUdevice fallback = CUINTERPOSE_NO_DEVICE;

  if (multicast->device_count != 0)
    fallback = multicast->devices[0];
  else if (multicast->binding_count != 0)
    fallback = multicast->bindings[0].device;
  return cuinterpose_enter_context(multicast->context, fallback, scope);
}

static int
add_handle(struct multicast* multicast, CUmemGenericAllocationHandle* logical)
{
  struct multicast_handle* handle = calloc(1, sizeof(*handle));

  if (handle == NULL || ops.allocate_logical_handle(&handle->logical) != 0) {
    free(handle);
    return -1;
  }
  handle->multicast = multicast;
  if (cuinterpose_table_put(&handles, cuinterpose_key_u64((uint64_t)handle->logical), handle) != 0) {
    free(handle);
    return -1;
  }
  multicast->live_handles++;
  *logical = handle->logical;
  return 0;
}

static void
remove_mapping(struct multicast* multicast, struct multicast_mapping* mapping)
{
  size_t index;

  for (index = 0; index < multicast->mapping_count; index++) {
    if (multicast->mappings[index] == mapping) {
      multicast->mappings[index] = multicast->mappings[multicast->mapping_count - 1];
      multicast->mapping_count--;
      break;
    }
  }
  free(mapping);
}

static void
free_object(struct multicast* multicast)
{
  size_t index;

  cuinterpose_table_remove(&objects, cuinterpose_key_bytes(multicast->id));
  for (index = 0; index < multicast->mapping_count; index++) {
    struct multicast_mapping* mapping = multicast->mappings[index];
    struct cuinterpose_range* range = cuinterpose_ranges_at(&mapped, (uint64_t)mapping->address);

    if (range != NULL && range->value == mapping)
      cuinterpose_ranges_remove_at(&mapped, (size_t)(range - mapped.items));
    free(mapping);
  }
  free(multicast->mappings);
  free(multicast->bindings);
  free(multicast->devices);
  free(multicast);
}

/* Frees an object nobody holds any more: the cached descriptor goes first,
 * then the driver handle, then the records. */
static void
settle(struct multicast* multicast)
{
  if (active(multicast) || multicast->checkpointed)
    return;
  cuinterpose_export_cache_drop(multicast->id);
  if (multicast->driver != 0) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");

    if (release != NULL)
      (void)release(multicast->driver);
    multicast->driver = 0;
  }
  free_object(multicast);
}

struct object_list {
  struct multicast** items;
  size_t count;
};

static int
collect_object(struct cuinterpose_key key, void* value, void* arg)
{
  struct object_list* list = arg;

  (void)key;
  list->items[list->count++] = value;
  return 0;
}

static int
list_objects(struct object_list* list)
{
  list->count = 0;
  list->items = calloc(objects.count == 0 ? 1 : objects.count, sizeof(*list->items));
  if (list->items == NULL)
    return -1;
  cuinterpose_table_each(&objects, collect_object, list);
  return 0;
}

static void
fill_ticket(const struct multicast* multicast, struct cuinterpose_posix_ticket* ticket)
{
  memset(ticket, 0, sizeof(*ticket));
  ticket->magic = CUINTERPOSE_POSIX_TICKET_MAGIC;
  ticket->version = CUINTERPOSE_POSIX_TICKET_VERSION;
  ticket->resource_kind = CUINTERPOSE_RESOURCE_MULTICAST;
  snprintf(ticket->creator_participant, sizeof(ticket->creator_participant), "%s", multicast->creator_participant);
  memcpy(ticket->allocation_id, multicast->id, sizeof(ticket->allocation_id));
  snprintf(ticket->creator_endpoint, sizeof(ticket->creator_endpoint), "%s", multicast->creator_endpoint);
  memcpy(ticket->authorization, multicast->authorization, sizeof(ticket->authorization));
  ticket->allocation_size = multicast->properties.size;
  ticket->handle_types = multicast->properties.handleTypes;
  ticket->object_flags = multicast->properties.flags;
  ticket->num_devices = multicast->properties.numDevices;
}

static bool
matches_ticket(const struct multicast* multicast, const struct cuinterpose_posix_ticket* ticket)
{
  return memcmp(multicast->authorization, ticket->authorization, sizeof(multicast->authorization)) == 0 &&
         strcmp(multicast->creator_participant, ticket->creator_participant) == 0 &&
         strcmp(multicast->creator_endpoint, ticket->creator_endpoint) == 0 &&
         multicast->properties.numDevices == ticket->num_devices &&
         multicast->properties.size == ticket->allocation_size &&
         multicast->properties.handleTypes == ticket->handle_types &&
         multicast->properties.flags == ticket->object_flags;
}

/* Merge descriptors into a mapping's per-location access set (same rules as
 * interpose.c: one entry per location, PROT_NONE removes it). */
static int
merge_access(
    const struct multicast_mapping* mapping, const CUmemAccessDesc* descriptors, size_t count, CUmemAccessDesc* merged,
    size_t* merged_count)
{
  size_t index;

  memcpy(merged, mapping->access, mapping->access_count * sizeof(*merged));
  *merged_count = mapping->access_count;
  for (index = 0; index < count; index++) {
    const CUmemAccessDesc* descriptor = &descriptors[index];
    size_t slot;

    for (slot = 0; slot < *merged_count; slot++) {
      if (merged[slot].location.type == descriptor->location.type &&
          merged[slot].location.id == descriptor->location.id)
        break;
    }
    if (descriptor->flags == CU_MEM_ACCESS_FLAGS_PROT_NONE) {
      if (slot < *merged_count) {
        merged[slot] = merged[*merged_count - 1];
        (*merged_count)--;
      }
      continue;
    }
    if (slot == *merged_count) {
      if (*merged_count == CUINTERPOSE_MAX_ACCESS)
        return -1;
      (*merged_count)++;
    }
    merged[slot] = *descriptor;
  }
  return 0;
}

/* ------------------------------------------------------------------------- */
/* Driver calls shared by the wrappers and restore                           */
/* ------------------------------------------------------------------------- */

static CUresult
forward_bind_mem(
    CUmemGenericAllocationHandle driver, CUdevice device, bool device_explicit, size_t multicast_offset,
    CUmemGenericAllocationHandle member, size_t member_offset, size_t size, unsigned long long flags)
{
  if (device_explicit) {
#if CUDA_VERSION >= 13010
    bind_mem_v2_fn function = (bind_mem_v2_fn)cuinterpose_lookup_real_symbol("cuMulticastBindMem_v2");

    if (function == NULL)
      return cuinterpose_unavailable();
    return function(driver, device, multicast_offset, member, member_offset, size, flags);
#else
    (void)device;
    return CUDA_ERROR_NOT_SUPPORTED;
#endif
  }
  {
    bind_mem_fn function = (bind_mem_fn)cuinterpose_lookup_real_symbol("cuMulticastBindMem");

    if (function == NULL)
      return cuinterpose_unavailable();
    return function(driver, multicast_offset, member, member_offset, size, flags);
  }
}

static CUresult
forward_bind_address(
    CUmemGenericAllocationHandle driver, CUdevice device, bool device_explicit, size_t multicast_offset,
    CUdeviceptr member, size_t size, unsigned long long flags)
{
  if (device_explicit) {
#if CUDA_VERSION >= 13010
    bind_address_v2_fn function = (bind_address_v2_fn)cuinterpose_lookup_real_symbol("cuMulticastBindAddr_v2");

    if (function == NULL)
      return cuinterpose_unavailable();
    return function(driver, device, multicast_offset, member, size, flags);
#else
    (void)device;
    return CUDA_ERROR_NOT_SUPPORTED;
#endif
  }
  {
    bind_address_fn function = (bind_address_fn)cuinterpose_lookup_real_symbol("cuMulticastBindAddr");

    if (function == NULL)
      return cuinterpose_unavailable();
    return function(driver, multicast_offset, member, size, flags);
  }
}

static CUresult
replay_binding(CUmemGenericAllocationHandle driver, const struct multicast_binding* binding, const char** error)
{
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  CUresult result;

  if (binding->kind == CUINTERPOSE_MULTICAST_BIND_MEM) {
    struct cuinterpose_multicast_member member;

    memset(&member, 0, sizeof(member));
    if (ops.member_from_id(binding->member_id, &member) != 0) {
      *error = "a multicast member allocation is missing after restore";
      return CUDA_ERROR_INVALID_VALUE;
    }
    result = forward_bind_mem(
        driver, binding->device, binding->api_version == 2, binding->multicast_offset, member.handle,
        binding->member_offset, binding->size, binding->flags);
    if (member.temporary_handle && release != NULL) {
      CUresult released = release(member.handle);
      if (result == CUDA_SUCCESS)
        result = released;
    }
  } else {
    result = forward_bind_address(
        driver, binding->device, binding->api_version == 2, binding->multicast_offset, binding->member_address,
        binding->size, binding->flags);
  }
  if (result != CUDA_SUCCESS)
    *error = "cuMulticastBind failed during restore";
  return result;
}

/* ------------------------------------------------------------------------- */
/* Module interface                                                          */
/* ------------------------------------------------------------------------- */

void
cuinterpose_multicast_initialize(
    const struct cuinterpose_multicast_callbacks* callbacks, const char* participant, const char* endpoint)
{
  ops = *callbacks;
  participant_id = participant;
  endpoint_path = endpoint;
}

static int
free_handle_value(struct cuinterpose_key key, void* value, void* arg)
{
  (void)key;
  (void)arg;
  free(value);
  return 0;
}

static int
free_object_value(struct cuinterpose_key key, void* value, void* arg)
{
  struct multicast* multicast = value;
  size_t index;

  (void)key;
  (void)arg;
  for (index = 0; index < multicast->mapping_count; index++)
    free(multicast->mappings[index]);
  free(multicast->mappings);
  free(multicast->bindings);
  free(multicast->devices);
  free(multicast);
  return 0;
}

void
cuinterpose_multicast_fork_child(void)
{
  cuinterpose_table_each(&handles, free_handle_value, NULL);
  cuinterpose_table_clear(&handles);
  cuinterpose_table_each(&objects, free_object_value, NULL);
  cuinterpose_table_clear(&objects);
  cuinterpose_ranges_clear(&mapped);
}

size_t
cuinterpose_multicast_count(void)
{
  return objects.count;
}

bool
cuinterpose_multicast_is_handle(CUmemGenericAllocationHandle logical)
{
  return find_handle(logical) != NULL;
}

bool
cuinterpose_multicast_owns_range(CUdeviceptr address, size_t size)
{
  size_t first;
  size_t last;

  if (size == 0 || (uint64_t)address > UINT64_MAX - size)
    return false;
  (void)cuinterpose_ranges_cover(&mapped, (uint64_t)address, (uint64_t)address + size, &first, &last);
  return first != last;
}

CUresult
cuinterpose_multicast_release(CUmemGenericAllocationHandle logical)
{
  struct multicast_handle* handle = find_handle(logical);
  struct multicast* multicast;

  if (handle == NULL)
    return CUDA_ERROR_INVALID_HANDLE;
  if (!ops.state_is_active())
    return CUDA_ERROR_NOT_READY;
  multicast = handle->multicast;
  cuinterpose_table_remove(&handles, cuinterpose_key_u64((uint64_t)logical));
  free(handle);
  multicast->live_handles--;
  settle(multicast);
  return CUDA_SUCCESS;
}

CUresult
cuinterpose_multicast_retain(CUmemGenericAllocationHandle* output, void* address)
{
  struct multicast_mapping* mapping = mapping_at((CUdeviceptr)(uintptr_t)address);
  struct multicast* multicast;
  CUmemGenericAllocationHandle logical;

  if (mapping == NULL || !mapping->mapped)
    return CUDA_ERROR_INVALID_VALUE;
  if (!ops.state_is_active())
    return CUDA_ERROR_NOT_READY;
  multicast = mapping->multicast;
  if (multicast->driver == 0) {
    retain_fn retain = (retain_fn)cuinterpose_lookup_real_symbol("cuMemRetainAllocationHandle");
    CUresult result;

    if (retain == NULL)
      return cuinterpose_unavailable();
    result = retain(&multicast->driver, address);
    if (result != CUDA_SUCCESS) {
      multicast->driver = 0;
      return result;
    }
  }
  /* The one driver handle is already held; the new logical handle aliases it. */
  if (add_handle(multicast, &logical) != 0)
    return CUDA_ERROR_OUT_OF_MEMORY;
  *output = logical;
  return CUDA_SUCCESS;
}

CUresult
cuinterpose_multicast_map(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle logical, unsigned long long flags)
{
  map_fn map = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  struct multicast_handle* handle = find_handle(logical);
  struct multicast* multicast;
  struct multicast_mapping* mapping;
  struct multicast_mapping** slot;
  CUmemGenericAllocationHandle driver;
  CUresult result;

  if (handle == NULL)
    return CUDA_ERROR_INVALID_HANDLE;
  if (!ops.state_is_active() || handle->multicast->driver == 0)
    return CUDA_ERROR_NOT_READY;
  if (map == NULL || unmap == NULL)
    return cuinterpose_unavailable();
  if (size == 0 || (uint64_t)address > UINT64_MAX - size)
    return CUDA_ERROR_INVALID_VALUE;
  mapping = calloc(1, sizeof(*mapping));
  if (mapping == NULL)
    return CUDA_ERROR_OUT_OF_MEMORY;
  /* Reserve the range before the driver sees it: two records for one address
   * would make restore ambiguous. */
  switch (cuinterpose_ranges_insert(&mapped, (uint64_t)address, (uint64_t)address + size, mapping)) {
    case 1:
      free(mapping);
      return CUDA_ERROR_INVALID_VALUE;
    case -1:
      free(mapping);
      return CUDA_ERROR_OUT_OF_MEMORY;
    default:
      break;
  }
  /*
   * Mapping a multicast object waits for the whole team, so the lock is not
   * held across it: other threads of this process must be able to go on
   * allocating, and a checkpoint that starts meanwhile must be able to see
   * the state. Everything is looked up again afterwards.
   */
  driver = handle->multicast->driver;
  ops.release_state_lock();
  result = map(address, size, offset, driver, flags);
  ops.acquire_state_lock();
  handle = find_handle(logical);
  if (result != CUDA_SUCCESS || handle == NULL || handle->multicast->driver != driver || !ops.state_is_active()) {
    struct cuinterpose_range* range = cuinterpose_ranges_at(&mapped, (uint64_t)address);

    if (range != NULL && range->value == mapping)
      cuinterpose_ranges_remove_at(&mapped, (size_t)(range - mapped.items));
    free(mapping);
    if (result != CUDA_SUCCESS)
      return result;
    (void)unmap(address, size);
    return handle == NULL ? CUDA_ERROR_INVALID_HANDLE : CUDA_ERROR_NOT_READY;
  }
  multicast = handle->multicast;
  slot = push((void**)&multicast->mappings, &multicast->mapping_count, sizeof(*multicast->mappings));
  if (slot == NULL) {
    struct cuinterpose_range* range = cuinterpose_ranges_at(&mapped, (uint64_t)address);

    if (range != NULL && range->value == mapping)
      cuinterpose_ranges_remove_at(&mapped, (size_t)(range - mapped.items));
    free(mapping);
    (void)unmap(address, size);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *slot = mapping;
  mapping->address = address;
  mapping->size = size;
  mapping->offset = offset;
  mapping->flags = flags;
  mapping->mapped = true;
  mapping->multicast = multicast;
  observe_extent(multicast, offset, size);
  adopt_context(multicast);
  return CUDA_SUCCESS;
}

CUresult
cuinterpose_multicast_unmap(CUdeviceptr address, size_t size)
{
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  struct multicast** affected;
  size_t affected_count = 0;
  size_t first;
  size_t last;
  size_t index;
  CUresult result;

  if (unmap == NULL)
    return cuinterpose_unavailable();
  if (cuinterpose_ranges_cover(&mapped, (uint64_t)address, (uint64_t)address + size, &first, &last) != 0 ||
      first == last)
    return CUDA_ERROR_INVALID_VALUE;
  for (index = first; index < last; index++) {
    const struct multicast_mapping* mapping = mapped.items[index].value;
    if (!mapping->mapped)
      return CUDA_ERROR_INVALID_VALUE;
  }
  if (!ops.state_is_active())
    return CUDA_ERROR_NOT_READY;
  affected = calloc(last - first, sizeof(*affected));
  if (affected == NULL)
    return CUDA_ERROR_OUT_OF_MEMORY;
  result = unmap(address, size);
  if (result != CUDA_SUCCESS) {
    free(affected);
    return result;
  }
  for (index = last; index > first; index--) {
    struct multicast_mapping* mapping = mapped.items[index - 1].value;
    struct multicast* multicast = mapping->multicast;
    size_t seen;

    cuinterpose_ranges_remove_at(&mapped, index - 1);
    remove_mapping(multicast, mapping);
    for (seen = 0; seen < affected_count; seen++) {
      if (affected[seen] == multicast)
        break;
    }
    if (seen == affected_count)
      affected[affected_count++] = multicast;
  }
  for (index = 0; index < affected_count; index++)
    settle(affected[index]);
  free(affected);
  return CUDA_SUCCESS;
}

CUresult
cuinterpose_multicast_set_access(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  access_fn set_access = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  CUmemAccessDesc (*merged)[CUINTERPOSE_MAX_ACCESS] = NULL;
  size_t* merged_counts = NULL;
  size_t first;
  size_t last;
  size_t index;
  CUresult result;

  if (set_access == NULL)
    return cuinterpose_unavailable();
  if (cuinterpose_ranges_cover(&mapped, (uint64_t)address, (uint64_t)address + size, &first, &last) != 0 ||
      first == last)
    return CUDA_ERROR_INVALID_VALUE;
  for (index = first; index < last; index++) {
    const struct multicast_mapping* mapping = mapped.items[index].value;
    if (!mapping->mapped)
      return CUDA_ERROR_INVALID_VALUE;
  }
  if (!ops.state_is_active())
    return CUDA_ERROR_NOT_READY;
  merged = calloc(last - first, sizeof(*merged));
  merged_counts = calloc(last - first, sizeof(*merged_counts));
  if (merged == NULL || merged_counts == NULL) {
    free(merged);
    free(merged_counts);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  for (index = first; index < last; index++) {
    if (merge_access(mapped.items[index].value, descriptors, count, merged[index - first], &merged_counts[index - first]) !=
        0) {
      free(merged);
      free(merged_counts);
      return CUDA_ERROR_NOT_SUPPORTED;
    }
  }
  result = set_access(address, size, descriptors, count);
  for (index = first; index < last; index++) {
    struct multicast_mapping* mapping = mapped.items[index].value;
    if (result == CUDA_SUCCESS) {
      memcpy(mapping->access, merged[index - first], merged_counts[index - first] * sizeof(*mapping->access));
      mapping->access_count = merged_counts[index - first];
    } else {
      mapping->access_unknown = true;
    }
  }
  free(merged);
  free(merged_counts);
  return result;
}

CUresult
cuinterpose_multicast_export(
    void* shareable, CUmemGenericAllocationHandle logical, CUmemAllocationHandleType type, unsigned long long flags)
{
  struct multicast_handle* handle = find_handle(logical);
  struct multicast* multicast;
  struct cuinterpose_posix_ticket ticket;
  int ticket_fd = -1;

  if (handle == NULL)
    return CUDA_ERROR_INVALID_HANDLE;
  if (shareable == NULL || flags != 0 || type != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
    return CUDA_ERROR_INVALID_VALUE;
  if (!ops.state_is_active())
    return CUDA_ERROR_NOT_READY;
  multicast = handle->multicast;
  adopt_context(multicast);
  if (multicast->creator && !cuinterpose_export_cache_has(multicast->id)) {
    /* The one real export of this object in this process; peers are served
     * duplicates of the descriptor from the export cache. */
    export_fn export_handle = (export_fn)cuinterpose_lookup_real_symbol("cuMemExportToShareableHandle");
    int real_fd = -1;
    CUresult result;

    if (export_handle == NULL)
      return cuinterpose_unavailable();
    if (multicast->driver == 0)
      return CUDA_ERROR_INVALID_HANDLE;
    result = export_handle(&real_fd, multicast->driver, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0);
    if (result != CUDA_SUCCESS)
      return result;
    if (cuinterpose_export_cache_put(multicast->id, multicast->authorization, real_fd) != 0) {
      close(real_fd);
      return CUDA_ERROR_OUT_OF_MEMORY;
    }
  }
  fill_ticket(multicast, &ticket);
  if (cuinterpose_posix_create_ticket(&ticket, &ticket_fd) != 0)
    return CUDA_ERROR_OUT_OF_MEMORY;
  multicast->shared = true;
  *(int*)shareable = ticket_fd;
  return CUDA_SUCCESS;
}

CUresult
cuinterpose_multicast_get_properties(CUmemAllocationProp* properties, CUmemGenericAllocationHandle logical)
{
  properties_fn function = (properties_fn)cuinterpose_lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");
  struct multicast_handle* handle = find_handle(logical);

  if (handle == NULL)
    return CUDA_ERROR_INVALID_HANDLE;
  if (!ops.state_is_active() || handle->multicast->driver == 0)
    return CUDA_ERROR_NOT_READY;
  return function != NULL ? function(properties, handle->multicast->driver) : cuinterpose_unavailable();
}

CUresult
cuinterpose_multicast_import(CUmemGenericAllocationHandle* output, const struct cuinterpose_posix_ticket* ticket)
{
  import_fn import_handle = (import_fn)cuinterpose_lookup_real_symbol("cuMemImportFromShareableHandle");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct multicast* multicast;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  bool created = false;
  int raw_fd = -1;
  CUresult result;

  if (import_handle == NULL || release == NULL)
    return cuinterpose_unavailable();
  if (ticket->handle_types != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
    return CUDA_ERROR_INVALID_HANDLE;
  ops.acquire_state_lock();
  if (!ops.state_is_active()) {
    ops.release_state_lock();
    return CUDA_ERROR_NOT_READY;
  }
  ops.release_state_lock();
  if (cuinterpose_posix_request_export(ticket, &raw_fd, NULL, 0) != 0)
    return CUDA_ERROR_INVALID_HANDLE;
  result = import_handle(&driver, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
  close(raw_fd);
  if (result != CUDA_SUCCESS)
    return result;
  ops.acquire_state_lock();
  if (!ops.state_is_active()) {
    ops.release_state_lock();
    (void)release(driver);
    return CUDA_ERROR_NOT_READY;
  }
  multicast = find_object(ticket->allocation_id);
  if (multicast != NULL && !matches_ticket(multicast, ticket)) {
    ops.release_state_lock();
    (void)release(driver);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  if (multicast == NULL) {
    multicast = calloc(1, sizeof(*multicast));
    if (multicast == NULL ||
        cuinterpose_table_put(&objects, cuinterpose_key_bytes(ticket->allocation_id), multicast) != 0) {
      ops.release_state_lock();
      (void)release(driver);
      free(multicast);
      return CUDA_ERROR_OUT_OF_MEMORY;
    }
    created = true;
    memcpy(multicast->id, ticket->allocation_id, sizeof(multicast->id));
    memcpy(multicast->authorization, ticket->authorization, sizeof(multicast->authorization));
    snprintf(
        multicast->creator_participant, sizeof(multicast->creator_participant), "%s", ticket->creator_participant);
    snprintf(multicast->creator_endpoint, sizeof(multicast->creator_endpoint), "%s", ticket->creator_endpoint);
    multicast->properties.numDevices = ticket->num_devices;
    multicast->properties.size = ticket->allocation_size;
    multicast->properties.handleTypes = ticket->handle_types;
    multicast->properties.flags = ticket->object_flags;
    multicast->effective_size = ticket->allocation_size;
    multicast->creator = false;
    multicast->driver = driver;
    cuinterpose_capture_context(&multicast->context);
  } else if (multicast->driver != 0) {
    /* Already imported here: the new logical handle aliases the driver handle. */
    if (release(driver) != CUDA_SUCCESS) {
      ops.release_state_lock();
      return CUDA_ERROR_UNKNOWN;
    }
  } else {
    multicast->driver = driver;
  }
  multicast->shared = true;
  if (add_handle(multicast, &logical) != 0) {
    if (created) {
      (void)release(multicast->driver);
      multicast->driver = 0;
      free_object(multicast);
    }
    ops.release_state_lock();
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  ops.release_state_lock();
  return CUDA_SUCCESS;
}

/* ------------------------------------------------------------------------- */
/* INSPECT records                                                           */
/* ------------------------------------------------------------------------- */

static int
count_records(struct cuinterpose_key key, void* value, void* arg)
{
  const struct multicast* multicast = value;
  size_t* total = arg;
  size_t index;

  (void)key;
  if (!active(multicast))
    return 0;
  *total += 1 + multicast->device_count + multicast->binding_count;
  for (index = 0; index < multicast->mapping_count; index++)
    *total += multicast->mappings[index]->mapped ? 1 : 0;
  return 0;
}

size_t
cuinterpose_multicast_record_count(void)
{
  size_t total = 0;

  cuinterpose_table_each(&objects, count_records, &total);
  return total;
}

int
cuinterpose_multicast_write_records(struct cuinterpose_record* records, size_t count, const char** error)
{
  struct object_list list;
  size_t written = 0;
  size_t index;

  if (list_objects(&list) != 0) {
    *error = "cannot allocate inspect records";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    const struct multicast* multicast = list.items[index];
    struct cuinterpose_record* record;
    size_t item;

    if (!active(multicast))
      continue;
    if (written == count)
      goto overflow;
    record = &records[written++];
    record->kind = CUINTERPOSE_MULTICAST;
    record->flags = multicast->creator ? CUINTERPOSE_CREATOR : 0;
    if (multicast->live_handles != 0)
      record->flags |= CUINTERPOSE_APPLICATION_HANDLE_LIVE;
    memcpy(record->allocation_id, multicast->id, sizeof(record->allocation_id));
    record->allocation_size = multicast->effective_size;
    record->application_handle_count = multicast->live_handles;
    record->handle_types = multicast->properties.handleTypes;
    record->object_flags = multicast->properties.flags;
    record->num_devices = multicast->properties.numDevices;
    snprintf(record->creator_participant, sizeof(record->creator_participant), "%s", multicast->creator_participant);
    for (item = 0; item < multicast->device_count; item++) {
      if (written == count)
        goto overflow;
      record = &records[written++];
      record->kind = CUINTERPOSE_MULTICAST_DEVICE;
      memcpy(record->allocation_id, multicast->id, sizeof(record->allocation_id));
      record->device = multicast->devices[item];
    }
    for (item = 0; item < multicast->binding_count; item++) {
      const struct multicast_binding* binding = &multicast->bindings[item];

      if (written == count)
        goto overflow;
      record = &records[written++];
      record->kind = CUINTERPOSE_MULTICAST_BINDING;
      memcpy(record->allocation_id, multicast->id, sizeof(record->allocation_id));
      memcpy(record->member_id, binding->member_id, sizeof(record->member_id));
      record->address = binding->member_address;
      record->size = binding->size;
      record->offset = binding->multicast_offset;
      record->member_offset = binding->member_offset;
      record->operation_flags = binding->flags;
      record->binding_kind = binding->kind;
      record->api_version = binding->api_version;
      record->device = binding->device;
    }
    for (item = 0; item < multicast->mapping_count; item++) {
      const struct multicast_mapping* mapping = multicast->mappings[item];
      size_t access;

      if (!mapping->mapped)
        continue;
      if (mapping->access_unknown) {
        free(list.items);
        *error = "a multicast mapping's access state is unknown after a failed cuMemSetAccess";
        return -1;
      }
      if (written == count)
        goto overflow;
      record = &records[written++];
      record->kind = CUINTERPOSE_MULTICAST_MAPPING;
      memcpy(record->allocation_id, multicast->id, sizeof(record->allocation_id));
      record->address = mapping->address;
      record->size = mapping->size;
      record->offset = mapping->offset;
      record->operation_flags = mapping->flags;
      record->access_count = (uint32_t)mapping->access_count;
      for (access = 0; access < mapping->access_count; access++) {
        record->access[access].location_type = mapping->access[access].location.type;
        record->access[access].location_id = mapping->access[access].location.id;
        record->access[access].flags = mapping->access[access].flags;
      }
    }
  }
  free(list.items);
  if (written != count) {
    *error = "multicast records changed while being described";
    return -1;
  }
  return 0;
overflow:
  free(list.items);
  *error = "multicast records changed while being described";
  return -1;
}

/* ------------------------------------------------------------------------- */
/* Lifecycle                                                                 */
/* ------------------------------------------------------------------------- */

/*
 * PREPARE_MULTICAST: take every live object apart so the native checkpoint
 * sees none of it. The cached export descriptor is closed first: it is an
 * independent reference to the object in the driver, and the object must be
 * gone before the checkpoint. Then mappings are unmapped, bindings unbound,
 * and the driver handle released. The records stay for restore.
 */
int
cuinterpose_multicast_prepare(const char** error)
{
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  unbind_fn unbind = (unbind_fn)cuinterpose_lookup_real_symbol("cuMulticastUnbind");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct object_list list;
  size_t index;

  if (unmap == NULL || unbind == NULL || release == NULL) {
    *error = "multicast teardown symbols are unavailable";
    return -1;
  }
  if (list_objects(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct multicast* multicast = list.items[index];
    struct cuinterpose_context_scope scope;
    size_t item;

    if (!active(multicast))
      continue;
    for (item = 0; item < multicast->mapping_count; item++) {
      if (multicast->mappings[item]->mapped && multicast->mappings[item]->access_unknown) {
        free(list.items);
        *error = "a multicast mapping's access state is unknown after a failed cuMemSetAccess";
        return -1;
      }
    }
    if (multicast->driver == 0) {
      free(list.items);
      *error = "a live multicast object has no driver handle";
      return -1;
    }
    cuinterpose_export_cache_drop(multicast->id);
    if (enter(multicast, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the multicast object's CUDA context";
      return -1;
    }
    for (item = 0; item < multicast->mapping_count; item++) {
      struct multicast_mapping* mapping = multicast->mappings[item];
      struct cuinterpose_range* range;

      if (!mapping->mapped)
        continue;
      if (unmap(mapping->address, mapping->size) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "cuMemUnmap of a multicast mapping failed during prepare";
        return -1;
      }
      range = cuinterpose_ranges_at(&mapped, (uint64_t)mapping->address);
      if (range != NULL && range->value == mapping)
        cuinterpose_ranges_remove_at(&mapped, (size_t)(range - mapped.items));
      mapping->mapped = false;
      mapping->checkpointed = true;
    }
    for (item = 0; item < multicast->binding_count; item++) {
      struct multicast_binding* binding = &multicast->bindings[item];

      if (unbind(multicast->driver, binding->device, binding->multicast_offset, binding->size) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "cuMulticastUnbind failed during prepare";
        return -1;
      }
      binding->checkpointed = true;
    }
    if (release(multicast->driver) != CUDA_SUCCESS) {
      (void)cuinterpose_leave_context(&scope);
      free(list.items);
      *error = "cuMemRelease of a multicast object failed during prepare";
      return -1;
    }
    multicast->driver = 0;
    multicast->checkpointed = true;
    if (cuinterpose_leave_context(&scope) != 0) {
      free(list.items);
      *error = "cannot leave the multicast object's CUDA context";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* RESTORE_MULTICAST_CREATORS: creators make the object again and re-export it
 * into the cache; importers ask for it in the next phase. */
int
cuinterpose_multicast_restore_creators(const char** error)
{
  create_fn create = (create_fn)cuinterpose_lookup_real_symbol("cuMulticastCreate");
  export_fn export_handle = (export_fn)cuinterpose_lookup_real_symbol("cuMemExportToShareableHandle");
  struct object_list list;
  size_t index;

  if (create == NULL || export_handle == NULL) {
    *error = "multicast restore symbols are unavailable";
    return -1;
  }
  if (list_objects(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct multicast* multicast = list.items[index];
    struct cuinterpose_context_scope scope;
    CUmemGenericAllocationHandle driver = 0;
    int real_fd = -1;

    if (!multicast->checkpointed || !multicast->creator)
      continue;
    if (enter(multicast, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the multicast object's CUDA context";
      return -1;
    }
    if (create(&driver, &multicast->properties) != CUDA_SUCCESS) {
      (void)cuinterpose_leave_context(&scope);
      free(list.items);
      *error = "cuMulticastCreate failed during restore";
      return -1;
    }
    multicast->driver = driver;
    if (multicast->shared &&
        (export_handle(&real_fd, driver, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0) != CUDA_SUCCESS ||
         cuinterpose_export_cache_put(multicast->id, multicast->authorization, real_fd) != 0)) {
      if (real_fd >= 0)
        close(real_fd);
      (void)cuinterpose_leave_context(&scope);
      free(list.items);
      *error = "cannot export a multicast object again after restore";
      return -1;
    }
    if (cuinterpose_leave_context(&scope) != 0) {
      free(list.items);
      *error = "cannot leave the multicast object's CUDA context";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* RESTORE_MULTICAST_IMPORTERS: importers fetch the object from its creator. */
int
cuinterpose_multicast_restore_importers(char* message, size_t message_size)
{
  import_fn import_handle = (import_fn)cuinterpose_lookup_real_symbol("cuMemImportFromShareableHandle");
  struct object_list list;
  size_t index;

  if (import_handle == NULL) {
    snprintf(message, message_size, "%s", "cuMemImportFromShareableHandle is unavailable");
    return -1;
  }
  if (list_objects(&list) != 0) {
    snprintf(message, message_size, "%s", "cannot allocate");
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct multicast* multicast = list.items[index];
    struct cuinterpose_posix_ticket ticket;
    struct cuinterpose_context_scope scope;
    char export_error[96] = {0};
    CUmemGenericAllocationHandle driver = 0;
    int raw_fd = -1;
    CUresult result;

    if (!multicast->checkpointed || multicast->creator)
      continue;
    fill_ticket(multicast, &ticket);
    /* The creator's listener serves this from its export cache without any
     * lock we hold, so the request can be made under state_lock. */
    if (cuinterpose_posix_request_export(&ticket, &raw_fd, export_error, sizeof(export_error)) != 0) {
      free(list.items);
      snprintf(message, message_size, "multicast creator export: %.70s", export_error);
      return -1;
    }
    if (enter(multicast, &scope) != 0) {
      close(raw_fd);
      free(list.items);
      snprintf(message, message_size, "%s", "cannot enter the multicast object's CUDA context");
      return -1;
    }
    result = import_handle(&driver, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
    close(raw_fd);
    if (result != CUDA_SUCCESS) {
      (void)cuinterpose_leave_context(&scope);
      free(list.items);
      snprintf(message, message_size, "multicast import failed during restore: CUresult=%d", (int)result);
      return -1;
    }
    multicast->driver = driver;
    if (cuinterpose_leave_context(&scope) != 0) {
      free(list.items);
      snprintf(message, message_size, "%s", "cannot leave the multicast object's CUDA context");
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* RESTORE_MULTICAST_DEVICES: every participant attaches the devices it had
 * attached before. Binding in the next phase waits for all of them, which is
 * why the coordinator puts a barrier between the two. */
int
cuinterpose_multicast_restore_devices(const char** error)
{
  add_device_fn add_device = (add_device_fn)cuinterpose_lookup_real_symbol("cuMulticastAddDevice");
  struct object_list list;
  size_t index;

  if (add_device == NULL) {
    *error = "cuMulticastAddDevice is unavailable";
    return -1;
  }
  if (list_objects(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct multicast* multicast = list.items[index];
    struct cuinterpose_context_scope scope;
    size_t item;

    if (!multicast->checkpointed)
      continue;
    if (multicast->driver == 0) {
      free(list.items);
      *error = "a multicast object was not recreated before its devices";
      return -1;
    }
    if (enter(multicast, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the multicast object's CUDA context";
      return -1;
    }
    for (item = 0; item < multicast->device_count; item++) {
      if (add_device(multicast->driver, multicast->devices[item]) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "cuMulticastAddDevice failed during restore";
        return -1;
      }
    }
    if (cuinterpose_leave_context(&scope) != 0) {
      free(list.items);
      *error = "cannot leave the multicast object's CUDA context";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* RESTORE_MULTICAST: bind the members again, map the object at the old
 * addresses with the old access, and check nothing is left checkpointed. */
int
cuinterpose_multicast_restore_topology(const char** error)
{
  map_fn map = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  access_fn set_access = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  struct object_list list;
  size_t index;

  if (map == NULL || set_access == NULL) {
    *error = "multicast mapping symbols are unavailable";
    return -1;
  }
  if (list_objects(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct multicast* multicast = list.items[index];
    struct cuinterpose_context_scope scope;
    size_t item;

    if (!multicast->checkpointed)
      continue;
    if (multicast->driver == 0) {
      free(list.items);
      *error = "a multicast object was not recreated before its bindings";
      return -1;
    }
    if (enter(multicast, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the multicast object's CUDA context";
      return -1;
    }
    for (item = 0; item < multicast->binding_count; item++) {
      struct multicast_binding* binding = &multicast->bindings[item];

      if (!binding->checkpointed)
        continue;
      if (replay_binding(multicast->driver, binding, error) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        return -1;
      }
      binding->checkpointed = false;
    }
    for (item = 0; item < multicast->mapping_count; item++) {
      struct multicast_mapping* mapping = multicast->mappings[item];

      if (!mapping->checkpointed)
        continue;
      if (map(mapping->address, mapping->size, mapping->offset, multicast->driver, mapping->flags) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "cuMemMap of a multicast mapping failed during restore";
        return -1;
      }
      if (mapping->access_count != 0 &&
          set_access(mapping->address, mapping->size, mapping->access, mapping->access_count) != CUDA_SUCCESS) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "cuMemSetAccess of a multicast mapping failed during restore";
        return -1;
      }
      if (cuinterpose_ranges_insert(&mapped, (uint64_t)mapping->address, (uint64_t)mapping->address + mapping->size,
                                    mapping) != 0) {
        (void)cuinterpose_leave_context(&scope);
        free(list.items);
        *error = "a restored multicast mapping overlaps another mapping";
        return -1;
      }
      mapping->mapped = true;
      mapping->checkpointed = false;
    }
    multicast->checkpointed = false;
    if (cuinterpose_leave_context(&scope) != 0) {
      free(list.items);
      *error = "cannot leave the multicast object's CUDA context";
      return -1;
    }
  }
  for (index = 0; index < list.count; index++) {
    if (list.items[index]->checkpointed || (active(list.items[index]) && list.items[index]->driver == 0)) {
      free(list.items);
      *error = "a multicast object remains checkpointed after restore";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* ------------------------------------------------------------------------- */
/* The cuMulticast* entry points                                             */
/* ------------------------------------------------------------------------- */

/* Refuses a raw driver handle that happens to fall in the shim's logical range. */
static CUresult
passthrough_handle(CUresult result, CUmemGenericAllocationHandle driver, CUmemGenericAllocationHandle* output)
{
  if (result != CUDA_SUCCESS) {
    *output = driver;
    return result;
  }
  if (cuinterpose_is_logical_handle(driver)) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");

    if (release != NULL)
      (void)release(driver);
    fprintf(stderr, "cuinterpose: driver returned a multicast handle in the reserved logical range; refusing it\n");
    return CUDA_ERROR_INVALID_HANDLE;
  }
  *output = driver;
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  create_fn function = (create_fn)cuinterpose_lookup_real_symbol("cuMulticastCreate");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct multicast* multicast;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  if (properties == NULL || properties->handleTypes != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) {
    /* Same rule as cuMemCreate: only the POSIX descriptor handle type is tracked. */
    if (properties != NULL && (properties->handleTypes & CU_MEM_HANDLE_TYPE_FABRIC) != 0 &&
        !atomic_exchange(&fabric_logged, true))
      fprintf(stderr, "cuinterpose: a multicast object with the FABRIC handle type passed through untracked; "
                      "it will not survive checkpoint/restore\n");
    result = function(&driver, properties);
    return passthrough_handle(result, driver, output);
  }
  /*
   * cuMulticastCreate is a team collective that can wait for the other ranks
   * (and contends the driver's global lock); nothing of ours needs protecting
   * until it returns, so the lock is taken only afterwards.
   */
  result = function(&driver, properties);
  if (result != CUDA_SUCCESS) {
    *output = driver; /* whatever the driver wrote, as if it had been called directly */
    return result;
  }
  multicast = calloc(1, sizeof(*multicast));
  ops.acquire_state_lock();
  if (!ops.state_is_active()) {
    ops.release_state_lock();
    if (release != NULL)
      (void)release(driver);
    free(multicast);
    return CUDA_ERROR_NOT_READY;
  }
  if (multicast == NULL || cuinterpose_random_bytes(multicast->id, sizeof(multicast->id)) != 0 ||
      cuinterpose_random_bytes(multicast->authorization, sizeof(multicast->authorization)) != 0 ||
      cuinterpose_table_put(&objects, cuinterpose_key_bytes(multicast->id), multicast) != 0) {
    ops.release_state_lock();
    if (release != NULL)
      (void)release(driver);
    free(multicast);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  multicast->properties = *properties;
  multicast->effective_size = properties->size;
  multicast->driver = driver;
  multicast->creator = true;
  cuinterpose_capture_context(&multicast->context);
  snprintf(multicast->creator_participant, sizeof(multicast->creator_participant), "%s", participant_id);
  snprintf(multicast->creator_endpoint, sizeof(multicast->creator_endpoint), "%s", endpoint_path);
  if (add_handle(multicast, &logical) != 0) {
    cuinterpose_table_remove(&objects, cuinterpose_key_bytes(multicast->id));
    ops.release_state_lock();
    if (release != NULL)
      (void)release(driver);
    free(multicast);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  ops.release_state_lock();
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle application, CUdevice device)
{
  add_device_fn function = (add_device_fn)cuinterpose_lookup_real_symbol("cuMulticastAddDevice");
  struct multicast_handle* handle;
  struct multicast* multicast;
  CUmemGenericAllocationHandle driver;
  CUdevice* slot;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  ops.acquire_state_lock();
  handle = find_handle(application);
  if (handle == NULL) {
    ops.release_state_lock();
    if (cuinterpose_is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function(application, device);
  }
  if (!ops.state_is_active() || handle->multicast->driver == 0) {
    ops.release_state_lock();
    return CUDA_ERROR_NOT_READY;
  }
  /* Another team collective: not under the lock. */
  driver = handle->multicast->driver;
  ops.release_state_lock();
  result = function(driver, device);
  ops.acquire_state_lock();
  if (result != CUDA_SUCCESS) {
    ops.release_state_lock();
    return result;
  }
  handle = find_handle(application);
  if (handle == NULL || handle->multicast->driver != driver || !ops.state_is_active()) {
    /* The object went away or a checkpoint began meanwhile; the driver keeps
     * the device attached, the record cannot. */
    ops.release_state_lock();
    return handle == NULL ? CUDA_ERROR_INVALID_HANDLE : CUDA_ERROR_NOT_READY;
  }
  multicast = handle->multicast;
  slot = push((void**)&multicast->devices, &multicast->device_count, sizeof(*multicast->devices));
  if (slot == NULL) {
    ops.release_state_lock();
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *slot = device;
  adopt_context(multicast);
  ops.release_state_lock();
  return CUDA_SUCCESS;
}

/* The two bind entry points share everything but how the member is named. */
static CUresult
bind(
    CUmemGenericAllocationHandle application, CUdevice device, bool device_explicit, size_t multicast_offset,
    CUmemGenericAllocationHandle member_handle, CUdeviceptr member_address, size_t member_offset, size_t size,
    unsigned long long flags, uint8_t kind)
{
  struct multicast_handle* handle;
  struct multicast* multicast;
  struct multicast_binding binding;
  struct multicast_binding* slot;
  struct cuinterpose_multicast_member member;
  CUmemGenericAllocationHandle driver;
  bool tracked_member;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  ops.acquire_state_lock();
  handle = find_handle(application);
  if (handle == NULL) {
    ops.release_state_lock();
    if (cuinterpose_is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    /* An untracked object; a tracked member still needs its driver handle. */
    if (kind == CUINTERPOSE_MULTICAST_BIND_MEM) {
      CUmemGenericAllocationHandle member_driver = member_handle;

      (void)cuinterpose_translate_handle(member_handle, &member_driver);
      return forward_bind_mem(application, device, device_explicit, multicast_offset, member_driver, member_offset, size, flags);
    }
    return forward_bind_address(application, device, device_explicit, multicast_offset, member_address, size, flags);
  }
  multicast = handle->multicast;
  if (!ops.state_is_active() || multicast->driver == 0) {
    ops.release_state_lock();
    return CUDA_ERROR_NOT_READY;
  }
  memset(&member, 0, sizeof(member));
  memset(&binding, 0, sizeof(binding));
  if (kind == CUINTERPOSE_MULTICAST_BIND_MEM) {
    if (ops.member_from_handle(member_handle, &member) != 0) {
      /* Memory the shim does not track cannot be bound again on restore. */
      ops.release_state_lock();
      if (!atomic_exchange(&untracked_member_logged, true))
        fprintf(stderr, "cuinterpose: refusing to bind memory the shim does not track to a tracked multicast object\n");
      return CUDA_ERROR_NOT_SUPPORTED;
    }
    tracked_member = true;
  } else {
    tracked_member = ops.member_from_address(member_address, size, &member) == 0;
    if (!tracked_member) {
      /* Memory outside the shim's records is rebound by address on restore;
       * its address is preserved by the native restore. */
      context_device_fn current_device = (context_device_fn)cuinterpose_lookup_real_symbol("cuCtxGetDevice");

      if (cuinterpose_random_bytes(member.id, sizeof(member.id)) != 0 ||
          (!device_explicit && (current_device == NULL || current_device(&device) != CUDA_SUCCESS))) {
        ops.release_state_lock();
        return CUDA_ERROR_NOT_SUPPORTED;
      }
      member.device = device;
    }
    member.address = member_address;
  }
  memcpy(binding.member_id, member.id, sizeof(binding.member_id));
  binding.member_address = kind == CUINTERPOSE_MULTICAST_BIND_ADDR ? member_address : 0;
  binding.multicast_offset = multicast_offset;
  binding.member_offset = kind == CUINTERPOSE_MULTICAST_BIND_MEM ? member_offset : member.allocation_offset;
  binding.size = size;
  binding.flags = flags;
  binding.device = device_explicit ? device : member.device;
  binding.kind = kind;
  binding.api_version = device_explicit ? 2 : 1;
  /*
   * Binding waits until every device of the team has been attached, so it
   * runs without the lock; see cuinterpose_multicast_map.
   */
  driver = multicast->driver;
  ops.release_state_lock();
  if (kind == CUINTERPOSE_MULTICAST_BIND_MEM)
    result = forward_bind_mem(driver, binding.device, device_explicit, multicast_offset, member.handle, member_offset, size, flags);
  else
    result = forward_bind_address(driver, binding.device, device_explicit, multicast_offset, member_address, size, flags);
  ops.acquire_state_lock();
  if (result != CUDA_SUCCESS) {
    ops.release_state_lock();
    return result;
  }
  handle = find_handle(application);
  if (handle == NULL || handle->multicast->driver != driver || !ops.state_is_active()) {
    if (handle != NULL) {
      unbind_fn unbind = (unbind_fn)cuinterpose_lookup_real_symbol("cuMulticastUnbind");

      if (unbind != NULL)
        (void)unbind(driver, binding.device, multicast_offset, size);
    }
    ops.release_state_lock();
    return handle == NULL ? CUDA_ERROR_INVALID_HANDLE : CUDA_ERROR_NOT_READY;
  }
  multicast = handle->multicast;
  slot = push((void**)&multicast->bindings, &multicast->binding_count, sizeof(*multicast->bindings));
  if (slot == NULL) {
    unbind_fn unbind = (unbind_fn)cuinterpose_lookup_real_symbol("cuMulticastUnbind");

    if (unbind != NULL)
      (void)unbind(driver, binding.device, multicast_offset, size);
    ops.release_state_lock();
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *slot = binding;
  if (tracked_member)
    ops.mark_member_shared(member.id);
  observe_extent(multicast, multicast_offset, size);
  adopt_context(multicast);
  ops.release_state_lock();
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  return bind(multicast, 0, false, multicast_offset, memory, 0, memory_offset, size, flags, CUINTERPOSE_MULTICAST_BIND_MEM);
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return bind(multicast, 0, false, multicast_offset, 0, memory, 0, size, flags, CUINTERPOSE_MULTICAST_BIND_ADDR);
}

#if CUDA_VERSION >= 13010
CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  return bind(multicast, device, true, multicast_offset, memory, 0, memory_offset, size, flags, CUINTERPOSE_MULTICAST_BIND_MEM);
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return bind(multicast, device, true, multicast_offset, 0, memory, 0, size, flags, CUINTERPOSE_MULTICAST_BIND_ADDR);
}
#endif

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  granularity_fn function = (granularity_fn)cuinterpose_lookup_real_symbol("cuMulticastGetGranularity");

  return function != NULL ? function(granularity, properties, option) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastUnbind(CUmemGenericAllocationHandle application, CUdevice device, size_t offset, size_t size)
{
  unbind_fn function = (unbind_fn)cuinterpose_lookup_real_symbol("cuMulticastUnbind");
  struct multicast_handle* handle;
  struct multicast* multicast;
  size_t index;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  ops.acquire_state_lock();
  handle = find_handle(application);
  if (handle == NULL) {
    ops.release_state_lock();
    if (cuinterpose_is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function(application, device, offset, size);
  }
  multicast = handle->multicast;
  if (!ops.state_is_active() || multicast->driver == 0) {
    ops.release_state_lock();
    return CUDA_ERROR_NOT_READY;
  }
  /* Unbind is not a collective: it runs under the lock. */
  result = function(multicast->driver, device, offset, size);
  if (result == CUDA_SUCCESS) {
    for (index = 0; index < multicast->binding_count; index++) {
      struct multicast_binding* binding = &multicast->bindings[index];

      if (binding->device == device && binding->multicast_offset == offset && binding->size == size) {
        multicast->bindings[index] = multicast->bindings[multicast->binding_count - 1];
        multicast->binding_count--;
        break;
      }
    }
  }
  ops.release_state_lock();
  return result;
}
