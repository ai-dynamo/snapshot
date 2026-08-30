/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <cuda.h>
#include <errno.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include "multicast.h"
#include "posix.h"
#include "protocol.h"
#include "symbols.h"
#include "util.h"

#undef cuMulticastBindAddr
#undef cuMulticastBindMem

#define CONTROL_DIR "/snapshot-control"
#define CONTROL_TIMEOUT_SECONDS 30
#define LOGICAL_HANDLE_TAG UINT64_C(0xd94d000000000000)
#define LOGICAL_HANDLE_TAG_MASK UINT64_C(0xffff000000000000)
#define LOGICAL_HANDLE_VALUE_MASK UINT64_C(0x0000ffffffffffff)

CUresult CUDAAPI cuMemRetainAllocationHandle(CUmemGenericAllocationHandle*, void*);
CUresult CUDAAPI cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp*, CUmemGenericAllocationHandle);

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
typedef CUresult(CUDAAPI* context_get_fn)(CUcontext*);
typedef CUresult(CUDAAPI* context_set_fn)(CUcontext);

struct allocation;

struct handle {
  CUmemGenericAllocationHandle logical;
  CUmemGenericAllocationHandle driver;
  bool live;
  struct allocation* allocation;
  struct handle* next;
};

struct mapping {
  CUdeviceptr address;
  size_t size;
  size_t offset;
  CUmemAccessDesc access[CUINTERPOSER_MAX_ACCESS];
  size_t access_count;
  bool mapped;
  bool checkpointed;
  struct allocation* allocation;
  struct mapping* next;
};

struct allocation {
  uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  uint8_t authorization[CUINTERPOSER_TOKEN_SIZE];
  char creator_participant[CUINTERPOSER_ID_SIZE];
  char creator_endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
  size_t size;
  CUmemAllocationProp properties;
  CUcontext context;
  CUmemGenericAllocationHandle carrier;
  bool creator;
  bool shared;
  struct allocation* next;
};

enum phase {
  PHASE_ACTIVE,
  PHASE_CARRIERS,
  PHASE_MULTICAST_DETACHED,
  PHASE_PREPARED,
  PHASE_CREATORS_RESTORED,
  PHASE_UNICAST_RESTORED,
  PHASE_MULTICAST_CREATED,
  PHASE_MULTICAST_IMPORTED,
  PHASE_MULTICAST_JOINED,
  PHASE_FAILED,
};

struct context_scope {
  CUcontext previous;
  bool changed;
};

static pthread_mutex_t state_lock = PTHREAD_MUTEX_INITIALIZER;
static struct allocation* allocations;
static struct handle* handles;
static struct mapping* mappings;
static enum phase current_phase = PHASE_ACTIVE;
static char failure[96];
static char participant_id[CUINTERPOSER_ID_SIZE];
static char control_directory[sizeof(((struct sockaddr_un*)0)->sun_path)];
static char socket_path[sizeof(((struct sockaddr_un*)0)->sun_path)];
static int listener = -1;
static bool endpoint_needs_initialization;
static uint64_t next_logical_handle = 1;
static struct handle* resolve_managed_handle(CUmemGenericAllocationHandle logical);
static struct mapping* find_mapping_at(CUdeviceptr address);
static struct mapping* first_mapping(const struct allocation* allocation);
static struct handle* first_live_handle(const struct allocation* allocation);

static void
release_state_lock(void)
{
  pthread_mutex_unlock(&state_lock);
}

static void
acquire_state_lock(void)
{
  pthread_mutex_lock(&state_lock);
}

static void
set_failure(const char* message)
{
  current_phase = PHASE_FAILED;
  snprintf(failure, sizeof(failure), "%s", message);
}

static void
set_importer_failure(const char* operation, CUresult result)
{
  current_phase = PHASE_FAILED;
  if (result == CUDA_SUCCESS)
    snprintf(failure, sizeof(failure), "importer restore: %s", operation);
  else
    snprintf(failure, sizeof(failure), "importer restore: %s failed: CUresult=%d", operation, (int)result);
}

static struct allocation*
find_allocation(const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  struct allocation* allocation;

  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (memcmp(allocation->id, id, CUINTERPOSER_ALLOCATION_ID_SIZE) == 0)
      return allocation;
  }
  return NULL;
}

static int
multicast_member_from_handle(CUmemGenericAllocationHandle logical, struct cuinterposer_multicast_member* member)
{
  struct handle* handle = resolve_managed_handle(logical);

  if (handle == NULL || handle->driver == 0)
    return -1;
  memcpy(member->id, handle->allocation->id, sizeof(member->id));
  member->handle = handle->driver;
  member->device = handle->allocation->properties.location.id;
  return 0;
}

static int
multicast_member_from_address(CUdeviceptr address, size_t size, struct cuinterposer_multicast_member* member)
{
  struct mapping* mapping = find_mapping_at(address);

  if (mapping == NULL || size > mapping->size || address - mapping->address > mapping->size - size)
    return -1;
  memcpy(member->id, mapping->allocation->id, sizeof(member->id));
  member->address = address;
  member->allocation_offset = mapping->offset + (size_t)(address - mapping->address);
  member->device = mapping->allocation->properties.location.id;
  return 0;
}

static void
multicast_mark_member_shared(const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  struct allocation* allocation = find_allocation(id);

  if (allocation != NULL)
    allocation->shared = true;
}

static int
multicast_member_from_id(const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE], struct cuinterposer_multicast_member* member)
{
  retain_fn retain = (retain_fn)lookup_real_symbol("cuMemRetainAllocationHandle");
  struct allocation* allocation = find_allocation(id);
  struct handle* handle;
  struct mapping* mapping;

  if (allocation == NULL)
    return -1;
  memcpy(member->id, allocation->id, sizeof(member->id));
  member->device = allocation->properties.location.id;
  handle = first_live_handle(allocation);
  if (handle != NULL && handle->driver != 0) {
    member->handle = handle->driver;
    return 0;
  }
  mapping = first_mapping(allocation);
  if (mapping == NULL || retain == NULL || retain(&member->handle, (void*)(uintptr_t)mapping->address) != CUDA_SUCCESS)
    return -1;
  member->temporary_handle = true;
  return 0;
}

static bool
is_logical_handle(CUmemGenericAllocationHandle handle)
{
  return ((uint64_t)handle & LOGICAL_HANDLE_TAG_MASK) == LOGICAL_HANDLE_TAG;
}

