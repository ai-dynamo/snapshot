/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

/*
 * cuinterpose-coordinator: the snapshot agent runs this once before a CUDA
 * checkpoint (--prepare) and once after a CUDA restore (--restore). It talks to
 * every CUDA process in the container over the shim's control sockets, checks
 * that their views of shared memory agree, and drives them through the
 * checkpoint or restore steps in the right order. It never touches the GPU
 * itself; every driver call happens inside the workload process that owns the
 * memory. It is statically linked so it can run inside the restored
 * container's mount namespace whatever C library that image has.
 *
 * Progress is reported on stdout as one "cuinterpose-coordinator phase=..."
 * line per phase; the agent logs them. Failures go to stderr.
 */

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <time.h>
#include <unistd.h>

#include "protocol.h"
#include "util/array.h"
#include "util/cleanup.h"
#include "util/id.h"
#include "util/io.h"
#include "util/misc.h"
#include "util/time.h"

struct participant {
  char* endpoint;
  char id[CUINTERPOSE_ID_SIZE];
  struct cuinterpose_record* records;
  uint32_t count;
  uint32_t live_raw_imports;
  uint32_t unsupported_exportable_creations;
  uint8_t phase;
};

/* Owns a participant array so every exit path frees it once, e.g.
 * CUINTERPOSE_CLEANUP(participant_set_release) struct participant_set set = {0}; */
struct participant_set {
  struct participant* items;
  size_t count;
};

static void
participant_set_release(struct participant_set* set);

static unsigned
allocation_transfer_timeout_seconds(void)
{
  static unsigned cached;

  if (cached == 0)
    cached = bounded_seconds(
        getenv(CUINTERPOSE_CARRIER_TIMEOUT_ENV), CUINTERPOSE_CARRIER_TIMEOUT_SECONDS_DEFAULT);
  return cached;
}

/* One machine-readable progress line per phase; the agent parses key=value pairs. */
static void
report_phase(const char* phase, const struct timespec* start, size_t participants, const char* extra)
{
  printf(
      "cuinterpose-coordinator phase=%s status=ok elapsed_ms=%.1f participants=%zu%s%s\n", phase,
      elapsed_since_milliseconds(start),
      participants, extra[0] != '\0' ? " " : "", extra);
  fflush(stdout);
}

struct multicast {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  char creator[CUINTERPOSE_ID_SIZE];
  uint64_t size;
  uint64_t handle_types;
  uint64_t flags;
  uint32_t num_devices;
  uint32_t creators;
  uint32_t devices;
  uint32_t bindings;
  struct multicast_device* device_list;
  size_t device_count;
};

struct multicast_device {
  int32_t device;
  bool bound;
};

struct allocation {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  char creator[CUINTERPOSE_ID_SIZE];
  uint64_t size;
  bool creator_handle;
  bool creator_mapping;
  bool preserve_content;
};

/* validate_topology's scratch set: every multicast owns a device array, so
 * release walks one level down before freeing the outer array. */
struct multicast_set {
  struct multicast* items;
  size_t count;
};

struct allocation_job {
  struct participant* participant;
  struct allocation* allocations;
  size_t allocation_count;
  uint16_t operation;
  int result;
  uint32_t copy_us; /* the shim's own timing of its copies */
};

/* Longest copy time any participant reported in the last carrier phase; the
 * participants copy concurrently, so this is the copy's share of the phase. */
static uint32_t last_allocation_copy_us;

static int
connect_endpoint(const char* endpoint)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  CUINTERPOSE_CLEANUP(close_fd) int fd = -1;

  if (strlen(endpoint) >= sizeof(address.sun_path))
    return -1;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", endpoint);
  fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0 || set_socket_timeouts(fd, cuinterpose_control_timeout_seconds()) != 0 ||
      connect(fd, (const struct sockaddr*)&address, sizeof(address)) != 0)
    return -1;
  return take_fd(&fd);
}

static struct multicast*
find_multicast(struct multicast* multicasts, size_t count, const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  size_t index;

  for (index = 0; index < count; index++) {
    if (allocation_id_eq(multicasts[index].id, id))
      return &multicasts[index];
  }
  return NULL;
}

static struct multicast_device*
find_multicast_device(struct multicast* multicast, int32_t device)
{
  size_t index;

  for (index = 0; index < multicast->device_count; index++) {
    if (multicast->device_list[index].device == device)
      return &multicast->device_list[index];
  }
  return NULL;
}

static void
multicast_set_release(struct multicast_set* set)
{
  size_t index;

  for (index = 0; index < set->count; index++)
    free(set->items[index].device_list);
  free(set->items);
  set->items = NULL;
  set->count = 0;
}

