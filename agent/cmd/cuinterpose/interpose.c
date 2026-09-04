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
#include <pthread.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include "export.h"
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
typedef CUresult(CUDAAPI* context_get_fn)(CUcontext*);

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
};

enum phase {
  PHASE_ACTIVE,
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
  if (allocation->live_handles == 0 && allocation->live_mappings == 0) {
    /* No local reference is left, so any ticket for this allocation is dead. */
    cuinterpose_export_cache_drop(allocation->id);
    cuinterpose_table_remove(&allocations, cuinterpose_key_bytes(allocation->id));
    free(allocation);
  }
  return result;
}

static int
current_context(CUcontext* context)
{
  context_get_fn get_current = (context_get_fn)cuinterpose_lookup_real_symbol("cuCtxGetCurrent");

  return get_current != NULL && get_current(context) == CUDA_SUCCESS && *context != NULL ? 0 : -1;
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
  stats->phase = current_phase == PHASE_ACTIVE ? CUINTERPOSE_PHASE_ACTIVE : CUINTERPOSE_PHASE_FAILED;
  pthread_mutex_unlock(&state_lock);
  stats->cached_exports = cuinterpose_export_cache_count();
}

/* ------------------------------------------------------------------------- */
/* Control socket: the listener thread and the requests it serves.            */
/* ------------------------------------------------------------------------- */

static uint8_t
phase_code(void)
{
  return current_phase == PHASE_ACTIVE ? CUINTERPOSE_PHASE_ACTIVE : CUINTERPOSE_PHASE_FAILED;
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
  if (passed_fd >= 0 || request.payload_size != 0 || !cuinterpose_header_strings_terminated(&request) ||
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

static int
start_control_endpoint(void)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  pthread_t thread;
  int count;

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
  if (allocation == NULL || current_phase != PHASE_ACTIVE ||
      cuinterpose_random_bytes(allocation->id, sizeof(allocation->id)) != 0 ||
      cuinterpose_random_bytes(allocation->authorization, sizeof(allocation->authorization)) != 0 ||
      current_context(&allocation->context) != 0 ||
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
        current_context(&allocation->context) != 0 ||
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