static struct handle*
resolve_managed_handle(CUmemGenericAllocationHandle logical)
{
  struct handle* handle;

  if (!is_logical_handle(logical))
    return NULL;
  for (handle = handles; handle != NULL; handle = handle->next) {
    if (handle->live && handle->logical == logical)
      return handle;
  }
  return NULL;
}

static int
allocate_logical_handle(CUmemGenericAllocationHandle* output)
{
  if (next_logical_handle == 0 || next_logical_handle > LOGICAL_HANDLE_VALUE_MASK)
    return -1;
  *output = (CUmemGenericAllocationHandle)(LOGICAL_HANDLE_TAG | next_logical_handle++);
  return 0;
}

static CUresult
transfer_passthrough_handle(CUresult result, CUmemGenericAllocationHandle driver, CUmemGenericAllocationHandle* output)
{
  release_fn release;

  if (result != CUDA_SUCCESS)
    return result;
  if (output == NULL || is_logical_handle(driver)) {
    release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(driver);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  *output = driver;
  return CUDA_SUCCESS;
}

static struct mapping*
find_mapping(CUdeviceptr address, size_t size)
{
  struct mapping* mapping;

  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (mapping->mapped && mapping->address == address && mapping->size == size)
      return mapping;
  }
  return NULL;
}

static struct mapping*
find_mapping_at(CUdeviceptr address)
{
  struct mapping* mapping;

  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (mapping->mapped && address >= mapping->address && address < mapping->address + mapping->size)
      return mapping;
  }
  return NULL;
}

static size_t
live_handle_count(const struct allocation* allocation)
{
  const struct handle* handle;
  size_t count = 0;

  for (handle = handles; handle != NULL; handle = handle->next) {
    if (handle->live && handle->allocation == allocation)
      count++;
  }
  return count;
}

static struct mapping*
first_mapping(const struct allocation* allocation)
{
  struct mapping* mapping;

  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (mapping->allocation == allocation && mapping->mapped)
      return mapping;
  }
  return NULL;
}

static struct handle*
first_live_handle(const struct allocation* allocation)
{
  struct handle* handle;

  for (handle = handles; handle != NULL; handle = handle->next) {
    if (handle->allocation == allocation && handle->live)
      return handle;
  }
  return NULL;
}

static int
add_managed_handle(
    struct allocation* allocation, CUmemGenericAllocationHandle driver, CUmemGenericAllocationHandle* logical)
{
  struct handle* handle = calloc(1, sizeof(*handle));

  if (handle == NULL || allocate_logical_handle(logical) != 0) {
    free(handle);
    return -1;
  }
  handle->logical = *logical;
  handle->driver = driver;
  handle->live = true;
  handle->allocation = allocation;
  handle->next = handles;
  handles = handle;
  return 0;
}

static int
enter_context(CUcontext context, struct context_scope* scope)
{
  context_get_fn get_current = (context_get_fn)lookup_real_symbol("cuCtxGetCurrent");
  context_set_fn set_current = (context_set_fn)lookup_real_symbol("cuCtxSetCurrent");

  memset(scope, 0, sizeof(*scope));
  if (get_current == NULL || set_current == NULL || get_current(&scope->previous) != CUDA_SUCCESS)
    return -1;
  if (scope->previous != context) {
    if (set_current(context) != CUDA_SUCCESS)
      return -1;
    scope->changed = true;
  }
  return 0;
}

static int
leave_context(const struct context_scope* scope)
{
  context_set_fn set_current = (context_set_fn)lookup_real_symbol("cuCtxSetCurrent");

  return !scope->changed || (set_current != NULL && set_current(scope->previous) == CUDA_SUCCESS) ? 0 : -1;
}

static int
current_context(CUcontext* context)
{
  context_get_fn get_current = (context_get_fn)lookup_real_symbol("cuCtxGetCurrent");

  return get_current != NULL && get_current(context) == CUDA_SUCCESS && *context != NULL ? 0 : -1;
}

static CUresult
export_raw(struct allocation* allocation, int* output)
{
  export_fn export_handle = (export_fn)lookup_real_symbol("cuMemExportToShareableHandle");
  retain_fn retain = (retain_fn)lookup_real_symbol("cuMemRetainAllocationHandle");
  release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
  struct context_scope scope;
  struct handle* handle;
  struct mapping* mapping;
  CUmemGenericAllocationHandle temporary = 0;
  CUresult result;

  *output = -1;
  if (!allocation->creator || export_handle == NULL || enter_context(allocation->context, &scope) != 0)
    return CUDA_ERROR_INVALID_HANDLE;
  handle = first_live_handle(allocation);
  if (allocation->carrier != 0)
    temporary = allocation->carrier;
  else if (handle != NULL)
    temporary = handle->driver;
  else if ((mapping = first_mapping(allocation)) != NULL && retain != NULL) {
    result = retain(&temporary, (void*)(uintptr_t)mapping->address);
    if (result != CUDA_SUCCESS)
      goto done;
  } else {
    result = CUDA_ERROR_INVALID_HANDLE;
    goto done;
  }
  result = export_handle(output, temporary, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0);
  if (temporary != allocation->carrier && (handle == NULL || temporary != handle->driver) && release != NULL)
    (void)release(temporary);
done:
  if (leave_context(&scope) != 0) {
    if (*output >= 0) {
      close(*output);
      *output = -1;
    }
    return CUDA_ERROR_UNKNOWN;
  }
  return result;
}

static int
request_export(const struct cuinterposer_posix_ticket* ticket, int* output, char* error, size_t error_size)
{
  int result = -1;

  *output = -1;
  if (error != NULL && error_size != 0)
    error[0] = '\0';
  if (strcmp(ticket->creator_participant, participant_id) == 0) {
    CUresult export_result = CUDA_ERROR_INVALID_HANDLE;

    pthread_mutex_lock(&state_lock);
    if (ticket->resource_kind == CUINTERPOSER_RESOURCE_MULTICAST) {
      export_result = cuinterposer_multicast_export_raw(ticket->allocation_id, ticket->authorization, output);
    } else {
      struct allocation* allocation = find_allocation(ticket->allocation_id);
      if (allocation != NULL &&
          memcmp(allocation->authorization, ticket->authorization, sizeof(allocation->authorization)) == 0)
        export_result = export_raw(allocation, output);
    }
    if (export_result == CUDA_ERROR_INVALID_HANDLE) {
      if (error != NULL && error_size != 0)
        snprintf(error, error_size, "%s", "creator resource is unavailable");
    } else if (export_result != CUDA_SUCCESS) {
      if (error != NULL && error_size != 0)
        snprintf(error, error_size, "creator export failed: CUresult=%d", (int)export_result);
    } else {
      result = 0;
    }
    pthread_mutex_unlock(&state_lock);
    return result;
  }
  return cuinterposer_posix_request_export(ticket, output, error, error_size);
}

