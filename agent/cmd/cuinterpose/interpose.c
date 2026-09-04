/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Tracking of CUDA virtual-memory-management (VMM) allocations that are
 * shared between processes.
 *
 * The application sees "logical handles": 64-bit values with a fixed tag in
 * the top 16 bits that the shim mints. Behind each logical handle is a tracked
 * allocation and, per process, exactly one real driver handle. Repeated
 * imports of the same allocation, or cuMemRetainAllocationHandle on one of its
 * mappings, produce new logical handles that share that one driver handle; the
 * driver handle is released when the last logical handle goes and the
 * allocation record is freed when no handle and no mapping remains.
 *
 * Sharing works through tickets (posix.c): an export hands the application a
 * sealed memfd instead of the driver's descriptor, and an import of a ticket
 * asks the creator process for the real descriptor over its control socket.
 * The creator answers from its export cache (export_cache.c) without touching
 * the driver.
 *
 * Everything the application touches is protected by state_lock. The listener
 * thread that serves peers takes only the export cache's lock.
 */

#define _GNU_SOURCE

#include "interpose.h"

#include <errno.h>
#include <dirent.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include "export.h"
#include "context.h"
#include "export_cache.h"
#include "posix.h"
#include "protocol.h"
#include "symbols.h"
#include "table.h"
#include "util.h"

#define LOGICAL_HANDLE_TAG UINT64_C(0xd94d000000000000)
#define LOGICAL_HANDLE_TAG_MASK UINT64_C(0xffff000000000000)
#define LOGICAL_HANDLE_VALUE_MASK UINT64_C(0x0000ffffffffffff)

CUINTERPOSE_API const struct cuinterpose_build_info cuinterpose_build_info = {
    .cuda_version = CUDA_VERSION,
    .protocol_version = CUINTERPOSE_VERSION,
};