static int
exchange(struct participant* participant, uint16_t operation, struct cuinterpose_record** records, uint32_t* count)
{
  struct cuinterpose_header request;
  struct cuinterpose_header response;
  uint64_t payload_size;
  CUINTERPOSE_CLEANUP(close_fd) int fd = -1;
  CUINTERPOSE_CLEANUP(free_ptr) struct cuinterpose_record* entries = NULL;
  bool strings_terminated;

  *records = NULL;
  *count = 0;
  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  if (operation != CUINTERPOSE_HANDSHAKE)
    id_copy(request.participant_id, participant->id);
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || send_all(fd, &request, sizeof(request)) != 0 ||
      read_all(fd, &response, sizeof(response)) != 0)
    return -1;
  strings_terminated = cuinterpose_header_strings_terminated(&response);
  if (!strings_terminated || response.magic != CUINTERPOSE_MAGIC || response.version != CUINTERPOSE_VERSION ||
      response.operation != operation || response.status != 0 || response.count > CUINTERPOSE_MAX_RECORDS ||
      response.payload_size != (uint64_t)response.count * sizeof(struct cuinterpose_record)) {
    if (strings_terminated && response.message[0] != '\0')
      fprintf(stderr, "%s: %s\n", participant->endpoint, response.message);
    return -1;
  }
  if (operation == CUINTERPOSE_HANDSHAKE) {
    id_copy(participant->id, response.participant_id);
  } else if (!id_eq(response.participant_id, participant->id)) {
    return -1;
  }
  if (operation == CUINTERPOSE_HANDSHAKE || operation == CUINTERPOSE_INSPECT) {
    participant->live_raw_imports = response.live_raw_imports;
    participant->unsupported_exportable_creations = response.unsupported_exportable_creations;
    participant->phase = response.phase;
  }
  payload_size = response.payload_size;
  if (payload_size != 0) {
    entries = calloc(response.count, sizeof(*entries));
    if (entries == NULL || read_all(fd, entries, (size_t)payload_size) != 0)
      return -1;
  }
  *records = entries;
  entries = NULL;
  *count = response.count;
  return 0;
}

static struct allocation*
find_allocation(struct allocation* allocations, size_t count, const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  size_t index;

  for (index = 0; index < count; index++) {
    if (allocation_id_eq(allocations[index].id, id))
      return &allocations[index];
  }
  return NULL;
}

/* Every validate_topology rejection is reported in one shape; the helpers
 * return -1 so failure sites are plain `return` statements. */
static int
topology_rejected(const char* reason, size_t participant_index, size_t participant_count, uint32_t record_index)
{
  if (participant_index < participant_count && record_index != UINT32_MAX)
    fprintf(
        stderr, "topology validate failed: %s (participant index %zu, record index %u)\n", reason, participant_index,
        (unsigned)record_index);
  else if (participant_index < participant_count)
    fprintf(stderr, "topology validate failed: %s (participant index %zu)\n", reason, participant_index);
  else
    fprintf(stderr, "topology validate failed: %s\n", reason);
  return -1;
}

static int
topology_rejected_value(const char* reason, uint64_t value, size_t participant_index, uint32_t record_index)
{
  fprintf(
      stderr, "topology validate failed: %s (value %llu, participant index %zu, record index %u)\n", reason,
      (unsigned long long)value, participant_index, (unsigned)record_index);
  return -1;
}

/* The returned allocation array is caller-owned; the multicast bookkeeping
 * never leaves this function and is scope-released, whatever path is taken. */
static int
validate_topology(
    struct participant* participants, size_t participant_count, struct allocation** output, size_t* output_count)
{
  CUINTERPOSE_CLEANUP(free_ptr) struct allocation* allocations = NULL;
  CUINTERPOSE_CLEANUP(multicast_set_release) struct multicast_set multicasts = {0};
  size_t allocation_count = 0;
  size_t participant_index;
  uint32_t record_index = UINT32_MAX;