static struct cuinterposer_record*
inspect_records(uint32_t* count)
{
  struct cuinterposer_record* records;
  struct cuinterposer_record* record;
  struct allocation* allocation;
  struct mapping* mapping;
  size_t total = 0;
  size_t index;

  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (allocation->shared && (live_handle_count(allocation) != 0 || first_mapping(allocation) != NULL))
      total++;
  }
  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (mapping->allocation->shared && mapping->mapped)
      total++;
  }
  total += cuinterposer_multicast_record_count();
  if (total > CUINTERPOSER_MAX_RECORDS)
    return NULL;
  records = calloc(total == 0 ? 1 : total, sizeof(*records));
  if (records == NULL)
    return NULL;
  record = records;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    size_t handles_count = live_handle_count(allocation);
    if (!allocation->shared || (handles_count == 0 && first_mapping(allocation) == NULL))
      continue;
    record->kind = CUINTERPOSER_ALLOCATION;
    record->flags = allocation->creator ? CUINTERPOSER_CREATOR : 0;
    if (handles_count != 0)
      record->flags |= CUINTERPOSER_APPLICATION_HANDLE_LIVE;
    memcpy(record->allocation_id, allocation->id, sizeof(record->allocation_id));
    record->allocation_size = allocation->size;
    record->allocation_type = allocation->properties.type;
    record->requested_handle_types = allocation->properties.requestedHandleTypes;
    record->allocation_location_type = allocation->properties.location.type;
    record->allocation_location_id = allocation->properties.location.id;
    record->application_handle_count = (uint32_t)handles_count;
    record++;
  }
  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (!mapping->allocation->shared || !mapping->mapped)
      continue;
    record->kind = CUINTERPOSER_MAPPING;
    record->flags = mapping->allocation->creator ? CUINTERPOSER_CREATOR : 0;
    memcpy(record->allocation_id, mapping->allocation->id, sizeof(record->allocation_id));
    record->address = mapping->address;
    record->size = mapping->size;
    record->offset = mapping->offset;
    record->access_count = (uint32_t)mapping->access_count;
    for (index = 0; index < mapping->access_count; index++) {
      record->access[index].location_type = mapping->access[index].location.type;
      record->access[index].location_id = mapping->access[index].location.id;
      record->access[index].flags = mapping->access[index].flags;
    }
    record++;
  }
  if (cuinterposer_multicast_write_records(record, total - (size_t)(record - records)) != 0) {
    free(records);
    return NULL;
  }
  *count = (uint32_t)total;
  return records;
}

static bool
driver_handle_used(const struct handle* except, CUmemGenericAllocationHandle driver)
{
  const struct handle* handle;

  for (handle = handles; handle != NULL; handle = handle->next) {
    if (handle != except && handle->live && handle->driver == driver)
      return true;
  }
  return false;
}

static int
create_checkpoint_carriers(void)
{
  retain_fn retain = (retain_fn)lookup_real_symbol("cuMemRetainAllocationHandle");
  struct allocation* allocation;
  struct context_scope scope;

  if (current_phase != PHASE_ACTIVE || retain == NULL)
    return -1;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    struct handle* carrier_handle;
    struct mapping* carrier_mapping;
    CUresult result;

    if (!allocation->shared || !allocation->creator ||
        (live_handle_count(allocation) == 0 && first_mapping(allocation) == NULL))
      continue;
    carrier_handle = first_live_handle(allocation);
    carrier_mapping = first_mapping(allocation);
    if (carrier_handle != NULL) {
      allocation->carrier = carrier_handle->driver;
      continue;
    }
    if (carrier_mapping == NULL || enter_context(allocation->context, &scope) != 0)
      goto failed;
    result = retain(&allocation->carrier, (void*)(uintptr_t)carrier_mapping->address);
    if (leave_context(&scope) != 0 || result != CUDA_SUCCESS)
      goto failed;
  }
  current_phase = PHASE_CARRIERS;
  return 0;
failed:
  set_failure("cannot create cuinterposer checkpoint carrier");
  return -1;
}

static int
prepare_topology(void)
{
  release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
  unmap_fn unmap = (unmap_fn)lookup_real_symbol("cuMemUnmap");
  struct allocation* allocation;
  struct context_scope scope;
  struct handle* handle;
  struct mapping* mapping;

  if (current_phase != PHASE_MULTICAST_DETACHED || release == NULL || unmap == NULL)
    return -1;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (allocation->shared && allocation->creator &&
        (live_handle_count(allocation) != 0 || first_mapping(allocation) != NULL) && allocation->carrier == 0)
      goto failed;
  }
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (!allocation->shared ||
        (live_handle_count(allocation) == 0 && first_mapping(allocation) == NULL && allocation->carrier == 0))
      continue;
    if (enter_context(allocation->context, &scope) != 0)
      goto failed;
    for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
      if (mapping->allocation == allocation && mapping->mapped) {
        if (unmap(mapping->address, mapping->size) != CUDA_SUCCESS) {
          (void)leave_context(&scope);
          goto failed;
        }
        mapping->mapped = false;
        mapping->checkpointed = true;
      }
    }
    for (handle = handles; handle != NULL; handle = handle->next) {
      CUmemGenericAllocationHandle old;

      if (!handle->live || handle->allocation != allocation)
        continue;
      old = handle->driver;
      if (allocation->creator && old == allocation->carrier) {
        handle->driver = allocation->carrier;
        continue;
      }
      if (!driver_handle_used(handle, old) && release(old) != CUDA_SUCCESS) {
        (void)leave_context(&scope);
        goto failed;
      }
      handle->driver = allocation->creator ? allocation->carrier : 0;
    }
    if (leave_context(&scope) != 0)
      goto failed;
  }
  current_phase = PHASE_PREPARED;
  return 0;
failed:
  set_failure("cannot prepare cuinterposer topology");
  return -1;
}