typedef CUresult(CUDAAPI* create_fn)(
    CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
typedef CUresult(CUDAAPI* release_fn)(CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* retain_fn)(CUmemGenericAllocationHandle*, void*);
typedef CUresult(CUDAAPI* map_fn)(CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
typedef CUresult(CUDAAPI* unmap_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* access_fn)(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
typedef CUresult(CUDAAPI* export_fn)(
    void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
typedef CUresult(CUDAAPI* import_fn)(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
typedef CUresult(CUDAAPI* properties_fn)(CUmemAllocationProp*, CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* address_reserve_fn)(CUdeviceptr*, size_t, size_t, CUdeviceptr, unsigned long long);
typedef CUresult(CUDAAPI* address_free_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* host_register_fn)(void*, size_t, unsigned int);
typedef CUresult(CUDAAPI* host_unregister_fn)(void*);
typedef CUresult(CUDAAPI* host_get_flags_fn)(unsigned int*, void*);
typedef CUresult(CUDAAPI* copy_dtoh_async_fn)(void*, CUdeviceptr, size_t, CUstream);
typedef CUresult(CUDAAPI* copy_htod_async_fn)(CUdeviceptr, const void*, size_t, CUstream);
typedef CUresult(CUDAAPI* stream_create_fn)(CUstream*, unsigned int);
typedef CUresult(CUDAAPI* stream_synchronize_fn)(CUstream);
typedef CUresult(CUDAAPI* stream_destroy_fn)(CUstream);

struct allocation {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  uint8_t authorization[CUINTERPOSE_TOKEN_SIZE];
  char creator_participant[CUINTERPOSE_ID_SIZE];
  char creator_endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
  size_t size; /* known to the creator; importers learn it only from the coordinator */
  CUmemAllocationProp properties;
  CUcontext context; /* the CUDA context the allocation was created or imported in */
  CUmemGenericAllocationHandle driver; /* the one backing driver handle, 0 when released */
  bool creator;
  bool shared; /* a ticket was issued (creator) or imported (importer) */
  unsigned live_handles;
  unsigned live_mappings;
  /* Checkpoint state. */
  bool checkpointed; /* PREPARE ran: mappings are unmapped and driver handles released */
  void* host_carrier; /* creator: pinned host copy of the contents while checkpointed */
  bool host_checkpointed; /* creator: the device backing was freed after copying to host */
};

struct handle {
  CUmemGenericAllocationHandle logical;
  struct allocation* allocation;
};

struct mapping {
  CUdeviceptr address;
  size_t size;
  size_t offset;
  struct allocation* allocation;
  CUmemAccessDesc access[CUINTERPOSE_MAX_ACCESS];
  size_t access_count;
  bool access_unknown; /* a driver access call failed part-way; replay cannot be trusted */
  bool checkpointed; /* unmapped for the checkpoint; to be mapped again on restore */
};

/*
 * Where this process is in the checkpoint/restore lifecycle. The coordinator
 * drives the transitions; while not ACTIVE every tracked entry point answers
 * CUDA_ERROR_NOT_READY, which is safe only because the workload is parked
 * (see docs/reference/cuinterpose.md, "Quiescence").
 */
enum phase {
  PHASE_ACTIVE,
  PHASE_CARRIERS, /* PREPARE_MULTICAST done: every creator allocation has a carrier handle */
  PHASE_PREPARED, /* PREPARE done: nothing shared is mapped or held */
  PHASE_CREATORS_RESTORED,
  PHASE_UNICAST_RESTORED,
  PHASE_MULTICAST_CREATED,
  PHASE_MULTICAST_IMPORTED,
  PHASE_MULTICAST_JOINED,
  PHASE_FAILED,
};

static pthread_mutex_t state_lock = PTHREAD_MUTEX_INITIALIZER;
static struct cuinterpose_table allocations; /* allocation id -> struct allocation* */
static struct cuinterpose_table handles; /* logical handle -> struct handle* */
static struct cuinterpose_ranges mappings; /* address range -> struct mapping* */
static struct cuinterpose_table raw_imports; /* driver handle of an untracked import -> marker */
static enum phase current_phase = PHASE_ACTIVE;
static char failure[96];
static char participant_id[CUINTERPOSE_ID_SIZE];
static char control_directory[sizeof(((struct sockaddr_un*)0)->sun_path)];
static char socket_path[sizeof(((struct sockaddr_un*)0)->sun_path)];
static int listener = -1;
static bool endpoint_needs_initialization;
static uint64_t next_logical_handle = 1;
static _Atomic uint32_t live_raw_imports;
static _Atomic uint32_t passthrough_creations;
static _Atomic bool fabric_passthrough_logged;

static uint8_t phase_code(void);
static int request_export(const struct cuinterpose_posix_ticket* ticket, int* output, char* error, size_t error_size);

static void
set_failure(const char* message)
{
  current_phase = PHASE_FAILED;
  snprintf(failure, sizeof(failure), "%s", message);
}

static void
warn(const char* message)
{
  fprintf(stderr, "cuinterpose: %s\n", message);
}

/* ------------------------------------------------------------------------- */
/* Bookkeeping helpers. Caller holds state_lock.                              */
/* ------------------------------------------------------------------------- */

static bool
is_logical_handle(CUmemGenericAllocationHandle handle)
{
  return ((uint64_t)handle & LOGICAL_HANDLE_TAG_MASK) == LOGICAL_HANDLE_TAG;
}

static struct handle*
find_handle(CUmemGenericAllocationHandle logical)
{
  if (!is_logical_handle(logical))
    return NULL;
  return cuinterpose_table_get(&handles, cuinterpose_key_u64((uint64_t)logical));
}

static struct allocation*
find_allocation(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  return cuinterpose_table_get(&allocations, cuinterpose_key_bytes(id));
}

static struct mapping*
mapping_at(CUdeviceptr address)
{
  struct cuinterpose_range* range = cuinterpose_ranges_at(&mappings, (uint64_t)address);

  return range == NULL ? NULL : range->value;
}

/* Mint a logical handle for allocation. Returns 0 and sets *logical. */
static int
add_handle(struct allocation* allocation, CUmemGenericAllocationHandle* logical)
{
  struct handle* handle;

  if (next_logical_handle > LOGICAL_HANDLE_VALUE_MASK)
    return -1;
  handle = calloc(1, sizeof(*handle));
  if (handle == NULL)
    return -1;
  handle->logical = (CUmemGenericAllocationHandle)(LOGICAL_HANDLE_TAG | next_logical_handle);
  handle->allocation = allocation;
  if (cuinterpose_table_put(&handles, cuinterpose_key_u64((uint64_t)handle->logical), handle) != 0) {
    free(handle);
    return -1;
  }
  next_logical_handle++;
  allocation->live_handles++;
  *logical = handle->logical;
  return 0;
}

/*
 * Called when an allocation lost a handle or a mapping. Releases the driver
 * handle once no logical handle refers to it (the driver keeps the memory
 * alive for any remaining mappings, as it would for the application), and
 * frees the record once nothing refers to it at all.
 */
static CUresult
settle_allocation(struct allocation* allocation)
{
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  CUresult result = CUDA_SUCCESS;

  if (allocation->live_handles == 0 && allocation->driver != 0) {
    result = release != NULL ? release(allocation->driver) : cuinterpose_unavailable();
    allocation->driver = 0;
  }
  if (allocation->live_handles == 0 && allocation->live_mappings == 0 && !allocation->checkpointed) {
    /* No local reference is left, so any ticket for this allocation is dead. */
    cuinterpose_export_cache_drop(allocation->id);
    cuinterpose_table_remove(&allocations, cuinterpose_key_bytes(allocation->id));
    free(allocation);
  }
  return result;
}

/*
 * The driver does not need a context for cuMemCreate or
 * cuMemImportFromShareableHandle, so neither may the shim: an allocation
 * without a context adopts the one current at its first map or export, and if
 * it never gets one the lifecycle code falls back to the primary context of
 * the allocation's device (context.c).
 */
static void
adopt_context(struct allocation* allocation)
{
  if (allocation->context == NULL)
    cuinterpose_capture_context(&allocation->context);
}

/*
 * Returns a driver handle that is not a logical one, or releases it and fails:
 * the tag is reserved for the shim, and a real handle in that range would be
 * indistinguishable from a logical one.
 */
static CUresult
passthrough_handle(CUresult result, CUmemGenericAllocationHandle driver, CUmemGenericAllocationHandle* output)
{
  if (result != CUDA_SUCCESS)
    return result;
  if (is_logical_handle(driver)) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(driver);
    warn("driver returned a handle in the reserved logical range; refusing it");
    return CUDA_ERROR_INVALID_HANDLE;
  }
  *output = driver;
  return CUDA_SUCCESS;
}

bool
cuinterpose_translate_handle(CUmemGenericAllocationHandle logical, CUmemGenericAllocationHandle* driver)
{
  struct handle* handle;
  bool found = false;

  if (!is_logical_handle(logical))
    return false;
  pthread_mutex_lock(&state_lock);
  handle = find_handle(logical);
  if (handle != NULL && handle->allocation->driver != 0) {
    *driver = handle->allocation->driver;
    found = true;
  }
  pthread_mutex_unlock(&state_lock);
  return found;
}

/* ------------------------------------------------------------------------- */
/* Debug statistics for tests and operators.                                  */
/* ------------------------------------------------------------------------- */

CUINTERPOSE_API void
cuinterpose_debug_stats(struct cuinterpose_debug_stats* stats)
{
  memset(stats, 0, sizeof(*stats));
  pthread_mutex_lock(&state_lock);
  stats->allocations = allocations.count;
  stats->handles = handles.count;
  stats->mappings = mappings.count;
  stats->live_raw_imports = atomic_load(&live_raw_imports);
  stats->passthrough_creations = atomic_load(&passthrough_creations);
  stats->phase = phase_code();
  pthread_mutex_unlock(&state_lock);
  stats->cached_exports = cuinterpose_export_cache_count();
}

/* ------------------------------------------------------------------------- */
/* Control socket: the listener thread and the requests it serves.            */
/* ------------------------------------------------------------------------- */

static uint8_t
phase_code(void)
{
  switch (current_phase) {
    case PHASE_ACTIVE:
      return CUINTERPOSE_PHASE_ACTIVE;
    case PHASE_CARRIERS:
      return CUINTERPOSE_PHASE_PREPARING;
    case PHASE_PREPARED:
      return CUINTERPOSE_PHASE_PREPARED;
    case PHASE_FAILED:
      return CUINTERPOSE_PHASE_FAILED;
    default:
      return CUINTERPOSE_PHASE_RESTORING;
  }
}


/* ------------------------------------------------------------------------- */
/* Checkpoint and restore of tracked allocations. Caller holds state_lock.    */
/*                                                                            */
/* The sequence, driven by the coordinator across every process at once:      */
/*   PREPARE_MULTICAST  give every creator allocation a "carrier" handle       */
/*   SAVE_HOST_CARRIER  copy each creator allocation to pinned host memory     */
/*   PREPARE            unmap everything shared, release every driver handle  */
/*   (native CUDA checkpoint, CRIU dump; later CRIU restore, native restore)  */
/*   RESTORE_HOST_CARRIER  new device memory, copy the bytes back             */
/*   RESTORE_CREATORS   map creators' memory back where it was, re-export     */
/*   RESTORE_IMPORTERS  fetch fresh descriptors from creators, import, remap  */
/*   RESTORE_MULTICAST* (no multicast state in this layer)                    */
/* ------------------------------------------------------------------------- */

static int
enter_allocation_context(const struct allocation* allocation, struct cuinterpose_context_scope* scope)
{
  CUdevice fallback = allocation->properties.location.type == CU_MEM_LOCATION_TYPE_DEVICE
                          ? (CUdevice)allocation->properties.location.id
                          : CUINTERPOSE_NO_DEVICE;

  return cuinterpose_enter_context(allocation->context, fallback, scope);
}

static int
leave_context(struct cuinterpose_context_scope* scope)
{
  return cuinterpose_leave_context(scope);
}

struct allocation_list {
  struct allocation** items;
  size_t count;
};

static int
collect_allocation(struct cuinterpose_key key, void* value, void* arg)
{
  struct allocation_list* list = arg;

  (void)key;
  list->items[list->count++] = value;
  return 0;
}

/* Snapshot of the allocation table as an array, so callers can mutate the
 * table while walking. Returns -1 on allocation failure. */
static int
list_allocations(struct allocation_list* list)
{
  list->count = 0;
  list->items = calloc(allocations.count == 0 ? 1 : allocations.count, sizeof(*list->items));
  if (list->items == NULL)
    return -1;
  cuinterpose_table_each(&allocations, collect_allocation, list);
  return 0;
}

/* Every tracked creator allocation travels through a host carrier. */
static bool
needs_host_carrier(const struct allocation* allocation)
{
  return allocation->creator && allocation->properties.type == CU_MEM_ALLOCATION_TYPE_PINNED &&
         allocation->properties.location.type == CU_MEM_LOCATION_TYPE_DEVICE;
}

static struct cuinterpose_record*
inspect_records(uint32_t* count, const char** error)
{
  struct cuinterpose_record* records;
  struct cuinterpose_record* record;
  struct allocation_list list;
  size_t total;
  size_t index;

  *error = NULL;
  total = allocations.count + mappings.count;
  if (total > CUINTERPOSE_MAX_RECORDS) {
    *error = "too many tracked records for one participant";
    return NULL;
  }
  if (list_allocations(&list) != 0) {
    *error = "cannot allocate inspect records";
    return NULL;
  }
  records = calloc(total == 0 ? 1 : total, sizeof(*records));
  if (records == NULL) {
    free(list.items);
    *error = "cannot allocate inspect records";
    return NULL;
  }
  record = records;
  for (index = 0; index < list.count; index++) {
    const struct allocation* allocation = list.items[index];

    record->kind = CUINTERPOSE_ALLOCATION;
    if (allocation->creator)
      record->flags |= CUINTERPOSE_CREATOR;
    if (allocation->live_handles != 0)
      record->flags |= CUINTERPOSE_APPLICATION_HANDLE_LIVE;
    if (needs_host_carrier(allocation))
      record->flags |= CUINTERPOSE_HOST_CARRIER;
    if (allocation->creator && !allocation->shared)
      record->flags |= CUINTERPOSE_CARRIER_ONLY;
    memcpy(record->allocation_id, allocation->id, sizeof(record->allocation_id));
    record->allocation_size = allocation->size;
    record->allocation_type = allocation->properties.type;
    record->requested_handle_types = allocation->properties.requestedHandleTypes;
    record->allocation_location_type = allocation->properties.location.type;
    record->allocation_location_id = allocation->properties.location.id;
    record->application_handle_count = allocation->live_handles;
    record++;
  }
  free(list.items);
  for (index = 0; index < mappings.count; index++) {
    const struct mapping* mapping = mappings.items[index].value;
    size_t access;

    if (mapping->access_unknown) {
      free(records);
      *error = "a mapping's access state is unknown after a failed cuMemSetAccess";
      return NULL;
    }
    record->kind = CUINTERPOSE_MAPPING;
    record->flags = mapping->allocation->creator ? CUINTERPOSE_CREATOR : 0;
    memcpy(record->allocation_id, mapping->allocation->id, sizeof(record->allocation_id));
    record->address = mapping->address;
    record->size = mapping->size;
    record->offset = mapping->offset;
    record->access_count = (uint32_t)mapping->access_count;
    for (access = 0; access < mapping->access_count; access++) {
      record->access[access].location_type = mapping->access[access].location.type;
      record->access[access].location_id = mapping->access[access].location.id;
      record->access[access].flags = mapping->access[access].flags;
    }
    record++;
  }
  *count = (uint32_t)total;
  return records;
}

/*
 * PREPARE_MULTICAST, unicast part: make sure every creator allocation has a
 * driver handle to work with. A creator that released its handles but still
 * has a mapping gets one back through cuMemRetainAllocationHandle.
 */
static int
create_carriers(const char** error)
{
  retain_fn retain = (retain_fn)cuinterpose_lookup_real_symbol("cuMemRetainAllocationHandle");
  struct allocation_list list;
  size_t index;

  if (retain == NULL) {
    *error = "cuMemRetainAllocationHandle is unavailable";
    return -1;
  }
  if (list_allocations(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct allocation* allocation = list.items[index];
    struct cuinterpose_context_scope scope;
    size_t range;
    CUresult result = CUDA_ERROR_INVALID_VALUE;

    if (!allocation->creator || allocation->driver != 0)
      continue;
    if (enter_allocation_context(allocation, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the allocation's CUDA context";
      return -1;
    }
    for (range = 0; range < mappings.count; range++) {
      const struct mapping* mapping = mappings.items[range].value;
      if (mapping->allocation == allocation) {
        result = retain(&allocation->driver, (void*)(uintptr_t)mapping->address);
        break;
      }
    }
    if (leave_context(&scope) != 0 || result != CUDA_SUCCESS) {
      free(list.items);
      *error = "cannot retain a carrier handle for a creator allocation";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* ------------------------------------------------------------------------- */
/* Host carriers                                                              */
/* ------------------------------------------------------------------------- */

/*
 * Every creator allocation's contents cross to the host before the native
 * checkpoint and back after the native restore. Why: the r615 driver attaches
 * fabric (FLA) state to device allocations at creation and does not rebuild
 * it on restore, so an allocation restored natively cannot be exported again;
 * creating a fresh allocation on restore and copying the bytes back avoids
 * that.
 *
 * The host side is one pinned arena per process: a single anonymous mapping,
 * pinned with one cuMemHostRegister, that CRIU saves as ordinary process
 * memory. The device side is one staging range per CUDA context with every
 * allocation mapped at consecutive offsets. One registration and one range
 * instead of one per allocation matters: vLLM's allocator hands out hundreds
 * of 2 MiB pieces, and pinning, mapping, and unpinning each one separately
 * costs more than the copies. All copies are issued asynchronously on one
 * stream per context and waited for once, so the transfer runs at the link's
 * speed; the copy time is reported apart from the phase as a whole, and the
 * arena is unpinned only after the restore reply has gone out.
 */
struct carrier_arena {
  void* base;
  size_t size;
  CUcontext context; /* where it was registered; the deferred unregister enters it again */
  CUdevice device; /* fallback when that context is NULL */
};

static struct carrier_arena arena;

struct staging_range {
  CUdeviceptr base;
  size_t size;
};

static double
elapsed_milliseconds(const struct timespec* start, const struct timespec* end)
{
  return (double)(end->tv_sec - start->tv_sec) * 1e3 + (double)(end->tv_nsec - start->tv_nsec) / 1e6;
}

static int
compare_by_context(const void* left, const void* right)
{
  uintptr_t a = (uintptr_t)(*(struct allocation* const*)left)->context;
  uintptr_t b = (uintptr_t)(*(struct allocation* const*)right)->context;

  return a < b ? -1 : a > b ? 1 : 0;
}

/* The allocations `wanted` selects, sorted by context so each context is entered once. */
static int
collect_carriers(bool (*wanted)(const struct allocation*), struct allocation_list* list, uint64_t* total)
{
  size_t kept = 0;
  size_t index;

  *total = 0;
  if (list_allocations(list) != 0)
    return -1;
  for (index = 0; index < list->count; index++) {
    if (wanted(list->items[index])) {
      list->items[kept++] = list->items[index];
      *total += list->items[index]->size;
    }
  }
  list->count = kept;
  qsort(list->items, list->count, sizeof(*list->items), compare_by_context);
  return 0;
}

static bool
wants_saving(const struct allocation* allocation)
{
  return needs_host_carrier(allocation) && allocation->host_carrier == NULL;
}

static bool
wants_restoring(const struct allocation* allocation)
{
  return allocation->host_checkpointed;
}

/* End of the run of allocations sharing list->items[begin]'s context; *bytes gets their sum. */
static size_t
context_batch_end(const struct allocation_list* list, size_t begin, uint64_t* bytes)
{
  size_t end = begin;

  *bytes = 0;
  while (end < list->count && list->items[end]->context == list->items[begin]->context) {
    *bytes += list->items[end]->size;
    end++;
  }
  return end;
}

/*
 * Reserves one range and maps handles[i] at consecutive offsets with read/write
 * access for each allocation's device. Nothing stays mapped on failure.
 */
static CUresult
map_staging_range(
    struct allocation* const* items, const CUmemGenericAllocationHandle* handles, size_t count, size_t total,
    struct staging_range* range)
{
  address_reserve_fn reserve = (address_reserve_fn)cuinterpose_lookup_real_symbol("cuMemAddressReserve");
  address_free_fn free_address = (address_free_fn)cuinterpose_lookup_real_symbol("cuMemAddressFree");
  map_fn map = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  access_fn set_access = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  size_t offset = 0;
  size_t mapped = 0;
  size_t index;
  CUresult result;

  range->base = 0;
  range->size = 0;
  if (reserve == NULL || free_address == NULL || map == NULL || unmap == NULL || set_access == NULL)
    return CUDA_ERROR_NOT_INITIALIZED;
  result = reserve(&range->base, total, 0, 0, 0);
  if (result != CUDA_SUCCESS) {
    range->base = 0;
    return result;
  }
  for (index = 0; index < count; index++) {
    CUmemAccessDesc access;

    result = map(range->base + offset, items[index]->size, 0, handles[index], 0);
    if (result != CUDA_SUCCESS)
      break;
    mapped = offset + items[index]->size;
    memset(&access, 0, sizeof(access));
    access.location = items[index]->properties.location;
    access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    result = set_access(range->base + offset, items[index]->size, &access, 1);
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
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  address_free_fn free_address = (address_free_fn)cuinterpose_lookup_real_symbol("cuMemAddressFree");

  if (range->base == 0)
    return;
  if (unmap != NULL)
    (void)unmap(range->base, range->size);
  if (free_address != NULL)
    (void)free_address(range->base, range->size);
  range->base = 0;
  range->size = 0;
}

/*
 * SAVE_HOST_CARRIER. Returns the bytes copied through *bytes and the time the
 * copies took through *copy_us. On failure nothing is left pinned or recorded.
 */
static int
save_host_carriers(uint64_t* bytes, uint32_t* copy_us, const char** error)
{
  host_register_fn register_host = (host_register_fn)cuinterpose_lookup_real_symbol("cuMemHostRegister_v2");
  host_unregister_fn unregister_host = (host_unregister_fn)cuinterpose_lookup_real_symbol("cuMemHostUnregister");
  copy_dtoh_async_fn copy = (copy_dtoh_async_fn)cuinterpose_lookup_real_symbol("cuMemcpyDtoHAsync_v2");
  stream_create_fn stream_create = (stream_create_fn)cuinterpose_lookup_real_symbol("cuStreamCreate");
  stream_synchronize_fn stream_synchronize =
      (stream_synchronize_fn)cuinterpose_lookup_real_symbol("cuStreamSynchronize");
  stream_destroy_fn stream_destroy = (stream_destroy_fn)cuinterpose_lookup_real_symbol("cuStreamDestroy_v2");
  struct allocation_list list = {NULL, 0};
  struct cuinterpose_context_scope scope;
  struct staging_range range = {0, 0};
  CUmemGenericAllocationHandle* handles = NULL;
  CUstream stream = NULL;
  void* base = MAP_FAILED;
  uint64_t total = 0;
  size_t placed = 0;
  size_t begin;
  size_t end;
  size_t index;
  double copy_time = 0.0;
  bool entered = false;
  bool registered = false;
  int result = -1;

  *bytes = 0;
  *copy_us = 0;
  *error = "host carrier symbols are unavailable";
  if (register_host == NULL || unregister_host == NULL || copy == NULL || stream_create == NULL ||
      stream_synchronize == NULL || stream_destroy == NULL)
    return -1;
  *error = "cannot allocate";
  if (collect_carriers(wants_saving, &list, &total) != 0)
    return -1;
  for (index = 0; index < list.count; index++) {
    if (list.items[index]->driver == 0) {
      *error = "creator allocation has no carrier handle";
      goto done;
    }
  }
  if (list.count == 0) {
    result = 0;
    *error = NULL;
    goto done;
  }
  if (arena.base != NULL) {
    *error = "a host carrier arena already exists";
    goto done;
  }
  handles = calloc(list.count, sizeof(*handles));
  if (handles == NULL)
    goto done;
  /* Populate up front: the pages have to exist before they can be pinned, and
   * faulting them in one call beats faulting them one by one while pinning. */
  base = mmap(NULL, total, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_POPULATE, -1, 0);
  if (base == MAP_FAILED) {
    *error = "cannot allocate host carrier memory";
    goto done;
  }
  for (begin = 0; begin < list.count; begin = end) {
    struct timespec started;
    struct timespec finished;
    uint64_t batch_bytes;
    size_t offset = 0;

    end = context_batch_end(&list, begin, &batch_bytes);
    if (enter_allocation_context(list.items[begin], &scope) != 0) {
      *error = "cannot enter the allocation's CUDA context";
      goto done;
    }
    entered = true;
    if (!registered) {
      /* Pinned once, as portable memory, so every context can copy from it. */
      if (register_host(base, total, CU_MEMHOSTREGISTER_PORTABLE) != CUDA_SUCCESS) {
        *error = "cannot pin host carrier memory";
        goto done;
      }
      registered = true;
      arena.context = list.items[begin]->context;
      arena.device = list.items[begin]->properties.location.type == CU_MEM_LOCATION_TYPE_DEVICE
                         ? (CUdevice)list.items[begin]->properties.location.id
                         : CUINTERPOSE_NO_DEVICE;
    }
    for (index = begin; index < end; index++)
      handles[index] = list.items[index]->driver;
    if (map_staging_range(&list.items[begin], &handles[begin], end - begin, batch_bytes, &range) != CUDA_SUCCESS) {
      *error = "cannot map creator allocations for the host copy";
      goto done;
    }
    if (stream_create(&stream, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
      stream = NULL;
      *error = "cannot create the carrier stream";
      goto done;
    }
    clock_gettime(CLOCK_MONOTONIC, &started);
    for (index = begin; index < end; index++) {
      struct allocation* allocation = list.items[index];
      void* host = (char*)base + placed;

      if (copy(host, range.base + offset, allocation->size, stream) != CUDA_SUCCESS) {
        *error = "device-to-host copy failed";
        goto done;
      }
      allocation->host_carrier = host;
      placed += allocation->size;
      offset += allocation->size;
    }
    if (stream_synchronize(stream) != CUDA_SUCCESS) {
      *error = "device-to-host copies did not complete";
      goto done;
    }
    clock_gettime(CLOCK_MONOTONIC, &finished);
    copy_time += elapsed_milliseconds(&started, &finished);
    *bytes += batch_bytes;
    unmap_staging_range(&range);
    (void)stream_destroy(stream);
    stream = NULL;
    entered = false;
    if (leave_context(&scope) != 0) {
      *error = "cannot leave the allocation's CUDA context";
      goto done;
    }
  }
  for (index = 0; index < list.count; index++)
    list.items[index]->host_checkpointed = true;
  arena.base = base;
  arena.size = total;
  *copy_us = (uint32_t)(copy_time * 1e3 + 0.5);
  result = 0;
  *error = NULL;
done:
  unmap_staging_range(&range);
  if (stream != NULL)
    (void)stream_destroy(stream);
  if (result != 0) {
    /* Undo so a retry (or the failure path) sees a clean slate. */
    if (registered) {
      if (!entered && enter_allocation_context(list.items[0], &scope) == 0)
        entered = true;
      (void)unregister_host(base);
      arena.context = NULL;
    }
    if (base != MAP_FAILED)
      munmap(base, total);
    for (index = 0; index < list.count; index++)
      list.items[index]->host_carrier = NULL;
  }
  if (entered)
    (void)leave_context(&scope);
  free(handles);
  free(list.items);
  return result;
}

/*
 * RESTORE_HOST_CARRIER: fresh device memory for every carried allocation,
 * contents copied back from the arena. The arena itself is released by
 * release_carrier_arena() once the reply is out; until then a failure here
 * loses nothing.
 */
static int
restore_host_carriers(uint64_t* bytes, uint32_t* copy_us, const char** error)
{
  create_fn create = (create_fn)cuinterpose_lookup_real_symbol("cuMemCreate");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  host_register_fn register_host = (host_register_fn)cuinterpose_lookup_real_symbol("cuMemHostRegister_v2");
  host_get_flags_fn host_flags = (host_get_flags_fn)cuinterpose_lookup_real_symbol("cuMemHostGetFlags");
  copy_htod_async_fn copy = (copy_htod_async_fn)cuinterpose_lookup_real_symbol("cuMemcpyHtoDAsync_v2");
  stream_create_fn stream_create = (stream_create_fn)cuinterpose_lookup_real_symbol("cuStreamCreate");
  stream_synchronize_fn stream_synchronize =
      (stream_synchronize_fn)cuinterpose_lookup_real_symbol("cuStreamSynchronize");
  stream_destroy_fn stream_destroy = (stream_destroy_fn)cuinterpose_lookup_real_symbol("cuStreamDestroy_v2");
  struct allocation_list list = {NULL, 0};
  struct cuinterpose_context_scope scope;
  struct staging_range range = {0, 0};
  CUmemGenericAllocationHandle* fresh = NULL;
  CUstream stream = NULL;
  uint64_t total = 0;
  size_t begin;
  size_t end;
  size_t index;
  double copy_time = 0.0;
  bool entered = false;
  bool checked = false;
  int result = -1;

  *bytes = 0;
  *copy_us = 0;
  *error = "host carrier symbols are unavailable";
  if (create == NULL || release == NULL || register_host == NULL || copy == NULL || stream_create == NULL ||
      stream_synchronize == NULL || stream_destroy == NULL)
    return -1;
  *error = "cannot allocate";
  if (collect_carriers(wants_restoring, &list, &total) != 0)
    return -1;
  if (list.count == 0) {
    result = 0;
    *error = NULL;
    goto done;
  }
  if (arena.base == NULL) {
    *error = "host carrier arena is missing";
    goto done;
  }
  for (index = 0; index < list.count; index++) {
    if (list.items[index]->host_carrier == NULL) {
      *error = "host carrier is missing";
      goto done;
    }
  }
  fresh = calloc(list.count, sizeof(*fresh));
  if (fresh == NULL)
    goto done;
  for (begin = 0; begin < list.count; begin = end) {
    struct timespec started;
    struct timespec finished;
    uint64_t batch_bytes;
    size_t offset = 0;

    end = context_batch_end(&list, begin, &batch_bytes);
    if (enter_allocation_context(list.items[begin], &scope) != 0) {
      *error = "cannot enter the allocation's CUDA context";
      goto done;
    }
    entered = true;
    if (!checked) {
      unsigned int flags = 0;

      /* The arena should still be pinned after the native restore. If it is
       * not, pin it again: a copy from pageable memory runs far below the
       * link's speed, and this copy is in the restore hot path. */
      if (host_flags == NULL || host_flags(&flags, arena.base) != CUDA_SUCCESS) {
        if (register_host(arena.base, arena.size, CU_MEMHOSTREGISTER_PORTABLE) != CUDA_SUCCESS) {
          *error = "cannot pin host carrier memory for the copy back";
          goto done;
        }
        warn("host carrier registration did not survive restore; re-registered");
      }
      checked = true;
      arena.context = list.items[begin]->context;
      arena.device = list.items[begin]->properties.location.type == CU_MEM_LOCATION_TYPE_DEVICE
                         ? (CUdevice)list.items[begin]->properties.location.id
                         : CUINTERPOSE_NO_DEVICE;
    }
    for (index = begin; index < end; index++) {
      if (create(&fresh[index], list.items[index]->size, &list.items[index]->properties, 0) != CUDA_SUCCESS) {
        fresh[index] = 0;
        *error = "cannot create fresh device memory for a creator allocation";
        goto done;
      }
    }
    if (map_staging_range(&list.items[begin], &fresh[begin], end - begin, batch_bytes, &range) != CUDA_SUCCESS) {
      *error = "cannot map fresh device memory for the copy back";
      goto done;
    }
    if (stream_create(&stream, CU_STREAM_NON_BLOCKING) != CUDA_SUCCESS) {
      stream = NULL;
      *error = "cannot create the carrier stream";
      goto done;
    }
    clock_gettime(CLOCK_MONOTONIC, &started);
    for (index = begin; index < end; index++) {
      const struct allocation* allocation = list.items[index];

      if (copy(range.base + offset, allocation->host_carrier, allocation->size, stream) != CUDA_SUCCESS) {
        *error = "host-to-device copy failed";
        goto done;
      }
      offset += allocation->size;
    }
    if (stream_synchronize(stream) != CUDA_SUCCESS) {
      *error = "host-to-device copies did not complete";
      goto done;
    }
    clock_gettime(CLOCK_MONOTONIC, &finished);
    copy_time += elapsed_milliseconds(&started, &finished);
    *bytes += batch_bytes;
    unmap_staging_range(&range);
    (void)stream_destroy(stream);
    stream = NULL;
    entered = false;
    if (leave_context(&scope) != 0) {
      *error = "cannot leave the allocation's CUDA context";
      goto done;
    }
  }
  /* Every copy landed: publish the fresh handles. */
  for (index = 0; index < list.count; index++) {
    struct allocation* allocation = list.items[index];

    allocation->driver = fresh[index];
    fresh[index] = 0;
    allocation->host_carrier = NULL;
    allocation->host_checkpointed = false;
  }
  *copy_us = (uint32_t)(copy_time * 1e3 + 0.5);
  result = 0;
  *error = NULL;
done:
  unmap_staging_range(&range);
  if (stream != NULL)
    (void)stream_destroy(stream);
  if (result != 0 && fresh != NULL) {
    for (index = 0; index < list.count; index++) {
      if (fresh[index] != 0)
        (void)release(fresh[index]);
    }
  }
  if (entered)
    (void)leave_context(&scope);
  free(fresh);
  free(list.items);
  return result;
}

/*
 * Unpins and frees the arena. Called after the RESTORE_HOST_CARRIER reply, so
 * the coordinator (and the application behind it) does not wait for the
 * unpinning of gigabytes of host memory. Runs without state_lock.
 */
static void
release_carrier_arena(void)
{
  host_unregister_fn unregister_host = (host_unregister_fn)cuinterpose_lookup_real_symbol("cuMemHostUnregister");
  struct cuinterpose_context_scope scope;
  struct carrier_arena local;

  pthread_mutex_lock(&state_lock);
  local = arena;
  memset(&arena, 0, sizeof(arena));
  pthread_mutex_unlock(&state_lock);
  if (local.base == NULL)
    return;
  if (unregister_host != NULL && cuinterpose_enter_context(local.context, local.device, &scope) == 0) {
    (void)unregister_host(local.base);
    (void)cuinterpose_leave_context(&scope);
  }
  munmap(local.base, local.size);
}

/* PREPARE: leave nothing shared mapped or held, so the native checkpoint sees
 * only private memory. The records stay; restore rebuilds from them. */
static int
prepare(const char** error)
{
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  unmap_fn unmap = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  struct allocation_list list;
  size_t index;

  if (release == NULL || unmap == NULL) {
    *error = "driver symbols are unavailable";
    return -1;
  }
  for (index = 0; index < mappings.count; index++) {
    const struct mapping* mapping = mappings.items[index].value;
    if (mapping->access_unknown) {
      *error = "a mapping's access state is unknown after a failed cuMemSetAccess";
      return -1;
    }
  }
  if (list_allocations(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  /* Peers must not fetch descriptors that are about to be invalidated. */
  cuinterpose_export_cache_quiesce();
  for (index = 0; index < list.count; index++) {
    struct allocation* allocation = list.items[index];
    struct cuinterpose_context_scope scope;
    size_t range;

    if (needs_host_carrier(allocation) && !allocation->host_checkpointed) {
      free(list.items);
      *error = "a creator allocation was not saved to its host carrier";
      return -1;
    }
    if (enter_allocation_context(allocation, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the allocation's CUDA context";
      return -1;
    }
    for (range = 0; range < mappings.count; range++) {
      struct mapping* mapping = mappings.items[range].value;
      if (mapping->allocation != allocation || mapping->checkpointed)
        continue;
      if (unmap(mapping->address, mapping->size) != CUDA_SUCCESS) {
        (void)leave_context(&scope);
        free(list.items);
        *error = "cuMemUnmap failed during prepare";
        return -1;
      }
      mapping->checkpointed = true;
    }
    if (allocation->driver != 0) {
      if (release(allocation->driver) != CUDA_SUCCESS) {
        (void)leave_context(&scope);
        free(list.items);
        *error = "cuMemRelease failed during prepare";
        return -1;
      }
      allocation->driver = 0;
    }
    allocation->checkpointed = true;
    if (leave_context(&scope) != 0) {
      free(list.items);
      *error = "cannot leave the allocation's CUDA context";
      return -1;
    }
  }
  free(list.items);
  return 0;
}

/* Map every checkpointed mapping of allocation back where it was. */
static CUresult
remap_allocation(struct allocation* allocation, const char** error)
{
  map_fn map = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  access_fn set_access = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  size_t index;

  if (map == NULL || set_access == NULL) {
    *error = "mapping symbols are unavailable";
    return CUDA_ERROR_NOT_INITIALIZED;
  }
  for (index = 0; index < mappings.count; index++) {
    struct mapping* mapping = mappings.items[index].value;
    CUresult result;

    if (mapping->allocation != allocation || !mapping->checkpointed)
      continue;
    result = map(mapping->address, mapping->size, mapping->offset, allocation->driver, 0);
    if (result != CUDA_SUCCESS) {
      *error = "cuMemMap failed during restore";
      return result;
    }
    mapping->checkpointed = false;
    if (mapping->access_count != 0) {
      result = set_access(mapping->address, mapping->size, mapping->access, mapping->access_count);
      if (result != CUDA_SUCCESS) {
        *error = "cuMemSetAccess failed during restore";
        return result;
      }
    }
  }
  return CUDA_SUCCESS;
}

/* Once an allocation is back, keep the driver handle only if the application
 * still holds a logical handle; mappings keep the memory alive otherwise. */
static int
finish_restore(struct allocation* allocation, const char** error)
{
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");

  allocation->checkpointed = false;
  if (allocation->live_handles == 0 && allocation->driver != 0) {
    if (release == NULL || release(allocation->driver) != CUDA_SUCCESS) {
      *error = "cuMemRelease failed during restore";
      return -1;
    }
    allocation->driver = 0;
  }
  return 0;
}

/* RESTORE_CREATORS: creators remap and re-export. Every creator must be done
 * before any importer asks, which the coordinator guarantees with a barrier. */
static int
restore_creators(const char** error)
{
  export_fn export_handle = (export_fn)cuinterpose_lookup_real_symbol("cuMemExportToShareableHandle");
  struct allocation_list list;
  size_t index;

  if (export_handle == NULL) {
    *error = "cuMemExportToShareableHandle is unavailable";
    return -1;
  }
  if (list_allocations(&list) != 0) {
    *error = "cannot allocate";
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct allocation* allocation = list.items[index];
    struct cuinterpose_context_scope scope;

    if (!allocation->creator || !allocation->checkpointed)
      continue;
    if (allocation->driver == 0) {
      free(list.items);
      *error = "creator allocation has no device memory after restore";
      return -1;
    }
    if (enter_allocation_context(allocation, &scope) != 0) {
      free(list.items);
      *error = "cannot enter the allocation's CUDA context";
      return -1;
    }
    if (remap_allocation(allocation, error) != CUDA_SUCCESS) {
      (void)leave_context(&scope);
      free(list.items);
      return -1;
    }
    if (allocation->shared) {
      int real_fd = -1;
      if (export_handle(&real_fd, allocation->driver, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0) != CUDA_SUCCESS ||
          cuinterpose_export_cache_put(allocation->id, allocation->authorization, real_fd) != 0) {
        if (real_fd >= 0)
          close(real_fd);
        (void)leave_context(&scope);
        free(list.items);
        *error = "cannot re-export a restored creator allocation";
        return -1;
      }
    }
    if (finish_restore(allocation, error) != 0 || leave_context(&scope) != 0) {
      free(list.items);
      if (*error == NULL)
        *error = "cannot leave the allocation's CUDA context";
      return -1;
    }
  }
  free(list.items);
  /* Peers may fetch descriptors again. */
  cuinterpose_export_cache_resume();
  return 0;
}

/* RESTORE_IMPORTERS: importers fetch fresh descriptors and remap. */
static int
restore_importers(char* message, size_t message_size)
{
  import_fn import_handle = (import_fn)cuinterpose_lookup_real_symbol("cuMemImportFromShareableHandle");
  struct allocation_list list;
  size_t index;

  if (import_handle == NULL) {
    snprintf(message, message_size, "%s", "cuMemImportFromShareableHandle is unavailable");
    return -1;
  }
  if (list_allocations(&list) != 0) {
    snprintf(message, message_size, "%s", "cannot allocate");
    return -1;
  }
  for (index = 0; index < list.count; index++) {
    struct allocation* allocation = list.items[index];
    struct cuinterpose_posix_ticket ticket;
    struct cuinterpose_context_scope scope;
    const char* error = NULL;
    char export_error[96];
    int raw_fd = -1;
    CUmemGenericAllocationHandle imported = 0;
    CUresult result;

    if (allocation->creator || !allocation->checkpointed)
      continue;
    memset(&ticket, 0, sizeof(ticket));
    ticket.magic = CUINTERPOSE_POSIX_TICKET_MAGIC;
    ticket.version = CUINTERPOSE_POSIX_TICKET_VERSION;
    ticket.resource_kind = CUINTERPOSE_RESOURCE_UNICAST;
    snprintf(ticket.creator_participant, sizeof(ticket.creator_participant), "%s", allocation->creator_participant);
    memcpy(ticket.allocation_id, allocation->id, sizeof(ticket.allocation_id));
    snprintf(ticket.creator_endpoint, sizeof(ticket.creator_endpoint), "%s", allocation->creator_endpoint);
    memcpy(ticket.authorization, allocation->authorization, sizeof(ticket.authorization));
    /* The creator's listener answers without any lock of its own that we hold,
     * so the request can be made while holding state_lock. */
    if (request_export(&ticket, &raw_fd, export_error, sizeof(export_error)) != 0) {
      free(list.items);
      snprintf(message, message_size, "creator export: %.70s", export_error);
      return -1;
    }
    if (enter_allocation_context(allocation, &scope) != 0) {
      close(raw_fd);
      free(list.items);
      snprintf(message, message_size, "%s", "cannot enter the allocation's CUDA context");
      return -1;
    }
    result = import_handle(&imported, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
    close(raw_fd);
    if (result != CUDA_SUCCESS) {
      (void)leave_context(&scope);
      free(list.items);
      snprintf(message, message_size, "cuMemImportFromShareableHandle failed: CUresult=%d", (int)result);
      return -1;
    }
    allocation->driver = imported;
    if (remap_allocation(allocation, &error) != CUDA_SUCCESS || finish_restore(allocation, &error) != 0 ||
        leave_context(&scope) != 0) {
      free(list.items);
      snprintf(message, message_size, "%s", error != NULL ? error : "cannot leave the allocation's CUDA context");
      return -1;
    }
  }
  free(list.items);
  return 0;
}

static void
serve(int client)
{
  struct cuinterpose_header request;
  struct cuinterpose_header response;
  int passed_fd = -1;

  if (cuinterpose_receive_header(client, &request, &passed_fd) != 0)
    return;
  if (passed_fd >= 0)
    close(passed_fd);
  memset(&response, 0, sizeof(response));
  response.magic = CUINTERPOSE_MAGIC;
  response.version = CUINTERPOSE_VERSION;
  response.operation = request.operation;
  snprintf(response.participant_id, sizeof(response.participant_id), "%s", participant_id);
  response.live_raw_imports = atomic_load(&live_raw_imports);
  response.passthrough_creations = atomic_load(&passthrough_creations);
  response.phase = phase_code();
  if (passed_fd >= 0 || !cuinterpose_header_strings_terminated(&request) ||
      request.magic != CUINTERPOSE_MAGIC || request.version != CUINTERPOSE_VERSION || request.status != 0 ||
      request.count != 0 ||
      !((request.operation == CUINTERPOSE_IDENTIFY && request.participant_id[0] == '\0') ||
        strcmp(request.participant_id, participant_id) == 0)) {
    cuinterpose_header_error(&response, "invalid cuinterpose control request");
    (void)cuinterpose_send_header(client, &response, -1);
    return;
  }
  switch (request.operation) {
    case CUINTERPOSE_IDENTIFY:
      if (current_phase == PHASE_FAILED)
        cuinterpose_header_error(&response, failure);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    case CUINTERPOSE_INSPECT: {
      struct cuinterpose_record* records;
      const char* error;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_ACTIVE) {
        pthread_mutex_unlock(&state_lock);
        cuinterpose_header_error(&response, "cuinterpose is not in the active phase");
        (void)cuinterpose_send_header(client, &response, -1);
        break;
      }
      records = inspect_records(&response.count, &error);
      pthread_mutex_unlock(&state_lock);
      if (records == NULL) {
        cuinterpose_header_error(&response, error);
        (void)cuinterpose_send_header(client, &response, -1);
        break;
      }
      response.payload_size = (uint64_t)response.count * sizeof(struct cuinterpose_record);
      if (cuinterpose_send_header(client, &response, -1) == 0 && response.payload_size != 0)
        (void)cuinterpose_send_all(client, records, (size_t)response.payload_size);
      free(records);
      break;
    }
    case CUINTERPOSE_PREPARE_MULTICAST: {
      const char* error = NULL;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_ACTIVE) {
        error = "cuinterpose is not in the active phase";
      } else if (create_carriers(&error) == 0) {
        current_phase = PHASE_CARRIERS;
      } else {
        set_failure(error);
      }
      pthread_mutex_unlock(&state_lock);
      if (error != NULL)
        cuinterpose_header_error(&response, error);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_SAVE_HOST_CARRIER: {
      const char* error = NULL;
      uint64_t bytes = 0;
      uint32_t copy_us = 0;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_CARRIERS) {
        error = "carriers were not created before the host copy";
      } else if (save_host_carriers(&bytes, &copy_us, &error) != 0) {
        set_failure(error);
      }
      pthread_mutex_unlock(&state_lock);
      if (error != NULL)
        cuinterpose_header_error(&response, error);
      response.payload_size = bytes;
      response.copy_us = copy_us;
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_PREPARE: {
      const char* error = NULL;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_CARRIERS) {
        error = "carriers were not created before prepare";
      } else if (prepare(&error) == 0) {
        current_phase = PHASE_PREPARED;
      } else {
        set_failure(error);
      }
      pthread_mutex_unlock(&state_lock);
      if (error != NULL)
        cuinterpose_header_error(&response, error);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_RESTORE_HOST_CARRIER: {
      const char* error = NULL;
      uint64_t bytes = 0;
      uint32_t copy_us = 0;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_PREPARED) {
        error = "cuinterpose is not in the prepared phase";
      } else if (restore_host_carriers(&bytes, &copy_us, &error) != 0) {
        set_failure(error);
      }
      pthread_mutex_unlock(&state_lock);
      if (error != NULL)
        cuinterpose_header_error(&response, error);
      response.payload_size = bytes;
      response.copy_us = copy_us;
      (void)cuinterpose_send_header(client, &response, -1);
      /* Off the hot path: the coordinator has its answer and moves on. */
      if (error == NULL)
        release_carrier_arena();
      break;
    }
    case CUINTERPOSE_RESTORE_CREATORS: {
      const char* error = NULL;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_PREPARED) {
        error = "cuinterpose is not in the prepared phase";
      } else if (restore_creators(&error) == 0) {
        current_phase = PHASE_CREATORS_RESTORED;
      } else {
        set_failure(error);
      }
      pthread_mutex_unlock(&state_lock);
      if (error != NULL)
        cuinterpose_header_error(&response, error);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_RESTORE_IMPORTERS: {
      char message[96] = {0};
      bool failed = false;

      pthread_mutex_lock(&state_lock);
      if (current_phase != PHASE_CREATORS_RESTORED) {
        snprintf(message, sizeof(message), "%s", "creators were not restored before importers");
        failed = true;
      } else if (restore_importers(message, sizeof(message)) == 0) {
        current_phase = PHASE_UNICAST_RESTORED;
      } else {
        set_failure(message);
        failed = true;
      }
      pthread_mutex_unlock(&state_lock);
      if (failed)
        cuinterpose_header_error(&response, message);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_RESTORE_MULTICAST_CREATORS:
    case CUINTERPOSE_RESTORE_MULTICAST_IMPORTERS:
    case CUINTERPOSE_RESTORE_MULTICAST_DEVICES:
    case CUINTERPOSE_RESTORE_MULTICAST: {
      /* No multicast state in this layer: each phase only advances. */
      static const enum phase expected[] = {
          PHASE_UNICAST_RESTORED, PHASE_MULTICAST_CREATED, PHASE_MULTICAST_IMPORTED, PHASE_MULTICAST_JOINED};
      static const enum phase next[] = {
          PHASE_MULTICAST_CREATED, PHASE_MULTICAST_IMPORTED, PHASE_MULTICAST_JOINED, PHASE_ACTIVE};
      size_t step = request.operation - CUINTERPOSE_RESTORE_MULTICAST_CREATORS;

      pthread_mutex_lock(&state_lock);
      if (current_phase != expected[step]) {
        pthread_mutex_unlock(&state_lock);
        cuinterpose_header_error(&response, "multicast restore phase out of order");
        (void)cuinterpose_send_header(client, &response, -1);
        break;
      }
      current_phase = next[step];
      if (current_phase == PHASE_ACTIVE)
        failure[0] = '\0';
      pthread_mutex_unlock(&state_lock);
      (void)cuinterpose_send_header(client, &response, -1);
      break;
    }
    case CUINTERPOSE_EXPORT: {
      /* A peer holding a ticket wants the real descriptor. Served from the
       * export cache only: no driver call, no state_lock. */
      const char* reason = NULL;
      int dup = -1;

      if (request.resource_kind != CUINTERPOSE_RESOURCE_UNICAST) {
        cuinterpose_header_error(&response, "creator resource is unavailable");
        (void)cuinterpose_send_header(client, &response, -1);
        break;
      }
      if (cuinterpose_export_cache_begin(request.allocation_id, request.authorization, &dup, &reason) != 0) {
        cuinterpose_header_error(&response, reason);
        (void)cuinterpose_send_header(client, &response, -1);
        break;
      }
      response.resource_kind = request.resource_kind;
      memcpy(response.allocation_id, request.allocation_id, sizeof(response.allocation_id));
      (void)cuinterpose_send_header(client, &response, dup);
      close(dup);
      cuinterpose_export_cache_end(request.allocation_id);
      break;
    }
    default:
      cuinterpose_header_error(&response, "operation is not supported by this shim build");
      (void)cuinterpose_send_header(client, &response, -1);
      break;
  }
}

static void*
control_connection(void* argument)
{
  int client = (int)(intptr_t)argument;

  if (cuinterpose_set_socket_timeouts(client, cuinterpose_control_timeout_seconds()) == 0)
    serve(client);
  close(client);
  return NULL;
}

static void*
control_agent(void* unused)
{
  unsigned backoff_ms = 1;

  (void)unused;
  for (;;) {
    int client = accept4(listener, NULL, NULL, SOCK_CLOEXEC);
    pthread_t worker;

    if (client < 0) {
      switch (errno) {
        case EINTR:
          continue;
        case EMFILE:
        case ENFILE:
        case ENOBUFS:
        case ENOMEM:
        case ECONNABORTED:
        case EAGAIN:
          /* Transient: back off and keep listening. Giving up here would
           * leave this process unreachable for every future checkpoint. */
          usleep(backoff_ms * 1000);
          if (backoff_ms < 1000)
            backoff_ms *= 2;
          continue;
        default:
          /* EBADF, EINVAL: the listener is gone (fork child, shutdown). */
          return NULL;
      }
    }
    backoff_ms = 1;
    /* One detached thread per connection: a slow request never blocks accept. */
    if (pthread_create(&worker, NULL, control_connection, (void*)(intptr_t)client) == 0)
      (void)pthread_detach(worker);
    else
      (void)control_connection((void*)(intptr_t)client);
  }
}

/*
 * A process that exits without running destructors (os._exit, a signal) leaves
 * its socket behind, and CUDA workloads spawn many such helpers. Remove the
 * sockets whose process no longer exists, so the control directory describes
 * the processes that are actually there.
 */
static void
remove_dead_sockets(void)
{
  DIR* directory = opendir(control_directory);
  struct dirent* entry;

  if (directory == NULL)
    return;
  while ((entry = readdir(directory)) != NULL) {
    const char* name = entry->d_name;
    const char* digits;
    long pid = 0;

    if (strncmp(name, CUINTERPOSE_SOCKET_PREFIX, strlen(CUINTERPOSE_SOCKET_PREFIX)) != 0)
      continue;
    /* Hand-rolled: strtol resolves to a glibc 2.38 symbol under C2x headers,
     * and the shim must load on glibc 2.34. */
    for (digits = name + strlen(CUINTERPOSE_SOCKET_PREFIX); *digits >= '0' && *digits <= '9' && pid < 100000000L;
         digits++)
      pid = pid * 10 + (*digits - '0');
    if (pid <= 0 || strcmp(digits, ".sock") != 0 || pid == (long)getpid())
      continue;
    if (kill((pid_t)pid, 0) == -1 && errno == ESRCH) {
      char path[sizeof(socket_path)];

      if (snprintf(path, sizeof(path), "%s/%s", control_directory, name) < (int)sizeof(path))
        unlink(path);
    }
  }
  closedir(directory);
}

static int
start_control_endpoint(void)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  pthread_t thread;
  int count;

  remove_dead_sockets();
  count = snprintf(
      socket_path, sizeof(socket_path), "%s/%s%ld.sock", control_directory, CUINTERPOSE_SOCKET_PREFIX,
      (long)getpid());
  if (count < 0 || (size_t)count >= sizeof(socket_path)) {
    socket_path[0] = '\0';
    return -1;
  }
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", socket_path);
  listener = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (listener < 0)
    goto failed;
  /* A stale socket from an earlier process with this PID would make bind fail. */
  unlink(socket_path);
  if (bind(listener, (const struct sockaddr*)&address, sizeof(address)) != 0)
    goto failed;
  /* Owner-only. Not authentication (any same-UID process may connect), but it
   * keeps other users on a shared node out. chmod on the path, because fchmod
   * on a socket descriptor does not change the filesystem node. */
  if (chmod(socket_path, 0600) != 0)
    goto failed;
  if (listen(listener, 16) != 0 || pthread_create(&thread, NULL, control_agent, NULL) != 0)
    goto failed;
  pthread_detach(thread);
  return 0;
failed:
  if (listener >= 0)
    close(listener);
  listener = -1;
  unlink(socket_path);
  socket_path[0] = '\0';
  return -1;
}

/* ------------------------------------------------------------------------- */
/* Process lifecycle: constructor, fork, lazy endpoint in children.           */
/* ------------------------------------------------------------------------- */

static void
fork_prepare(void)
{
  pthread_mutex_lock(&state_lock);
  cuinterpose_export_cache_fork_prepare();
}

static void
fork_parent(void)
{
  cuinterpose_export_cache_fork_parent();
  pthread_mutex_unlock(&state_lock);
}

static int
free_value(struct cuinterpose_key key, void* value, void* arg)
{
  (void)key;
  (void)arg;
  free(value);
  return 0;
}

static void
fork_child(void)
{
  size_t index;

  /*
   * The child inherits the parent's records but none of the parent's CUDA
   * state is usable in it (CUDA contexts do not survive fork). Drop the
   * bookkeeping without touching the driver; the child gets its own identity
   * and socket on its first CUDA activity.
   */
  if (listener >= 0)
    close(listener);
  listener = -1;
  participant_id[0] = '\0';
  socket_path[0] = '\0';
  endpoint_needs_initialization = true;
  cuinterpose_table_each(&handles, free_value, NULL);
  cuinterpose_table_clear(&handles);
  for (index = 0; index < mappings.count; index++)
    free(mappings.items[index].value);
  cuinterpose_ranges_clear(&mappings);
  cuinterpose_table_each(&allocations, free_value, NULL);
  cuinterpose_table_clear(&allocations);
  cuinterpose_table_clear(&raw_imports);
  atomic_store(&live_raw_imports, 0);
  atomic_store(&passthrough_creations, 0);
  next_logical_handle = 1;
  current_phase = PHASE_ACTIVE;
  failure[0] = '\0';
  if (arena.base != NULL)
    munmap(arena.base, arena.size);
  memset(&arena, 0, sizeof(arena));
  cuinterpose_export_cache_fork_child();
  pthread_mutex_init(&state_lock, NULL);
}

CUresult
cuinterpose_ensure_process_endpoint(void)
{
  CUresult result = CUDA_SUCCESS;

  pthread_mutex_lock(&state_lock);
  if (endpoint_needs_initialization) {
    if (current_phase == PHASE_FAILED) {
      result = CUDA_ERROR_NOT_INITIALIZED;
    } else if (cuinterpose_random_id(participant_id) != 0 || start_control_endpoint() != 0) {
      set_failure("cannot start forked cuinterpose control endpoint");
      result = CUDA_ERROR_NOT_INITIALIZED;
    } else {
      endpoint_needs_initialization = false;
    }
  }
  pthread_mutex_unlock(&state_lock);
  return result;
}

__attribute__((constructor)) static void
initialize(void)
{
  const char* control;
  const char* configured_participant;

  configured_participant = getenv(CUINTERPOSE_PARTICIPANT_ID_ENV);
  if (configured_participant != NULL && !cuinterpose_is_lower_hex_id(configured_participant)) {
    set_failure("invalid cuinterpose participant identity");
    warn(failure);
    return;
  }
  if (configured_participant != NULL)
    snprintf(participant_id, sizeof(participant_id), "%s", configured_participant);
  else if (cuinterpose_random_id(participant_id) != 0) {
    set_failure("cannot create cuinterpose participant identity");
    warn(failure);
    return;
  }
  control = getenv(CUINTERPOSE_CONTROL_DIR_ENV);
  if (control == NULL || control[0] == '\0')
    control = CUINTERPOSE_CONTROL_DIR;
  if (control[0] != '/' || strlen(control) >= sizeof(control_directory)) {
    set_failure("invalid cuinterpose control directory");
    warn(failure);
    return;
  }
  snprintf(control_directory, sizeof(control_directory), "%s", control);
  if (pthread_atfork(fork_prepare, fork_parent, fork_child) != 0) {
    set_failure("cannot register cuinterpose fork handlers");
    warn(failure);
    return;
  }
  if (start_control_endpoint() != 0) {
    set_failure("cannot start cuinterpose control endpoint");
    warn(failure);
    return;
  }
}

__attribute__((destructor)) static void
finalize(void)
{
  if (listener >= 0)
    close(listener);
  if (socket_path[0] != '\0')
    unlink(socket_path);
}

/* ------------------------------------------------------------------------- */
/* Peer export: the same-process shortcut and the socket path.                */
/* ------------------------------------------------------------------------- */

/* Obtain the real descriptor for a ticket. Caller must not hold state_lock. */
static int
request_export(const struct cuinterpose_posix_ticket* ticket, int* output, char* error, size_t error_size)
{
  if (strcmp(ticket->creator_participant, participant_id) == 0) {
    const char* reason = NULL;
    int dup = -1;

    if (cuinterpose_export_cache_begin(ticket->allocation_id, ticket->authorization, &dup, &reason) != 0) {
      if (error != NULL && error_size != 0)
        snprintf(error, error_size, "%s", reason);
      *output = -1;
      return -1;
    }
    cuinterpose_export_cache_end(ticket->allocation_id);
    *output = dup;
    return 0;
  }
  return cuinterpose_posix_request_export(ticket, output, error, error_size);
}

/* ------------------------------------------------------------------------- */
/* The CUDA entry points.                                                     */
/* ------------------------------------------------------------------------- */

CUINTERPOSE_API CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  create_fn function = (create_fn)cuinterpose_lookup_real_symbol("cuMemCreate");
  struct allocation* allocation;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  if (properties == NULL || properties->requestedHandleTypes != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) {
    /*
     * Only allocations created with exactly the POSIX fd handle type are
     * tracked. Everything else, including FABRIC and combinations, goes to the
     * driver untouched and is invisible to checkpoint repair.
     */
    if (properties != NULL && properties->requestedHandleTypes != 0) {
      atomic_fetch_add(&passthrough_creations, 1);
      if ((properties->requestedHandleTypes & CU_MEM_HANDLE_TYPE_FABRIC) != 0 &&
          !atomic_exchange(&fabric_passthrough_logged, true))
        warn("an allocation with the FABRIC handle type passed through untracked; "
             "its sharing will not survive checkpoint/restore");
    }
    result = function(&driver, size, properties, flags);
    return passthrough_handle(result, driver, output);
  }
  result = function(&driver, size, properties, flags);
  if (result != CUDA_SUCCESS)
    return result;
  allocation = calloc(1, sizeof(*allocation));
  pthread_mutex_lock(&state_lock);
  if (current_phase != PHASE_ACTIVE) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
    pthread_mutex_unlock(&state_lock);
    if (release != NULL)
      (void)release(driver);
    free(allocation);
    return CUDA_ERROR_NOT_READY;
  }
  if (allocation == NULL || cuinterpose_random_bytes(allocation->id, sizeof(allocation->id)) != 0 ||
      cuinterpose_random_bytes(allocation->authorization, sizeof(allocation->authorization)) != 0 ||
      cuinterpose_table_put(&allocations, cuinterpose_key_bytes(allocation->id), allocation) != 0) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
    pthread_mutex_unlock(&state_lock);
    if (release != NULL)
      (void)release(driver);
    free(allocation);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  allocation->size = size;
  allocation->properties = *properties;
  allocation->driver = driver;
  allocation->creator = true;
  cuinterpose_capture_context(&allocation->context);
  snprintf(allocation->creator_participant, sizeof(allocation->creator_participant), "%s", participant_id);
  snprintf(allocation->creator_endpoint, sizeof(allocation->creator_endpoint), "%s", socket_path);
  if (add_handle(allocation, &logical) != 0) {
    release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
    cuinterpose_table_remove(&allocations, cuinterpose_key_bytes(allocation->id));
    pthread_mutex_unlock(&state_lock);
    if (release != NULL)
      (void)release(driver);
    free(allocation);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle application)
{
  release_fn function = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct handle* handle;
  struct allocation* allocation;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = find_handle(application);
  if (handle == NULL) {
    if (is_logical_handle(application)) {
      pthread_mutex_unlock(&state_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
    /* An untracked import being released drops out of the raw-import count. */
    if (cuinterpose_table_remove(&raw_imports, cuinterpose_key_u64((uint64_t)application)) != NULL)
      atomic_fetch_sub(&live_raw_imports, 1);
    pthread_mutex_unlock(&state_lock);
    return function != NULL ? function(application) : cuinterpose_unavailable();
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  allocation = handle->allocation;
  cuinterpose_table_remove(&handles, cuinterpose_key_u64((uint64_t)application));
  free(handle);
  allocation->live_handles--;
  result = settle_allocation(allocation);
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  retain_fn function = (retain_fn)cuinterpose_lookup_real_symbol("cuMemRetainAllocationHandle");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct mapping* mapping;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  pthread_mutex_lock(&state_lock);
  mapping = mapping_at((CUdeviceptr)(uintptr_t)address);
  if (mapping == NULL) {
    pthread_mutex_unlock(&state_lock);
    result = function(&driver, address);
    return passthrough_handle(result, driver, output);
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  result = function(&driver, address);
  if (result != CUDA_SUCCESS) {
    pthread_mutex_unlock(&state_lock);
    return result;
  }
  /*
   * One backing driver handle per allocation: if we already hold one, the
   * fresh reference is redundant and released right away; the new logical
   * handle aliases the existing driver handle.
   */
  if (mapping->allocation->driver != 0) {
    if (release == NULL || release(driver) != CUDA_SUCCESS) {
      pthread_mutex_unlock(&state_lock);
      return CUDA_ERROR_UNKNOWN;
    }
  } else {
    mapping->allocation->driver = driver;
  }
  if (add_handle(mapping->allocation, &logical) != 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemMap(CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle application, unsigned long long flags)
{
  map_fn function = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");
  struct handle* handle;
  struct mapping* mapping;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = find_handle(application);
  if (handle == NULL) {
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(address, size, offset, application, flags) : cuinterpose_unavailable();
  }
  if (current_phase != PHASE_ACTIVE || handle->allocation->driver == 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  if (function == NULL) {
    pthread_mutex_unlock(&state_lock);
    return cuinterpose_unavailable();
  }
  if (size == 0 || (uint64_t)address > UINT64_MAX - size) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_VALUE;
  }
  mapping = calloc(1, sizeof(*mapping));
  if (mapping == NULL) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  /* Reserve the range first so an overlap with a tracked mapping is refused
   * before the driver is touched; two records for one address would make
   * restore ambiguous. */
  switch (cuinterpose_ranges_insert(&mappings, (uint64_t)address, (uint64_t)address + size, mapping)) {
    case 1:
      pthread_mutex_unlock(&state_lock);
      free(mapping);
      return CUDA_ERROR_INVALID_VALUE;
    case -1:
      pthread_mutex_unlock(&state_lock);
      free(mapping);
      return CUDA_ERROR_OUT_OF_MEMORY;
    default:
      break;
  }
  result = function(address, size, offset, handle->allocation->driver, flags);
  if (result != CUDA_SUCCESS) {
    struct cuinterpose_range* range = cuinterpose_ranges_at(&mappings, (uint64_t)address);
    if (range != NULL && range->value == mapping)
      cuinterpose_ranges_remove_at(&mappings, (size_t)(range - mappings.items));
    free(mapping);
    pthread_mutex_unlock(&state_lock);
    return result;
  }
  mapping->address = address;
  mapping->size = size;
  mapping->offset = offset;
  mapping->allocation = handle->allocation;
  handle->allocation->live_mappings++;
  adopt_context(handle->allocation);
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  unmap_fn function = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");
  size_t first;
  size_t last;
  size_t index;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (size == 0 || (uint64_t)address > UINT64_MAX - size)
    return function != NULL ? function(address, size) : cuinterpose_unavailable();
  pthread_mutex_lock(&state_lock);
  /*
   * The application may unmap a range that spans several tracked mappings at
   * once, as long as it does not cut through one. A partial cut cannot be
   * represented in the records, so it is refused rather than passed through
   * with a stale record left behind.
   */
  if (cuinterpose_ranges_cover(&mappings, (uint64_t)address, (uint64_t)address + size, &first, &last) != 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_VALUE;
  }
  if (first == last) {
    pthread_mutex_unlock(&state_lock);
    return function != NULL ? function(address, size) : cuinterpose_unavailable();
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  result = function != NULL ? function(address, size) : cuinterpose_unavailable();
  if (result == CUDA_SUCCESS) {
    for (index = last; index > first; index--) {
      struct mapping* mapping = mappings.items[index - 1].value;
      struct allocation* allocation = mapping->allocation;
      CUresult settle;

      cuinterpose_ranges_remove_at(&mappings, index - 1);
      free(mapping);
      allocation->live_mappings--;
      settle = settle_allocation(allocation);
      if (settle != CUDA_SUCCESS)
        result = settle;
    }
  }
  pthread_mutex_unlock(&state_lock);
  return result;
}

/*
 * Merge `descriptors` into a mapping's recorded access set, one entry per
 * (location type, location id). CUDA applies access per location, so a second
 * call for another GPU adds to what the first granted; NONE removes that
 * location. Returns -1 when the merged set would exceed the record's capacity.
 */
static int
merge_access(
    const struct mapping* mapping, const CUmemAccessDesc* descriptors, size_t count, CUmemAccessDesc* merged,
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

CUINTERPOSE_API CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  access_fn function = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");
  CUmemAccessDesc (*merged)[CUINTERPOSE_MAX_ACCESS] = NULL;
  size_t* merged_counts = NULL;
  size_t first;
  size_t last;
  size_t index;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (size == 0 || (uint64_t)address > UINT64_MAX - size || descriptors == NULL)
    return function != NULL ? function(address, size, descriptors, count) : cuinterpose_unavailable();
  pthread_mutex_lock(&state_lock);
  if (cuinterpose_ranges_cover(&mappings, (uint64_t)address, (uint64_t)address + size, &first, &last) != 0) {
    /* Partly overlapping a tracked mapping: the driver would apply it, but the
     * record could not say which part, and restore would replay it wrong. */
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_VALUE;
  }
  if (first == last) {
    pthread_mutex_unlock(&state_lock);
    return function != NULL ? function(address, size, descriptors, count) : cuinterpose_unavailable();
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  if (function == NULL) {
    pthread_mutex_unlock(&state_lock);
    return cuinterpose_unavailable();
  }
  /* Compute every merged set first; refuse before the driver is touched if
   * any mapping would overflow. */
  merged = calloc(last - first, sizeof(*merged));
  merged_counts = calloc(last - first, sizeof(*merged_counts));
  if (merged == NULL || merged_counts == NULL) {
    pthread_mutex_unlock(&state_lock);
    free(merged);
    free(merged_counts);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  for (index = first; index < last; index++) {
    if (merge_access(mappings.items[index].value, descriptors, count, merged[index - first],
                     &merged_counts[index - first]) != 0) {
      pthread_mutex_unlock(&state_lock);
      free(merged);
      free(merged_counts);
      return CUDA_ERROR_NOT_SUPPORTED;
    }
  }
  result = function(address, size, descriptors, count);
  for (index = first; index < last; index++) {
    struct mapping* mapping = mappings.items[index].value;
    if (result == CUDA_SUCCESS) {
      memcpy(mapping->access, merged[index - first], merged_counts[index - first] * sizeof(*mapping->access));
      mapping->access_count = merged_counts[index - first];
    } else {
      /* The driver applies descriptors one by one and stops at the first
       * failure, so the recorded set may no longer match the device. */
      mapping->access_unknown = true;
    }
  }
  pthread_mutex_unlock(&state_lock);
  free(merged);
  free(merged_counts);
  return result;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* shareable, CUmemGenericAllocationHandle application, CUmemAllocationHandleType type, unsigned long long flags)
{
  export_fn function = (export_fn)cuinterpose_lookup_real_symbol("cuMemExportToShareableHandle");
  struct handle* handle;
  struct allocation* allocation;
  struct cuinterpose_posix_ticket ticket;
  int ticket_fd = -1;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = find_handle(application);
  if (handle == NULL) {
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(shareable, application, type, flags) : cuinterpose_unavailable();
  }
  /* The driver's own rules for these arguments. */
  if (shareable == NULL || flags != 0 || type != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_VALUE;
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  allocation = handle->allocation;
  adopt_context(allocation);
  if (allocation->creator && !cuinterpose_export_cache_has(allocation->id)) {
    /*
     * The one real export for this allocation in this process. The descriptor
     * goes into the export cache, where the listener serves copies of it to
     * peers; the application only ever sees tickets.
     */
    int real_fd = -1;

    if (function == NULL || allocation->driver == 0) {
      pthread_mutex_unlock(&state_lock);
      return function == NULL ? cuinterpose_unavailable() : CUDA_ERROR_INVALID_HANDLE;
    }
    result = function(&real_fd, allocation->driver, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0);
    if (result != CUDA_SUCCESS) {
      pthread_mutex_unlock(&state_lock);
      return result;
    }
    if (cuinterpose_export_cache_put(allocation->id, allocation->authorization, real_fd) != 0) {
      close(real_fd);
      pthread_mutex_unlock(&state_lock);
      return CUDA_ERROR_OUT_OF_MEMORY;
    }
  }
  memset(&ticket, 0, sizeof(ticket));
  ticket.magic = CUINTERPOSE_POSIX_TICKET_MAGIC;
  ticket.version = CUINTERPOSE_POSIX_TICKET_VERSION;
  ticket.resource_kind = CUINTERPOSE_RESOURCE_UNICAST;
  snprintf(ticket.creator_participant, sizeof(ticket.creator_participant), "%s", allocation->creator_participant);
  memcpy(ticket.allocation_id, allocation->id, sizeof(ticket.allocation_id));
  snprintf(ticket.creator_endpoint, sizeof(ticket.creator_endpoint), "%s", allocation->creator_endpoint);
  memcpy(ticket.authorization, allocation->authorization, sizeof(ticket.authorization));
  if (cuinterpose_posix_create_ticket(&ticket, &ticket_fd) != 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  allocation->shared = true;
  *(int*)shareable = ticket_fd;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  import_fn function = (import_fn)cuinterpose_lookup_real_symbol("cuMemImportFromShareableHandle");
  properties_fn get_properties = (properties_fn)cuinterpose_lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");
  release_fn release = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");
  struct cuinterpose_posix_ticket ticket;
  struct allocation* allocation;
  CUmemGenericAllocationHandle logical;
  CUmemGenericAllocationHandle imported = 0;
  int raw_fd = -1;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return cuinterpose_unavailable();
  if (type != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR ||
      cuinterpose_posix_read_ticket((int)(uintptr_t)os_handle, &ticket) != 0) {
    /*
     * Not one of our tickets: a real descriptor from a process without the
     * shim, or a non-POSIX handle. It goes to the driver untouched, but the
     * result is remembered so the coordinator can refuse to checkpoint while
     * such an import is alive: native restore would give this process a
     * private copy and silently break the sharing.
     */
    result = function(&imported, os_handle, type);
    if (result != CUDA_SUCCESS)
      return result;
    result = passthrough_handle(result, imported, output);
    if (result == CUDA_SUCCESS) {
      pthread_mutex_lock(&state_lock);
      if (cuinterpose_table_put(&raw_imports, cuinterpose_key_u64((uint64_t)imported), (void*)1) == 0)
        atomic_fetch_add(&live_raw_imports, 1);
      pthread_mutex_unlock(&state_lock);
    }
    return result;
  }
  if (ticket.resource_kind != CUINTERPOSE_RESOURCE_UNICAST)
    return CUDA_ERROR_INVALID_HANDLE;
  if (get_properties == NULL || release == NULL)
    return cuinterpose_unavailable();
  pthread_mutex_lock(&state_lock);
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  pthread_mutex_unlock(&state_lock);
  if (request_export(&ticket, &raw_fd, NULL, 0) != 0)
    return CUDA_ERROR_INVALID_HANDLE;
  result = function(&imported, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
  close(raw_fd);
  if (result != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    (void)release(imported);
    return CUDA_ERROR_NOT_READY;
  }
  allocation = find_allocation(ticket.allocation_id);
  if (allocation == NULL) {
    allocation = calloc(1, sizeof(*allocation));
    if (allocation == NULL || get_properties(&allocation->properties, imported) != CUDA_SUCCESS ||
        cuinterpose_table_put(&allocations, cuinterpose_key_bytes(ticket.allocation_id), allocation) != 0) {
      pthread_mutex_unlock(&state_lock);
      (void)release(imported);
      free(allocation);
      return CUDA_ERROR_OUT_OF_MEMORY;
    }
    memcpy(allocation->id, ticket.allocation_id, sizeof(allocation->id));
    memcpy(allocation->authorization, ticket.authorization, sizeof(allocation->authorization));
    snprintf(
        allocation->creator_participant, sizeof(allocation->creator_participant), "%s", ticket.creator_participant);
    snprintf(allocation->creator_endpoint, sizeof(allocation->creator_endpoint), "%s", ticket.creator_endpoint);
    allocation->creator = false;
    allocation->driver = imported;
    cuinterpose_capture_context(&allocation->context);
  } else if (allocation->driver != 0) {
    /* Already imported here: alias the existing driver handle. */
    if (release(imported) != CUDA_SUCCESS) {
      pthread_mutex_unlock(&state_lock);
      return CUDA_ERROR_UNKNOWN;
    }
  } else {
    allocation->driver = imported;
  }
  allocation->shared = true;
  if (add_handle(allocation, &logical) != 0) {
    if (allocation->live_handles == 0 && allocation->live_mappings == 0) {
      cuinterpose_table_remove(&allocations, cuinterpose_key_bytes(allocation->id));
      (void)release(allocation->driver);
      free(allocation);
    }
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle application)
{
  properties_fn function = (properties_fn)cuinterpose_lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");
  struct handle* handle;
  CUmemGenericAllocationHandle driver;
  CUresult result;

  if ((result = cuinterpose_ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = find_handle(application);
  if (handle == NULL) {
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(properties, application) : cuinterpose_unavailable();
  }
  if (current_phase != PHASE_ACTIVE || handle->allocation->driver == 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  driver = handle->allocation->driver;
  pthread_mutex_unlock(&state_lock);
  return function != NULL ? function(properties, driver) : cuinterpose_unavailable();
}