  if (participant_count == 0) {
    fprintf(stderr, "topology validate failed: no participants\n");
    return -1;
  }
  for (participant_index = 0; participant_index < participant_count; participant_index++) {
    struct participant* participant = &participants[participant_index];
    size_t previous;

    record_index = UINT32_MAX;
    if (!is_lower_hex_id(participant->id)) {
      return topology_rejected("invalid participant identity", participant_index, participant_count, record_index);
    }
    for (previous = 0; previous < participant_index; previous++) {
      if (id_eq(participants[previous].id, participant->id)) {
        return topology_rejected("duplicate participant identity", participant_index, participant_count, record_index);
      }
    }
    for (record_index = 0; record_index < participant->count; record_index++) {
      const struct cuinterpose_record* record = &participant->records[record_index];
      struct allocation* allocation = find_allocation(allocations, allocation_count, record->allocation_id);
      struct multicast* multicast = find_multicast(multicasts.items, multicasts.count, record->allocation_id);

      if (record->kind == CUINTERPOSE_ALLOCATION) {
        if (allocation == NULL) {
          allocation = array_push((void**)&allocations, &allocation_count, sizeof(*allocation));
          if (allocation == NULL) {
            return topology_rejected("allocation metadata allocation failed", participant_index, participant_count, record_index);
          }
          memset(allocation, 0, sizeof(*allocation));
          allocation_id_copy(allocation->id, record->allocation_id);
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0) {
          if (record->requested_handle_types != CUINTERPOSE_POSIX_HANDLE_TYPE) {
            return topology_rejected_value("non-POSIX requested handle type", record->requested_handle_types, participant_index, record_index);
          }
          if (record->allocation_size == 0) {
            return topology_rejected("zero creator allocation size", participant_index, participant_count, record_index);
          }
          if (allocation->creator[0] != '\0' && !id_eq(allocation->creator, participant->id)) {
            return topology_rejected("conflicting creators", participant_index, participant_count, record_index);
          }
          id_copy(allocation->creator, participant->id);
          allocation->size = record->allocation_size;
          allocation->creator_handle = (record->flags & CUINTERPOSE_APPLICATION_HANDLE_LIVE) != 0;
          allocation->preserve_content = (record->flags & CUINTERPOSE_ALLOCATION_CONTENT) != 0;
        } else if ((record->flags & CUINTERPOSE_ALLOCATION_CONTENT) != 0) {
          return topology_rejected(
              "allocation content flag on importer", participant_index, participant_count, record_index);
        }
      } else if (record->kind == CUINTERPOSE_MAPPING) {
        if (record->address == 0) {
          return topology_rejected("zero mapping address", participant_index, participant_count, record_index);
        }
        if (record->size == 0) {
          return topology_rejected("zero mapping size", participant_index, participant_count, record_index);
        }
        if (record->access_count > CUINTERPOSE_MAX_ACCESS) {
          return topology_rejected_value("mapping access count exceeds limit", record->access_count, participant_index, record_index);
        }
        if (allocation == NULL) {
          allocation = array_push((void**)&allocations, &allocation_count, sizeof(*allocation));
          if (allocation == NULL) {
            return topology_rejected("allocation metadata allocation failed", participant_index, participant_count, record_index);
          }
          memset(allocation, 0, sizeof(*allocation));
          allocation_id_copy(allocation->id, record->allocation_id);
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0)
          allocation->creator_mapping = true;
      } else if (record->kind == CUINTERPOSE_MULTICAST) {
        if (record->handle_types != CUINTERPOSE_POSIX_HANDLE_TYPE || record->num_devices == 0 ||
            record->allocation_size == 0 || !is_lower_hex_id(record->creator_participant)) {
          return topology_rejected("invalid multicast properties", participant_index, participant_count, record_index);
        }
        if (multicast == NULL) {
          multicast = array_push((void**)&multicasts.items, &multicasts.count, sizeof(*multicast));
          if (multicast == NULL) {
            return topology_rejected("multicast metadata allocation failed", participant_index, participant_count, record_index);
          }
          memset(multicast, 0, sizeof(*multicast));
          allocation_id_copy(multicast->id, record->allocation_id);
          multicast->size = record->allocation_size;
          multicast->handle_types = record->handle_types;
          multicast->flags = record->object_flags;
          multicast->num_devices = record->num_devices;
          id_copy(multicast->creator, record->creator_participant);
        } else if (
            multicast->size != record->allocation_size || multicast->handle_types != record->handle_types ||
            multicast->flags != record->object_flags || multicast->num_devices != record->num_devices ||
            !id_eq(multicast->creator, record->creator_participant)) {
          return topology_rejected("inconsistent multicast properties", participant_index, participant_count, record_index);
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0) {
          if (!id_eq(participant->id, multicast->creator)) {
            return topology_rejected("invalid multicast creator", participant_index, participant_count, record_index);
          }
          multicast->creators++;
        }
      } else if (record->kind == CUINTERPOSE_MULTICAST_DEVICE) {
        struct multicast_device* device;
        if (multicast == NULL) {
          return topology_rejected("multicast device precedes object", participant_index, participant_count, record_index);
        }
        if (find_multicast_device(multicast, record->device) != NULL) {
          return topology_rejected("duplicate multicast device", participant_index, participant_count, record_index);
        }
        device = array_push((void**)&multicast->device_list, &multicast->device_count, sizeof(*device));
        if (device == NULL) {
          return topology_rejected("multicast device metadata allocation failed", participant_index, participant_count, record_index);
        }
        memset(device, 0, sizeof(*device));
        device->device = record->device;
        multicast->devices++;
      } else if (record->kind == CUINTERPOSE_MULTICAST_BINDING) {
        struct allocation* member = find_allocation(allocations, allocation_count, record->member_id);
        struct multicast_device* device;
        if (multicast == NULL || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset ||
            (record->binding_kind != CUINTERPOSE_MULTICAST_BIND_MEM &&
             record->binding_kind != CUINTERPOSE_MULTICAST_BIND_ADDR) ||
            (record->api_version != 1 && record->api_version != 2)) {
          return topology_rejected("invalid multicast binding", participant_index, participant_count, record_index);
        }
        if ((record->binding_kind == CUINTERPOSE_MULTICAST_BIND_MEM && (member == NULL || record->address != 0)) ||
            (record->binding_kind == CUINTERPOSE_MULTICAST_BIND_ADDR && record->address == 0)) {
          return topology_rejected("invalid multicast member", participant_index, participant_count, record_index);
        }
        device = find_multicast_device(multicast, record->device);
        if (device == NULL) {
          return topology_rejected("multicast binding device is absent", participant_index, participant_count, record_index);
        }
        device->bound = true;
        multicast->bindings++;
      } else if (record->kind == CUINTERPOSE_MULTICAST_MAPPING) {
        if (multicast == NULL || record->address == 0 || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset || record->access_count > CUINTERPOSE_MAX_ACCESS) {
          return topology_rejected("invalid multicast mapping", participant_index, participant_count, record_index);
        }
      } else {
        return topology_rejected_value("unknown record kind", record->kind, participant_index, record_index);
      }
    }
  }
  /* Whole-topology checks: no single participant or record is at fault. */
  participant_index = participant_count;
  record_index = UINT32_MAX;
  {
    size_t allocation_index;

    for (allocation_index = 0; allocation_index < allocation_count; allocation_index++) {
      struct allocation* allocation = &allocations[allocation_index];
      struct participant* participant;

      if (allocation->creator[0] == '\0') {
        return topology_rejected("missing creator", participant_index, participant_count, record_index);
      }
      if (allocation->size == 0) {
        return topology_rejected("missing allocation size", participant_index, participant_count, record_index);
      }
      if (!allocation->creator_handle && !allocation->creator_mapping) {
        return topology_rejected("missing creator anchor", participant_index, participant_count, record_index);
      }
      for (participant_index = 0; participant_index < participant_count; participant_index++) {
        participant = &participants[participant_index];
        for (record_index = 0; record_index < participant->count; record_index++) {
          const struct cuinterpose_record* record = &participant->records[record_index];
          if (record->kind == CUINTERPOSE_MAPPING &&
              allocation_id_eq(record->allocation_id, allocation->id) &&
              (record->offset > allocation->size || record->size > allocation->size - record->offset)) {
            return topology_rejected("mapping out of bounds", participant_index, participant_count, record_index);
          }
          /* A multicast binding of this allocation must lie inside it. */
          if (record->kind == CUINTERPOSE_MULTICAST_BINDING && record->binding_kind == CUINTERPOSE_MULTICAST_BIND_MEM &&
              allocation_id_eq(record->member_id, allocation->id) &&
              (record->member_offset > allocation->size || record->size > allocation->size - record->member_offset)) {
            return topology_rejected("multicast binding out of member bounds", participant_index, participant_count, record_index);
          }
        }
      }
      participant_index = participant_count;
      record_index = UINT32_MAX;
    }
  }
  {
    size_t multicast_index;

    for (multicast_index = 0; multicast_index < multicasts.count; multicast_index++) {
      struct multicast* multicast = &multicasts.items[multicast_index];

      if (multicast->creators != 1) {
        return topology_rejected_value("multicast group must have exactly one creator", multicast->creators, participant_index, record_index);
      }
      if (multicast->devices != multicast->num_devices) {
        return topology_rejected_value("incomplete multicast device group", multicast->devices, participant_index, record_index);
      }
      if (multicast->bindings < multicast->num_devices) {
        return topology_rejected_value("incomplete multicast binding group", multicast->bindings, participant_index, record_index);
      }
      {
        size_t device_index;

        for (device_index = 0; device_index < multicast->device_count; device_index++) {
          if (!multicast->device_list[device_index].bound) {
            return topology_rejected_value(
                "multicast device has no binding", (uint64_t)(uint32_t)multicast->device_list[device_index].device,
                participant_index, record_index);
          }
        }
      }
    }
  }
  *output = allocations;
  *output_count = allocation_count;
  allocations = NULL; /* output owns the array now */
  return 0;
}