static CUresult
restore_mappings(struct allocation* allocation, CUmemGenericAllocationHandle handle, const char** operation)
{
  map_fn map = (map_fn)lookup_real_symbol("cuMemMap");
  access_fn set_access = (access_fn)lookup_real_symbol("cuMemSetAccess");
  struct mapping* mapping;
  CUresult result;

  if (map == NULL || set_access == NULL) {
    *operation = "mapping symbols are unavailable";
    return CUDA_ERROR_NOT_INITIALIZED;
  }
  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    if (!mapping->allocation->shared || mapping->allocation != allocation || !mapping->checkpointed)
      continue;
    result = map(mapping->address, mapping->size, mapping->offset, handle, 0);
    if (result != CUDA_SUCCESS) {
      *operation = "cuMemMap";
      return result;
    }
    mapping->mapped = true;
    if (mapping->access_count != 0) {
      result = set_access(mapping->address, mapping->size, mapping->access, mapping->access_count);
      if (result != CUDA_SUCCESS) {
        *operation = "cuMemSetAccess";
        return result;
      }
    }
    mapping->checkpointed = false;
  }
  return CUDA_SUCCESS;
}

static int
restore_creators(void)
{
  release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
  struct allocation* allocation;
  struct context_scope scope;
  struct handle* handle;
  const char* mapping_operation;

  if ((current_phase != PHASE_PREPARED && current_phase != PHASE_FAILED) || release == NULL)
    return -1;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (!allocation->shared || !allocation->creator || allocation->carrier == 0)
      continue;
    if (enter_context(allocation->context, &scope) != 0)
      goto failed;
    if (restore_mappings(allocation, allocation->carrier, &mapping_operation) != CUDA_SUCCESS) {
      (void)leave_context(&scope);
      goto failed;
    }
    for (handle = handles; handle != NULL; handle = handle->next) {
      if (handle->live && handle->allocation == allocation)
        handle->driver = allocation->carrier;
    }
    if (live_handle_count(allocation) == 0) {
      if (release(allocation->carrier) != CUDA_SUCCESS) {
        (void)leave_context(&scope);
        goto failed;
      }
      allocation->carrier = 0;
    }
    if (leave_context(&scope) != 0)
      goto failed;
  }
  current_phase = PHASE_CREATORS_RESTORED;
  failure[0] = '\0';
  return 0;
failed:
  set_failure("cannot restore creator cuinterposer topology");
  return -1;
}

static int
restore_importers(void)
{
  import_fn import_handle = (import_fn)lookup_real_symbol("cuMemImportFromShareableHandle");
  release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
  struct allocation* allocation;
  struct context_scope scope;
  struct handle* handle;

  if (current_phase != PHASE_CREATORS_RESTORED || import_handle == NULL || release == NULL) {
    snprintf(failure, sizeof(failure), "%s", "importer restore: phase or symbols are not ready");
    return -1;
  }
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    struct cuinterposer_posix_ticket ticket;
    CUmemGenericAllocationHandle imported = 0;
    CUresult cuda_result;
    char export_error[sizeof(failure)];
    int export_result;
    int raw_fd = -1;
    bool needed = false;
    const char* mapping_operation;

    if (!allocation->shared || allocation->creator)
      continue;
    for (handle = handles; handle != NULL; handle = handle->next) {
      if (handle->live && handle->allocation == allocation) {
        needed = true;
        break;
      }
    }
    if (!needed) {
      struct mapping* mapping;
      for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
        if (mapping->allocation == allocation && mapping->checkpointed) {
          needed = true;
          break;
        }
      }
    }
    if (!needed)
      continue;
    memset(&ticket, 0, sizeof(ticket));
    ticket.magic = CUINTERPOSER_POSIX_TICKET_MAGIC;
    ticket.version = CUINTERPOSER_POSIX_TICKET_VERSION;
    ticket.resource_kind = CUINTERPOSER_RESOURCE_UNICAST;
    snprintf(
        ticket.creator_participant, sizeof(ticket.creator_participant), "%s", allocation->creator_participant);
    memcpy(ticket.allocation_id, allocation->id, sizeof(ticket.allocation_id));
    snprintf(ticket.creator_endpoint, sizeof(ticket.creator_endpoint), "%s", allocation->creator_endpoint);
    memcpy(ticket.authorization, allocation->authorization, sizeof(ticket.authorization));
    if (enter_context(allocation->context, &scope) != 0) {
      set_importer_failure("enter context", CUDA_SUCCESS);
      return -1;
    }
    pthread_mutex_unlock(&state_lock);
    export_result = request_export(&ticket, &raw_fd, export_error, sizeof(export_error));
    pthread_mutex_lock(&state_lock);
    if (export_result != 0) {
      (void)leave_context(&scope);
      current_phase = PHASE_FAILED;
      snprintf(
          failure, sizeof(failure), "importer restore: creator export: %.61s",
          export_error[0] != '\0' ? export_error : "request failed");
      return -1;
    }
    cuda_result = import_handle(&imported, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
    if (cuda_result != CUDA_SUCCESS) {
      close(raw_fd);
      (void)leave_context(&scope);
      set_importer_failure("cuMemImportFromShareableHandle", cuda_result);
      return -1;
    }
    if (close(raw_fd) != 0) {
      (void)release(imported);
      (void)leave_context(&scope);
      set_importer_failure("raw FD close", CUDA_SUCCESS);
      return -1;
    }
    cuda_result = restore_mappings(allocation, imported, &mapping_operation);
    if (cuda_result != CUDA_SUCCESS) {
      (void)release(imported);
      (void)leave_context(&scope);
      set_importer_failure(mapping_operation, cuda_result);
      return -1;
    }
    for (handle = handles; handle != NULL; handle = handle->next) {
      if (handle->live && handle->allocation == allocation)
        handle->driver = imported;
    }
    if (live_handle_count(allocation) == 0) {
      cuda_result = release(imported);
      if (cuda_result != CUDA_SUCCESS) {
        (void)leave_context(&scope);
        set_importer_failure("cuMemRelease imported handle", cuda_result);
        return -1;
      }
    }
    if (leave_context(&scope) != 0) {
      set_importer_failure("leave context", CUDA_SUCCESS);
      return -1;
    }
  }
  current_phase = PHASE_UNICAST_RESTORED;
  failure[0] = '\0';
  return 0;
}