static int write_state(struct participant* participants, size_t count, FILE* output);

static int
write_state_atomic(const char* path, struct participant* participants, size_t count)
{
  char temporary[PATH_MAX];
  CUINTERPOSE_CLEANUP(close_file) FILE* output = NULL;
  CUINTERPOSE_CLEANUP(close_fd) int fd = -1;
  CUINTERPOSE_CLEANUP(close_fd) int dir_fd = -1;
  /* Removes the temporary file unless the rename below disarms it. */
  CUINTERPOSE_CLEANUP(unlink_guard_release) struct unlink_guard guard = {NULL, false};
  int length;

  length = snprintf(temporary, sizeof(temporary), "%s.tmp.XXXXXX", path);
  if (length < 0 || (size_t)length >= sizeof(temporary))
    return -1;
  fd = mkstemp(temporary);
  if (fd < 0)
    return -1;
  guard.path = temporary;
  guard.armed = true;
  output = fdopen(fd, "w");
  if (output == NULL)
    return -1;
  fd = -1; /* output owns the descriptor now */

  if (write_state(participants, count, output) != 0 || fsync(fileno(output)) != 0)
    return -1;
  /* fclose reports write-back errors; the stream is gone either way. */
  if (fclose(output) != 0) {
    output = NULL;
    return -1;
  }
  output = NULL;
  if (rename(temporary, path) != 0)
    return -1;
  guard.armed = false;
  /* The rename is only durable once the directory entry itself is on disk. */
  {
    char directory[PATH_MAX];
    char* slash;

    snprintf(directory, sizeof(directory), "%s", path);
    slash = strrchr(directory, '/');
    if (slash != NULL)
      *slash = '\0';
    dir_fd = open(slash != NULL ? directory : ".", O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (dir_fd < 0 || fsync(dir_fd) != 0)
      return -1;
  }
  return 0;
}

static int
record_compare(const void* left, const void* right)
{
  return memcmp(left, right, sizeof(struct cuinterpose_record));
}

static int
participant_compare(const void* left, const void* right)
{
  const struct participant* a = left;
  const struct participant* b = right;
  return memcmp(a->id, b->id, CUINTERPOSE_ID_SIZE);
}

static int
write_state(struct participant* participants, size_t count, FILE* output)
{
  size_t index;

  qsort(participants, count, sizeof(*participants), participant_compare);
  if (fprintf(output, "%s\n", CUINTERPOSE_STATE_HEADER) < 0)
    return -1;
  for (index = 0; index < count; index++) {
    struct participant* participant = &participants[index];
    uint32_t record_index;

    qsort(participant->records, participant->count, sizeof(*participant->records), record_compare);
    if (fprintf(output, "participant %s %u\n", participant->id, participant->count) < 0)
      return -1;
    for (record_index = 0; record_index < participant->count; record_index++) {
      const uint8_t* bytes = (const uint8_t*)&participant->records[record_index];
      size_t byte_index;
      for (byte_index = 0; byte_index < sizeof(struct cuinterpose_record); byte_index++) {
        if (fprintf(output, "%02x", bytes[byte_index]) < 0)
          return -1;
      }
      if (fputc('\n', output) == EOF)
        return -1;
    }
  }
  return fflush(output);
}

static int
read_record(FILE* input, struct cuinterpose_record* record)
{
  uint8_t* bytes = (uint8_t*)record;
  size_t index;

  for (index = 0; index < sizeof(*record); index++) {
    int high = hex_digit(fgetc(input));
    int low = hex_digit(fgetc(input));
    if (high < 0 || low < 0)
      return -1;
    bytes[index] = (uint8_t)((high << 4) | low);
  }
  return fgetc(input) == '\n' ? 0 : -1;
}

static int
read_state(FILE* input, struct participant_set* output)
{
  char line[128];
  CUINTERPOSE_CLEANUP(participant_set_release) struct participant_set parsed = {0};

  if (fgets(line, sizeof(line), input) == NULL || strncmp(line, CUINTERPOSE_STATE_HEADER, strlen(CUINTERPOSE_STATE_HEADER)) != 0 ||
      line[strlen(CUINTERPOSE_STATE_HEADER)] != '\n')
    return -1;
  while (fgets(line, sizeof(line), input) != NULL) {
    struct participant* participant;
    struct participant* expanded;
    char id[CUINTERPOSE_ID_SIZE];
    unsigned int record_count;
    unsigned int index;

    if (sscanf(line, "participant %32s %u", id, &record_count) != 2 || !is_lower_hex_id(id) ||
        record_count > CUINTERPOSE_MAX_RECORDS)
      return -1;
    expanded = reallocarray(parsed.items, parsed.count + 1, sizeof(*parsed.items));
    if (expanded == NULL)
      return -1;
    parsed.items = expanded;
    participant = &parsed.items[parsed.count++];
    memset(participant, 0, sizeof(*participant));
    id_copy(participant->id, id);
    participant->records = calloc(record_count == 0 ? 1 : record_count, sizeof(*participant->records));
    if (participant->records == NULL)
      return -1;
    participant->count = record_count;
    for (index = 0; index < record_count; index++) {
      if (read_record(input, &participant->records[index]) != 0)
        return -1;
    }
  }
  *output = parsed;
  parsed.items = NULL; /* output owns the array now */
  parsed.count = 0;
  return output->count == 0 ? -1 : 0;
}

static int
handshake(struct participant* participants, size_t count)
{
  size_t index;

  for (index = 0; index < count; index++) {
    struct cuinterpose_record* records = NULL;
    uint32_t record_count = 0;
    if (exchange(&participants[index], CUINTERPOSE_HANDSHAKE, &records, &record_count) != 0)
      return -1;
    free(records);
  }
  return 0;
}

static int
inspect(struct participant* participants, size_t count)
{
  size_t index;

  if (handshake(participants, count) != 0)
    return -1;
  for (index = 0; index < count; index++) {
    if (exchange(
            &participants[index], CUINTERPOSE_INSPECT, &participants[index].records, &participants[index].count) != 0)
      return -1;
  }
  return 0;
}

static void
free_participants(struct participant* participants, size_t count)
{
  size_t index;

  if (participants == NULL)
    return;
  for (index = 0; index < count; index++) {
    free(participants[index].endpoint);
    free(participants[index].records);
  }
  free(participants);
}

static void
participant_set_release(struct participant_set* set)
{
  free_participants(set->items, set->count);
  set->items = NULL;
  set->count = 0;
}

struct command {
  struct participant* participant;
  uint16_t operation;
  int result;
};

static void*
command_run(void* argument)
{
  struct command* command = argument;
  struct cuinterpose_record* records = NULL;
  uint32_t record_count = 0;

  command->result = exchange(command->participant, command->operation, &records, &record_count);
  free(records);
  return NULL;
}

static int
command_all_parallel(struct participant* participants, size_t count, uint16_t operation)
{
  CUINTERPOSE_CLEANUP(free_ptr) struct command* commands = NULL;
  CUINTERPOSE_CLEANUP(free_ptr) pthread_t* threads = NULL;
  size_t started = 0;
  size_t index;
  int result = 0;

  commands = calloc(count, sizeof(*commands));
  threads = calloc(count, sizeof(*threads));
  if (commands == NULL || threads == NULL)
    return -1;
  for (index = 0; index < count; index++) {
    commands[index].participant = &participants[index];
    commands[index].operation = operation;
    commands[index].result = -1;
    if (pthread_create(&threads[index], NULL, command_run, &commands[index]) != 0) {
      result = -1;
      break;
    }
    started++;
  }
  for (index = 0; index < started; index++) {
    if (pthread_join(threads[index], NULL) != 0 || commands[index].result != 0)
      result = -1;
  }
  return result;
}

static int
exchange_allocations(struct participant* participant, uint16_t operation, uint64_t size, uint32_t* copy_us)
{
  struct cuinterpose_header request;
  struct cuinterpose_header response;
  CUINTERPOSE_CLEANUP(close_fd) int response_fd = -1;
  CUINTERPOSE_CLEANUP(close_fd) int fd = -1;

  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  request.payload_size = size;
  id_copy(request.participant_id, participant->id);
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || set_socket_timeouts(fd, allocation_transfer_timeout_seconds()) != 0 ||
      cuinterpose_send_header(fd, &request, -1) != 0 || cuinterpose_receive_header(fd, &response, &response_fd) != 0)
    return -1;
  if (response_fd >= 0 || !cuinterpose_header_strings_terminated(&response) || response.magic != CUINTERPOSE_MAGIC ||
      response.version != CUINTERPOSE_VERSION || response.operation != operation || response.status != 0 ||
      response.count != 0 || !id_eq(response.participant_id, participant->id)) {
    if (cuinterpose_header_strings_terminated(&response) && response.message[0] != '\0')
      fprintf(stderr, "%s: %s\n", participant->endpoint, response.message);
    return -1;
  }
  if (response.payload_size != size) {
    fprintf(
        stderr, "%s: allocation transfer moved %llu bytes, expected %llu\n", participant->endpoint,
        (unsigned long long)response.payload_size, (unsigned long long)size);
    return -1;
  }
  *copy_us = response.copy_us;
  return 0;
}

/*
 * One request per participant: the shim copies every allocation it owns in
 * one batch (all copies issued on one stream, one wait), and reports
 * the byte count in the reply's payload_size. The expected total is checked
 * against the topology so a shim that silently skipped an allocation fails
 * here rather than at restore.
 */
static void*
run_allocation_job(void* argument)
{
  struct allocation_job* job = argument;
  size_t index;
  uint64_t expected = 0;

  for (index = 0; index < job->allocation_count; index++) {
    const struct allocation* allocation = &job->allocations[index];
    if (allocation->preserve_content && id_eq(allocation->creator, job->participant->id))
      expected += allocation->size;
  }
  job->result = exchange_allocations(job->participant, job->operation, expected, &job->copy_us);
  return NULL;
}

static int
transfer_allocations(
    struct participant* participants, size_t participant_count, struct allocation* allocations,
    size_t allocation_count, uint16_t operation)
{
  CUINTERPOSE_CLEANUP(free_ptr) struct allocation_job* jobs = NULL;
  CUINTERPOSE_CLEANUP(free_ptr) pthread_t* threads = NULL;
  CUINTERPOSE_CLEANUP(free_ptr) bool* launched = NULL;
  size_t index;
  int result = 0;

  last_allocation_copy_us = 0;
  jobs = calloc(participant_count, sizeof(*jobs));
  threads = calloc(participant_count, sizeof(*threads));
  launched = calloc(participant_count, sizeof(*launched));
  if (jobs == NULL || threads == NULL || launched == NULL)
    return -1;
  for (index = 0; index < participant_count; index++) {
    jobs[index].participant = &participants[index];
    jobs[index].allocations = allocations;
    jobs[index].allocation_count = allocation_count;
    jobs[index].operation = operation;
    if (pthread_create(&threads[index], NULL, run_allocation_job, &jobs[index]) != 0) {
      result = -1;
      break;
    }
    launched[index] = true;
  }
  for (index = 0; index < participant_count; index++) {
    if (!launched[index])
      continue;
    if (pthread_join(threads[index], NULL) != 0 || jobs[index].result != 0)
      result = -1;
    else if (jobs[index].copy_us > last_allocation_copy_us)
      last_allocation_copy_us = jobs[index].copy_us;
  }
  return result;
}