static void
serve(int client)
{
  struct cuinterposer_header request;
  struct cuinterposer_header response;
  struct cuinterposer_record* records = NULL;
  int passed_fd = -1;
  int exported_fd = -1;

  if (receive_header(client, &request, &passed_fd) != 0)
    goto done;
  /* Reply with the same operation and this process's participant id. */
  memset(&response, 0, sizeof(response));
  response.magic = CUINTERPOSER_MAGIC;
  response.version = CUINTERPOSER_VERSION;
  response.operation = request.operation;
  snprintf(response.participant_id, sizeof(response.participant_id), "%s", participant_id);
  /* Reject ancillary FDs and requests that are not a well-formed control header for this process. */
  if (passed_fd >= 0 || !header_strings_terminated(&request) || request.magic != CUINTERPOSER_MAGIC ||
      request.version != CUINTERPOSER_VERSION || request.status != 0 || request.count != 0 ||
      request.payload_size != 0 ||
      !((request.operation == CUINTERPOSER_IDENTIFY && request.participant_id[0] == '\0') ||
        strcmp(request.participant_id, participant_id) == 0)) {
    header_error(&response, "invalid cuinterposer control request");
    (void)send_header(client, &response, -1);
    goto done;
  }
  pthread_mutex_lock(&state_lock);
  switch (request.operation) {
    case CUINTERPOSER_IDENTIFY:
      if (current_phase == PHASE_FAILED)
        header_error(&response, failure);
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_INSPECT:
      if (current_phase != PHASE_ACTIVE) {
        header_error(&response, "cuinterposer topology is not active");
        (void)send_header(client, &response, -1);
        break;
      }
      records = inspect_records(&response.count);
      if (records == NULL) {
        header_error(&response, "cannot inspect cuinterposer topology");
        (void)send_header(client, &response, -1);
        break;
      }
      response.payload_size = (uint64_t)response.count * sizeof(struct cuinterposer_record);
      if (send_header(client, &response, -1) == 0 && response.payload_size != 0)
        (void)write_all(client, records, (size_t)response.payload_size);
      break;
    case CUINTERPOSER_PREPARE:
      if (prepare_topology() != 0)
        header_error(&response, failure);
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_PREPARE_MULTICAST:
      /* Keep a unicast carrier, then tear down multicast before any rank unmaps UC. */
      if (create_checkpoint_carriers() != 0) {
        header_error(&response, failure);
      } else if (cuinterposer_multicast_prepare() != 0) {
        set_failure(cuinterposer_multicast_error());
        header_error(&response, failure);
      } else {
        current_phase = PHASE_MULTICAST_DETACHED;
      }
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_CREATORS:
      if (restore_creators() != 0)
        header_error(&response, failure);
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_IMPORTERS:
      if (restore_importers() != 0)
        header_error(&response, failure);
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_MULTICAST_CREATORS:
      if (current_phase != PHASE_UNICAST_RESTORED || cuinterposer_multicast_restore_creators() != 0) {
        set_failure(cuinterposer_multicast_error());
        header_error(&response, failure);
      } else {
        current_phase = PHASE_MULTICAST_CREATED;
      }
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_MULTICAST_IMPORTERS:
      if (current_phase != PHASE_MULTICAST_CREATED || cuinterposer_multicast_restore_importers() != 0) {
        set_failure(cuinterposer_multicast_error());
        header_error(&response, failure);
      } else {
        current_phase = PHASE_MULTICAST_IMPORTED;
      }
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_MULTICAST_DEVICES:
      if (current_phase != PHASE_MULTICAST_IMPORTED || cuinterposer_multicast_restore_devices() != 0) {
        set_failure(cuinterposer_multicast_error());
        header_error(&response, failure);
      } else {
        current_phase = PHASE_MULTICAST_JOINED;
      }
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_RESTORE_MULTICAST:
      if (current_phase != PHASE_MULTICAST_JOINED || cuinterposer_multicast_restore_topology() != 0) {
        set_failure(cuinterposer_multicast_error());
        header_error(&response, failure);
      } else {
        current_phase = PHASE_ACTIVE;
        failure[0] = '\0';
      }
      (void)send_header(client, &response, -1);
      break;
    case CUINTERPOSER_EXPORT: {
      CUresult export_result;
      if (strcmp(request.participant_id, participant_id) != 0 ||
          (request.resource_kind != CUINTERPOSER_RESOURCE_UNICAST &&
           request.resource_kind != CUINTERPOSER_RESOURCE_MULTICAST) ||
          (current_phase != PHASE_ACTIVE && current_phase != PHASE_CREATORS_RESTORED &&
           current_phase != PHASE_UNICAST_RESTORED && current_phase != PHASE_MULTICAST_CREATED &&
           current_phase != PHASE_MULTICAST_IMPORTED && current_phase != PHASE_MULTICAST_JOINED)) {
        header_error(&response, "creator resource is unavailable");
        (void)send_header(client, &response, -1);
        break;
      }
      response.resource_kind = request.resource_kind;
      if (request.resource_kind == CUINTERPOSER_RESOURCE_MULTICAST) {
        export_result = cuinterposer_multicast_export_raw(request.allocation_id, request.authorization, &exported_fd);
      } else {
        struct allocation* allocation = find_allocation(request.allocation_id);
        if (allocation == NULL || !allocation->creator ||
            memcmp(allocation->authorization, request.authorization, sizeof(request.authorization)) != 0) {
          header_error(&response, "creator allocation is unavailable");
          (void)send_header(client, &response, -1);
          break;
        }
        export_result = export_raw(allocation, &exported_fd);
      }
      if (export_result != CUDA_SUCCESS) {
        char message[sizeof(response.message)];
        snprintf(message, sizeof(message), "creator export failed: CUresult=%d", (int)export_result);
        header_error(&response, message);
        (void)send_header(client, &response, -1);
        break;
      }
      memcpy(response.allocation_id, request.allocation_id, sizeof(response.allocation_id));
      (void)send_header(client, &response, exported_fd);
      break;
    }
    default:
      header_error(&response, "unknown cuinterposer control operation");
      (void)send_header(client, &response, -1);
      break;
  }
  pthread_mutex_unlock(&state_lock);
done:
  if (passed_fd >= 0)
    close(passed_fd);
  if (exported_fd >= 0)
    close(exported_fd);
  free(records);
}

static void*
control_agent(void* unused)
{
  (void)unused;
  for (;;) {
    int client = accept4(listener, NULL, NULL, SOCK_CLOEXEC);
    if (client < 0) {
      if (errno == EINTR)
        continue;
      return NULL;
    }
    if (set_socket_timeouts(client, CONTROL_TIMEOUT_SECONDS) == 0)
      serve(client);
    close(client);
  }
}

static int
start_control_endpoint(void)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  pthread_t thread;

  /* Bind the per-process control socket under the snapshot-control directory. */
  {
    int count =
        snprintf(socket_path, sizeof(socket_path), "%s/%s%ld.sock", control_directory, CUINTERPOSER_SOCKET_PREFIX, (long)getpid());
    if (count < 0 || (size_t)count >= sizeof(socket_path)) {
      socket_path[0] = '\0';
      return -1;
    }
  }
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", socket_path);
  listener = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (listener >= 0)
    unlink(socket_path);
  if (listener < 0 || bind(listener, (const struct sockaddr*)&address, sizeof(address)) != 0 ||
      listen(listener, 8) != 0 || pthread_create(&thread, NULL, control_agent, NULL) != 0) {
    if (listener >= 0)
      close(listener);
    listener = -1;
    unlink(socket_path);
    socket_path[0] = '\0';
    return -1;
  }
  pthread_detach(thread);
  return 0;
}