static int
save_allocations(
    struct participant* participants, size_t participant_count, struct allocation* allocations,
    size_t allocation_count)
{
  return transfer_allocations(
      participants, participant_count, allocations, allocation_count, CUINTERPOSE_SAVE_ALLOCATIONS);
}

static int
load_allocations(
    struct participant* participants, size_t participant_count, struct allocation* allocations,
    size_t allocation_count)
{
  return transfer_allocations(
      participants, participant_count, allocations, allocation_count, CUINTERPOSE_LOAD_ALLOCATIONS);
}

/*
 * Multicast restore is four phases with a barrier after each. The barrier
 * between DEVICES and the final phase is a hard requirement of the driver:
 * cuMulticastBindMem/BindAddr spin until every device of the team has been
 * attached with cuMulticastAddDevice, so a bind issued before every
 * participant has finished AddDevice would wait forever. Within a phase the
 * participants run concurrently.
 */
static int
restore_multicast(struct participant* participants, size_t count)
{
  if (command_all_parallel(participants, count, CUINTERPOSE_RESTORE_MULTICAST_CREATORS) != 0)
    return -1;
  if (command_all_parallel(participants, count, CUINTERPOSE_RESTORE_MULTICAST_IMPORTERS) != 0)
    return -1;
  if (command_all_parallel(participants, count, CUINTERPOSE_RESTORE_MULTICAST_DEVICES) != 0)
    return -1;
  return command_all_parallel(participants, count, CUINTERPOSE_RESTORE_MULTICAST_BINDINGS);
}

/* Sum of allocation-content bytes and count for the progress lines. */
static void
allocation_totals(const struct allocation* allocations, size_t allocation_count, size_t* count, uint64_t* bytes)
{
  size_t index;

  *count = 0;
  *bytes = 0;
  for (index = 0; index < allocation_count; index++) {
    if (allocations[index].preserve_content) {
      (*count)++;
      *bytes += allocations[index].size;
    }
  }
}

static void
allocation_extra(
    char* buffer, size_t size, const struct allocation* allocations, size_t allocation_count,
    const struct timespec* start)
{
  size_t count;
  uint64_t bytes;
  double ms = elapsed_since_milliseconds(start);

  allocation_totals(allocations, allocation_count, &count, &bytes);
  /* gb_per_s covers setup, copies, teardown, and sockets; copy_gb_per_s uses
   * the slowest participant's device-copy time. */
  snprintf(
      buffer, size, "allocation_count=%zu allocation_bytes=%llu gb_per_s=%.2f copy_gb_per_s=%.2f", count,
      (unsigned long long)bytes, ms > 0.0 ? ((double)bytes / 1e9) / (ms / 1000.0) : 0.0,
      last_allocation_copy_us > 0 ? ((double)bytes / 1e9) / ((double)last_allocation_copy_us / 1e6) : 0.0);
}

/* Prepare must not start with state cuinterpose cannot reconstruct. These
 * checks happen before the first destructive operation. */
static int
refuse_unsupported_state(const struct participant* participants, size_t count)
{
  size_t index;
  int result = 0;

  for (index = 0; index < count; index++) {
    if (participants[index].live_raw_imports != 0) {
      fprintf(
          stderr, "prepare refused: participant %s (%s) holds %u live raw imports; release untracked imports before checkpoint\n",
          participants[index].id, participants[index].endpoint, participants[index].live_raw_imports);
      result = -1;
    }
    if (participants[index].unsupported_exportable_creations != 0) {
      fprintf(
          stderr,
          "prepare refused: participant %s (%s) created %u CUDA resources with unsupported exportable handle types\n",
          participants[index].id, participants[index].endpoint,
          participants[index].unsupported_exportable_creations);
      result = -1;
    }
  }
  return result;
}

static int
same_participants(struct participant* expected, size_t expected_count, struct participant* actual, size_t actual_count)
{
  size_t index;

  if (expected_count != actual_count)
    return -1;
  qsort(expected, expected_count, sizeof(*expected), participant_compare);
  qsort(actual, actual_count, sizeof(*actual), participant_compare);
  for (index = 0; index < expected_count; index++) {
    if (!id_eq(expected[index].id, actual[index].id))
      return -1;
  }
  return 0;
}

static int
same_topology(struct participant* expected, size_t expected_count, struct participant* actual, size_t actual_count)
{
  size_t index;

  if (same_participants(expected, expected_count, actual, actual_count) != 0)
    return -1;
  for (index = 0; index < expected_count; index++) {
    if (expected[index].count != actual[index].count)
      return -1;
    qsort(expected[index].records, expected[index].count, sizeof(*expected[index].records), record_compare);
    qsort(actual[index].records, actual[index].count, sizeof(*actual[index].records), record_compare);
    if (memcmp(
            expected[index].records, actual[index].records,
            (size_t)expected[index].count * sizeof(*expected[index].records)) != 0)
      return -1;
  }
  return 0;
}