static void
fork_prepare(void)
{
  pthread_mutex_lock(&state_lock);
}

static void
fork_parent(void)
{
  pthread_mutex_unlock(&state_lock);
}

static void
fork_child(void)
{
  if (listener >= 0)
    close(listener);
  listener = -1;
  participant_id[0] = '\0';
  socket_path[0] = '\0';
  endpoint_needs_initialization = true;
  allocations = NULL;
  handles = NULL;
  mappings = NULL;
  cuinterposer_multicast_reset();
  next_logical_handle = 1;
  current_phase = PHASE_ACTIVE;
  failure[0] = '\0';
  pthread_mutex_unlock(&state_lock);
}

static CUresult
ensure_process_endpoint(void)
{
  CUresult result = CUDA_SUCCESS;

  pthread_mutex_lock(&state_lock);
  if (endpoint_needs_initialization) {
    if (current_phase == PHASE_FAILED) {
      result = CUDA_ERROR_NOT_INITIALIZED;
    } else if (random_id(participant_id) != 0 || start_control_endpoint() != 0) {
      set_failure("cannot start forked cuinterposer control endpoint");
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
  const struct cuinterposer_multicast_callbacks multicast_callbacks = {
      .allocate_logical_handle = allocate_logical_handle,
      .member_from_handle = multicast_member_from_handle,
      .member_from_address = multicast_member_from_address,
      .member_from_id = multicast_member_from_id,
      .mark_member_shared = multicast_mark_member_shared,
      .release_state_lock = release_state_lock,
      .acquire_state_lock = acquire_state_lock,
  };
  const char* control;
  const char* configured_participant;

  configured_participant = getenv("DYN_SNAPSHOT_PARTICIPANT_ID");
  if (configured_participant != NULL && !is_lower_hex_id(configured_participant)) {
    set_failure("invalid cuinterposer participant identity");
    return;
  }
  if (configured_participant != NULL)
    snprintf(participant_id, sizeof(participant_id), "%s", configured_participant);
  else if (random_id(participant_id) != 0) {
    set_failure("cannot create cuinterposer participant identity");
    return;
  }
  control = getenv("DYN_SNAPSHOT_CONTROL_DIR");
  if (control == NULL || control[0] == '\0')
    control = CONTROL_DIR;
  if (control[0] != '/' || strlen(control) >= sizeof(control_directory) ||
      snprintf(control_directory, sizeof(control_directory), "%s", control) >= (int)sizeof(control_directory)) {
    set_failure("invalid cuinterposer control directory");
    return;
  }
  if (pthread_atfork(fork_prepare, fork_parent, fork_child) != 0) {
    set_failure("cannot register cuinterposer fork handlers");
    return;
  }
  if (start_control_endpoint() != 0) {
    set_failure("cannot start cuinterposer control endpoint");
    return;
  }
  cuinterposer_multicast_initialize(&multicast_callbacks, participant_id, socket_path);
}

__attribute__((destructor)) static void
finalize(void)
{
  if (listener >= 0)
    close(listener);
  if (socket_path[0] != '\0')
    unlink(socket_path);
}

CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  create_fn function = (create_fn)lookup_real_symbol("cuMemCreate");
  struct allocation* allocation;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if (properties == NULL || properties->requestedHandleTypes == 0) {
    result = function != NULL ? function(&driver, size, properties, flags) : unavailable();
    return transfer_passthrough_handle(result, driver, output);
  }
  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (properties->requestedHandleTypes != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
    return CUDA_ERROR_NOT_SUPPORTED;
  if (function == NULL)
    return unavailable();
  result = function(&driver, size, properties, flags);
  if (result != CUDA_SUCCESS)
    return result;
  allocation = calloc(1, sizeof(*allocation));
  pthread_mutex_lock(&state_lock);
  if (allocation == NULL || current_phase != PHASE_ACTIVE ||
      random_bytes(allocation->id, sizeof(allocation->id)) != 0 ||
      random_bytes(allocation->authorization, sizeof(allocation->authorization)) != 0 ||
      add_managed_handle(allocation, driver, &logical) != 0) {
    release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(driver);
    free(allocation);
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  allocation->size = size;
  allocation->properties = *properties;
  allocation->creator = true;
  snprintf(allocation->creator_participant, sizeof(allocation->creator_participant), "%s", participant_id);
  snprintf(allocation->creator_endpoint, sizeof(allocation->creator_endpoint), "%s", socket_path);
  allocation->next = allocations;
  allocations = allocation;
  *output = logical;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle application)
{
  release_fn function = (release_fn)lookup_real_symbol("cuMemRelease");
  struct handle* handle;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = resolve_managed_handle(application);
  if (handle == NULL) {
    if (cuinterposer_multicast_is_handle(application)) {
      result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_release(application) : CUDA_ERROR_NOT_READY;
      pthread_mutex_unlock(&state_lock);
      return result;
    }
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(application) : unavailable();
  }
  if (current_phase != PHASE_ACTIVE || handle->driver == 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  result = CUDA_SUCCESS;
  if (!driver_handle_used(handle, handle->driver))
    result = function != NULL ? function(handle->driver) : unavailable();
  if (result == CUDA_SUCCESS)
    handle->live = false;
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  retain_fn function = (retain_fn)lookup_real_symbol("cuMemRetainAllocationHandle");
  struct mapping* mapping;
  CUmemGenericAllocationHandle driver = 0;
  CUmemGenericAllocationHandle logical;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (function == NULL)
    return unavailable();
  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_retain(output, address) : CUDA_ERROR_NOT_READY;
  if (result != CUDA_ERROR_INVALID_VALUE) {
    pthread_mutex_unlock(&state_lock);
    return result;
  }
  pthread_mutex_unlock(&state_lock);
  result = function(&driver, address);
  if (result != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  mapping = find_mapping_at((CUdeviceptr)(uintptr_t)address);
  if (mapping == NULL) {
    result = transfer_passthrough_handle(result, driver, output);
  } else if (add_managed_handle(mapping->allocation, driver, &logical) != 0) {
    release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(driver);
    result = CUDA_ERROR_OUT_OF_MEMORY;
  } else {
    *output = logical;
  }
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMemMap(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle application, unsigned long long flags)
{
  map_fn function = (map_fn)lookup_real_symbol("cuMemMap");
  struct mapping* mapping;
  struct handle* handle;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = resolve_managed_handle(application);
  if (handle == NULL) {
    if (cuinterposer_multicast_is_handle(application)) {
      result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_map(address, size, offset, application, flags)
                                             : CUDA_ERROR_NOT_READY;
      pthread_mutex_unlock(&state_lock);
      return result;
    }
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(address, size, offset, application, flags) : unavailable();
  }
  if (current_phase != PHASE_ACTIVE || handle->driver == 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  result = function != NULL ? function(address, size, offset, handle->driver, flags) : unavailable();
  if (result == CUDA_SUCCESS) {
    mapping = calloc(1, sizeof(*mapping));
    if (mapping == NULL) {
      unmap_fn unmap = (unmap_fn)lookup_real_symbol("cuMemUnmap");
      if (unmap != NULL)
        (void)unmap(address, size);
      result = CUDA_ERROR_OUT_OF_MEMORY;
    } else {
      mapping->address = address;
      mapping->size = size;
      mapping->offset = offset;
      mapping->mapped = true;
      mapping->allocation = handle->allocation;
      mapping->next = mappings;
      mappings = mapping;
    }
  }
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  unmap_fn function = (unmap_fn)lookup_real_symbol("cuMemUnmap");
  struct mapping* mapping;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  mapping = find_mapping(address, size);
  if (mapping == NULL) {
    if (cuinterposer_multicast_has_mapping(address, size)) {
      result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_unmap(address, size) : CUDA_ERROR_NOT_READY;
      pthread_mutex_unlock(&state_lock);
      return result;
    }
    pthread_mutex_unlock(&state_lock);
    return function != NULL ? function(address, size) : unavailable();
  }
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_READY;
  }
  result = function != NULL ? function(address, size) : unavailable();
  if (result == CUDA_SUCCESS)
    mapping->mapped = false;
  pthread_mutex_unlock(&state_lock);
  return result;
}

/*
 * Record the access descriptors on every tracked mapping that `[address,size)`
 * covers.
 *
 * Restore replays access only when `mapping->access_count != 0` (see
 * restore_mappings). Matching solely on an exact (address, size) pair meant any
 * caller that mapped at one granularity and set access at another -- which
 * PyTorch's caching allocator routinely does, and which is legal CUDA -- left
 * `access_count == 0`. Such a mapping was restored mapped but with NO device
 * access, so the VA resolved and the first kernel to touch it died with
 * CUDA_ERROR_ILLEGAL_ADDRESS. Observed on an 8-rank GLM-5.2 restore: 192 of 336
 * recorded mappings had access_count == 0.
 *
 * Returns false if the range partially overlaps a tracked mapping, i.e. the
 * request cannot be represented per-mapping. The caller then fails closed
 * rather than passing it through and silently losing the access on restore.
 *
 * When `descriptors` is NULL nothing is mutated; the walk only classifies the
 * range. `*matched` receives the number of tracked mappings fully covered.
 */
static bool
record_access_over_range(
    CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count, size_t* matched)
{
  struct mapping* mapping;
  CUdeviceptr end = address + size;

  *matched = 0;
  for (mapping = mappings; mapping != NULL; mapping = mapping->next) {
    CUdeviceptr mapping_end;

    if (!mapping->mapped)
      continue;
    mapping_end = mapping->address + mapping->size;
    if (mapping_end <= address || mapping->address >= end)
      continue; /* disjoint */
    if (mapping->address < address || mapping_end > end)
      return false; /* partial overlap: not representable per-mapping */
    if (descriptors != NULL) {
      memcpy(mapping->access, descriptors, count * sizeof(*descriptors));
      mapping->access_count = count;
    }
    (*matched)++;
  }
  return true;
}

CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  access_fn function = (access_fn)lookup_real_symbol("cuMemSetAccess");
  size_t matched;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);

  /* Multicast ranges keep their own bookkeeping. */
  if (find_mapping(address, size) == NULL && cuinterposer_multicast_has_mapping(address, size)) {
    result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_set_access(address, size, descriptors, count)
                                           : CUDA_ERROR_NOT_READY;
    pthread_mutex_unlock(&state_lock);
    return result;
  }

  /*
   * Classify first (NULL descriptors => no mutation) so a range that touches
   * nothing we track stays a pure passthrough, and so an unrepresentable range
   * is rejected before the device is modified.
   */
  if (!record_access_over_range(address, size, NULL, 0, &matched)) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_SUPPORTED;
  }
  if (matched == 0) {
    pthread_mutex_unlock(&state_lock);
    return function != NULL ? function(address, size, descriptors, count) : unavailable();
  }
  if (current_phase != PHASE_ACTIVE || count > CUINTERPOSER_MAX_ACCESS) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_NOT_SUPPORTED;
  }
  result = function != NULL ? function(address, size, descriptors, count) : unavailable();
  if (result == CUDA_SUCCESS)
    (void)record_access_over_range(address, size, descriptors, count, &matched);
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* shareable, CUmemGenericAllocationHandle application, CUmemAllocationHandleType type, unsigned long long flags)
{
  export_fn function = (export_fn)lookup_real_symbol("cuMemExportToShareableHandle");
  struct handle* handle;
  CUcontext context;
  CUresult result;
  int ticket_fd = -1;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (type != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
    return CUDA_ERROR_NOT_SUPPORTED;
  pthread_mutex_lock(&state_lock);
  handle = resolve_managed_handle(application);
  if (handle == NULL) {
    if (cuinterposer_multicast_is_handle(application)) {
      result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_export(shareable, application, type, flags)
                                             : CUDA_ERROR_NOT_READY;
      pthread_mutex_unlock(&state_lock);
      return result;
    }
    pthread_mutex_unlock(&state_lock);
    if (is_logical_handle(application))
      return CUDA_ERROR_INVALID_HANDLE;
    return function != NULL ? function(shareable, application, type, flags) : unavailable();
  }
  if (!handle->allocation->creator || current_phase != PHASE_ACTIVE || current_context(&context) != 0) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  /* Issue a memfd ticket the importer uses to ask this creator for the POSIX fd. */
  {
    struct cuinterposer_posix_ticket ticket;
    memset(&ticket, 0, sizeof(ticket));
    ticket.magic = CUINTERPOSER_POSIX_TICKET_MAGIC;
    ticket.version = CUINTERPOSER_POSIX_TICKET_VERSION;
    ticket.resource_kind = CUINTERPOSER_RESOURCE_UNICAST;
    snprintf(ticket.creator_participant, sizeof(ticket.creator_participant), "%s", handle->allocation->creator_participant);
    memcpy(ticket.allocation_id, handle->allocation->id, sizeof(ticket.allocation_id));
    snprintf(ticket.creator_endpoint, sizeof(ticket.creator_endpoint), "%s", handle->allocation->creator_endpoint);
    memcpy(ticket.authorization, handle->allocation->authorization, sizeof(ticket.authorization));
    if (cuinterposer_posix_create_ticket(&ticket, &ticket_fd) != 0) {
      pthread_mutex_unlock(&state_lock);
      return CUDA_ERROR_INVALID_HANDLE;
    }
  }
  handle->allocation->context = context;
  handle->allocation->shared = true;
  *(int*)shareable = ticket_fd;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  import_fn function = (import_fn)lookup_real_symbol("cuMemImportFromShareableHandle");
  properties_fn get_properties;
  struct cuinterposer_posix_ticket ticket;
  struct allocation* allocation;
  CUmemGenericAllocationHandle logical;
  CUmemGenericAllocationHandle imported = 0;
  int raw_fd = -1;
  int ticket_fd = (int)(uintptr_t)os_handle;
  CUresult result;

  if (output == NULL)
    return CUDA_ERROR_INVALID_VALUE;
  if (type != CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR)
    return CUDA_ERROR_INVALID_HANDLE;
  if (cuinterposer_posix_read_ticket(ticket_fd, &ticket) != 0) {
    result = function != NULL ? function(&imported, os_handle, type) : unavailable();
    return transfer_passthrough_handle(result, imported, output);
  }
  get_properties = (properties_fn)lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");
  if (function == NULL || get_properties == NULL)
    return CUDA_ERROR_INVALID_HANDLE;
  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  if (ticket.resource_kind == CUINTERPOSER_RESOURCE_MULTICAST) {
    if (request_export(&ticket, &raw_fd, NULL, 0) != 0)
      return CUDA_ERROR_INVALID_HANDLE;
    pthread_mutex_lock(&state_lock);
    result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_import(output, &ticket, raw_fd)
                                           : CUDA_ERROR_INVALID_HANDLE;
    pthread_mutex_unlock(&state_lock);
    if (close(raw_fd) != 0 && result == CUDA_SUCCESS) {
      (void)cuMemRelease(*output);
      return CUDA_ERROR_UNKNOWN;
    }
    return result;
  }
  pthread_mutex_lock(&state_lock);
  if (current_phase != PHASE_ACTIVE) {
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  pthread_mutex_unlock(&state_lock);
  if (request_export(&ticket, &raw_fd, NULL, 0) != 0)
    return CUDA_ERROR_INVALID_HANDLE;
  result = function(&imported, (void*)(uintptr_t)raw_fd, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR);
  if (close(raw_fd) != 0 && result == CUDA_SUCCESS) {
    release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(imported);
    return CUDA_ERROR_UNKNOWN;
  }
  if (result != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  if (current_phase != PHASE_ACTIVE) {
    release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(imported);
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_INVALID_HANDLE;
  }
  allocation = find_allocation(ticket.allocation_id);
  if (allocation == NULL) {
    allocation = calloc(1, sizeof(*allocation));
    if (allocation != NULL) {
      memcpy(allocation->id, ticket.allocation_id, sizeof(allocation->id));
      memcpy(allocation->authorization, ticket.authorization, sizeof(allocation->authorization));
      snprintf(
          allocation->creator_participant, sizeof(allocation->creator_participant), "%s",
          ticket.creator_participant);
      snprintf(allocation->creator_endpoint, sizeof(allocation->creator_endpoint), "%s", ticket.creator_endpoint);
      allocation->creator = false;
      allocation->next = allocations;
      allocations = allocation;
    }
  }
  if (allocation != NULL)
    allocation->shared = true;
  if (allocation == NULL || current_context(&allocation->context) != 0 ||
      get_properties(&allocation->properties, imported) != CUDA_SUCCESS ||
      add_managed_handle(allocation, imported, &logical) != 0) {
    release_fn release = (release_fn)lookup_real_symbol("cuMemRelease");
    if (release != NULL)
      (void)release(imported);
    pthread_mutex_unlock(&state_lock);
    return CUDA_ERROR_OUT_OF_MEMORY;
  }
  *output = logical;
  pthread_mutex_unlock(&state_lock);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle application)
{
  properties_fn function = (properties_fn)lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");
  struct handle* handle;
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  handle = resolve_managed_handle(application);
  if (handle == NULL) {
    if (cuinterposer_multicast_is_handle(application))
      result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_get_properties(properties, application)
                                             : CUDA_ERROR_NOT_READY;
    else
      result = is_logical_handle(application) ? CUDA_ERROR_INVALID_HANDLE
                                              : (function != NULL ? function(properties, application) : unavailable());
  } else {
    result = current_phase == PHASE_ACTIVE && handle->driver != 0 ? function(properties, handle->driver)
                                                                  : CUDA_ERROR_NOT_READY;
  }
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_create(output, properties) : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE ? cuinterposer_multicast_add_device(multicast, device) : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}

CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE
               ? cuinterposer_multicast_bind_mem(multicast, 0, false, multicast_offset, memory, memory_offset, size, flags)
               : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result =
      current_phase == PHASE_ACTIVE
          ? cuinterposer_multicast_bind_mem(multicast, device, true, multicast_offset, memory, memory_offset, size, flags)
          : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}
#endif

CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE
               ? cuinterposer_multicast_bind_address(multicast, 0, false, multicast_offset, memory, size, flags)
               : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result = current_phase == PHASE_ACTIVE
               ? cuinterposer_multicast_bind_address(multicast, device, true, multicast_offset, memory, size, flags)
               : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}
#endif

CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  typedef CUresult(CUDAAPI * function_type)(size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
  function_type function = (function_type)lookup_real_symbol("cuMulticastGetGranularity");

  return function != NULL ? function(granularity, properties, option) : unavailable();
}

CUresult CUDAAPI
cuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  CUresult result;

  if ((result = ensure_process_endpoint()) != CUDA_SUCCESS)
    return result;
  pthread_mutex_lock(&state_lock);
  result =
      current_phase == PHASE_ACTIVE ? cuinterposer_multicast_unbind(multicast, device, offset, size) : CUDA_ERROR_NOT_READY;
  pthread_mutex_unlock(&state_lock);
  return result;
}