int
main(int argc, char** argv)
{
  /* participant_store owns the argv-parsed array; the plain variables are
   * borrowed views so the phase logic below stays free of .items/.count. */
  CUINTERPOSE_CLEANUP(participant_set_release) struct participant_set participant_store = {0};
  CUINTERPOSE_CLEANUP(close_file) FILE* state = NULL;
  CUINTERPOSE_CLEANUP(free_ptr) struct allocation* allocations = NULL;
  CUINTERPOSE_CLEANUP(free_ptr) struct allocation* restored_allocations = NULL;
  struct participant* participants = NULL;
  size_t allocation_count = 0;
  size_t restored_count = 0;
  size_t participant_count = 0;
  size_t index;
  bool prepare;
  struct timespec phase_start;
  char extra[160];

  char state_path[PATH_MAX];
  const char* proc_root;
  const char* checkpoint_dir;
  const char* control_dir;
  int length;

  /* A participant that hangs up mid-exchange must produce an error, not kill us. */
  signal(SIGPIPE, SIG_IGN);

  if (argc < 11 || (argc - 8) % 3 != 0 || (strcmp(argv[1], "--prepare") != 0 && strcmp(argv[1], "--restore") != 0) ||
      strcmp(argv[2], "--proc-root") != 0 || strcmp(argv[4], "--checkpoint-dir") != 0 ||
      strcmp(argv[6], "--control-dir") != 0) {
    fprintf(
        stderr,
        "usage: %s (--prepare|--restore) --proc-root PATH --checkpoint-dir PATH --control-dir PATH "
        "--process OBSERVED_PID NAMESPACE_PID...\n"
        "  --proc-root: a /proc mount through which every participant's root is reachable as\n"
        "               PATH/<observed-pid>/root; empty when running inside the target container's\n"
        "               mount namespace (the agent's normal prepare and restore mode).\n"
        "  --control-dir: the snapshot control directory inside the container, holding the\n"
        "               shim's cuinterpose-<namespace-pid>.sock endpoints.\n",
        argv[0]);
    return EXIT_FAILURE;
  }
  prepare = strcmp(argv[1], "--prepare") == 0;
  proc_root = argv[3];
  checkpoint_dir = argv[5];
  control_dir = argv[7];
  if (control_dir[0] != '/') {
    fprintf(stderr, "--control-dir must be an absolute path\n");
    return EXIT_FAILURE;
  }
  length = snprintf(state_path, sizeof(state_path), "%s/%s", checkpoint_dir, CUINTERPOSE_STATE_FILENAME);
  if (length < 0 || (size_t)length >= sizeof(state_path))
    return EXIT_FAILURE;
  if (!prepare) {
    state = fopen(state_path, "r");
    if (state == NULL) {
      /* The agent only asks for a restore when the checkpoint recorded that
       * prepare ran, so a missing state file means a damaged artifact. */
      fprintf(stderr, "restore failed: %s is missing or unreadable: %s\n", state_path, strerror(errno));
      return EXIT_FAILURE;
    }
  }
  participant_store.count = (size_t)(argc - 8) / 3;
  participant_store.items = calloc(participant_store.count, sizeof(*participant_store.items));
  if (participant_store.items == NULL)
    return EXIT_FAILURE;
  participants = participant_store.items;
  participant_count = participant_store.count;
  for (index = 0; index < participant_count; index++) {
    char endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
    char* end;
    long observed;
    long namespace;
    int length;

    if (strcmp(argv[8 + index * 3], "--process") != 0)
      return EXIT_FAILURE;
    errno = 0;
    observed = strtol(argv[9 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || observed <= 0 || observed > INT_MAX)
      return EXIT_FAILURE;
    errno = 0;
    namespace = strtol(argv[10 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || namespace <= 0 || namespace > INT_MAX)
      return EXIT_FAILURE;
    if (proc_root[0] == '\0')
      length = snprintf(endpoint, sizeof(endpoint), "%s/%s%ld.sock", control_dir, CUINTERPOSE_SOCKET_PREFIX, namespace);
    else
      length = snprintf(
          endpoint, sizeof(endpoint), "%s/%ld/root%s/%s%ld.sock", proc_root, observed, control_dir,
          CUINTERPOSE_SOCKET_PREFIX, namespace);
    if (length < 0 || (size_t)length >= sizeof(endpoint)) {
      fprintf(stderr, "control socket path for process %ld does not fit in sun_path\n", observed);
      return EXIT_FAILURE;
    }
    participants[index].endpoint = strdup(endpoint);
    if (participants[index].endpoint == NULL)
      return EXIT_FAILURE;
  }
  if (prepare) {
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (inspect(participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: participant inspect\n");
      return EXIT_FAILURE;
    }
    {
      size_t records = 0;
      uint32_t raw_imports = 0;
      uint32_t unsupported = 0;
      for (index = 0; index < participant_count; index++) {
        records += participants[index].count;
        raw_imports += participants[index].live_raw_imports;
        unsupported += participants[index].unsupported_exportable_creations;
      }
      snprintf(
          extra, sizeof(extra), "records=%zu live_raw_imports=%u unsupported_exportable_creations=%u", records,
          raw_imports, unsupported);
    }
    report_phase("inspect", &phase_start, participant_count, extra);
    if (refuse_unsupported_state(participants, participant_count) != 0)
      return EXIT_FAILURE;
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (validate_topology(participants, participant_count, &allocations, &allocation_count) != 0) {
      fprintf(stderr, "prepare failed: topology validate\n");
      return EXIT_FAILURE;
    }
    report_phase("validate", &phase_start, participant_count, "");
    /* Every rank must finish multicast teardown before PREPARE_UNICAST
     * unmaps memory: multicast bindings sit on top of unicast allocations. */
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (command_all_parallel(participants, participant_count, CUINTERPOSE_PREPARE_MULTICAST) != 0) {
      fprintf(stderr, "prepare failed: multicast teardown\n");
      return EXIT_FAILURE;
    }
    report_phase("prepare_multicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (save_allocations(participants, participant_count, allocations, allocation_count) != 0) {
      fprintf(stderr, "prepare failed: allocation save\n");
      return EXIT_FAILURE;
    }
    allocation_extra(extra, sizeof(extra), allocations, allocation_count, &phase_start);
    report_phase("save_allocations", &phase_start, participant_count, extra);
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (command_all_parallel(participants, participant_count, CUINTERPOSE_PREPARE_UNICAST) != 0) {
      fprintf(stderr, "prepare failed: unicast teardown\n");
      return EXIT_FAILURE;
    }
    report_phase("prepare_unicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (write_state_atomic(state_path, participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: atomic state write\n");
      return EXIT_FAILURE;
    }
    report_phase("state_write", &phase_start, participant_count, "");
  } else {
    CUINTERPOSE_CLEANUP(participant_set_release) struct participant_set expected = {0};

    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (read_state(state, &expected) != 0) {
      fprintf(stderr, "restore failed: cannot parse %s\n", state_path);
      return EXIT_FAILURE;
    }
    if (fclose(state) != 0) {
      state = NULL;
      return EXIT_FAILURE;
    }
    state = NULL;
    if (handshake(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: participant handshake\n");
      return EXIT_FAILURE;
    }
    if (same_participants(expected.items, expected.count, participants, participant_count) != 0) {
      fprintf(
          stderr, "restore failed: the restored processes (%zu) do not match the checkpointed participants (%zu)\n",
          participant_count, expected.count);
      return EXIT_FAILURE;
    }
    if (validate_topology(expected.items, expected.count, &allocations, &allocation_count) != 0) {
      fprintf(stderr, "restore failed: checkpointed topology is invalid\n");
      return EXIT_FAILURE;
    }
    report_phase("handshake", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (load_allocations(participants, participant_count, allocations, allocation_count) != 0) {
      fprintf(stderr, "restore failed: allocation load\n");
      return EXIT_FAILURE;
    }
    allocation_extra(extra, sizeof(extra), allocations, allocation_count, &phase_start);
    report_phase("load_allocations", &phase_start, participant_count, extra);
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    /* LOAD_ALLOCATIONS publishes every creator descriptor before importers
     * are allowed to request one. */
    if (command_all_parallel(participants, participant_count, CUINTERPOSE_RESTORE_UNICAST) != 0) {
      fprintf(stderr, "restore failed: unicast restore\n");
      return EXIT_FAILURE;
    }
    report_phase("restore_unicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (restore_multicast(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: multicast restore\n");
      return EXIT_FAILURE;
    }
    report_phase("restore_multicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    for (index = 0; index < participant_count; index++) {
      free(participants[index].records);
      participants[index].records = NULL;
      participants[index].count = 0;
    }
    if (inspect(participants, participant_count) != 0 ||
        validate_topology(participants, participant_count, &restored_allocations, &restored_count) != 0 ||
        same_topology(expected.items, expected.count, participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: restored topology does not match the checkpoint\n");
      return EXIT_FAILURE;
    }
    report_phase("validate", &phase_start, participant_count, "");
  }
  return EXIT_SUCCESS;
}
