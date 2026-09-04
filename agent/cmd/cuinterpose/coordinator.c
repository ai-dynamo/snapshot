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
#include "util.h"

struct participant {
  char* endpoint;
  char id[CUINTERPOSE_ID_SIZE];
  struct cuinterpose_record* records;
  uint32_t count;
  uint32_t live_raw_imports;
  uint32_t passthrough_creations;
  uint8_t phase;
};

static unsigned
carrier_timeout_seconds(void)
{
  static unsigned cached;

  if (cached == 0)
    cached = cuinterpose_bounded_seconds(
        getenv(CUINTERPOSE_CARRIER_TIMEOUT_ENV), CUINTERPOSE_CARRIER_TIMEOUT_SECONDS_DEFAULT);
  return cached;
}

static double
elapsed_ms(const struct timespec* start)
{
  struct timespec now;

  clock_gettime(CLOCK_MONOTONIC, &now);
  return (double)(now.tv_sec - start->tv_sec) * 1000.0 + (double)(now.tv_nsec - start->tv_nsec) / 1e6;
}

/* One machine-readable progress line per phase; the agent parses key=value pairs. */
static void
report_phase(const char* phase, const struct timespec* start, size_t participants, const char* extra)
{
  printf(
      "cuinterpose-coordinator phase=%s status=ok elapsed_ms=%.1f participants=%zu%s%s\n", phase, elapsed_ms(start),
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
  struct multicast* next;
};

struct multicast_device {
  int32_t device;
  bool bound;
  struct multicast_device* next;
};

struct allocation {
  uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  char creator[CUINTERPOSE_ID_SIZE];
  uint64_t size;
  bool creator_handle;
  bool creator_mapping;
  bool host_carrier;
  struct allocation* next;
};

struct carrier_job {
  struct participant* participant;
  struct allocation* allocations;
  uint16_t operation;
  int result;
  uint32_t copy_us; /* the shim's own timing of its copies */
};

/* Longest copy time any participant reported in the last carrier phase; the
 * participants copy concurrently, so this is the copy's share of the phase. */
static uint32_t last_carrier_copy_us;

static int
connect_endpoint(const char* endpoint)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  int fd;

  if (strlen(endpoint) >= sizeof(address.sun_path))
    return -1;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", endpoint);
  fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0 || cuinterpose_set_socket_timeouts(fd, cuinterpose_control_timeout_seconds()) != 0 ||
      connect(fd, (const struct sockaddr*)&address, sizeof(address)) != 0) {
    if (fd >= 0)
      close(fd);
    return -1;
  }
  return fd;
}

static struct multicast*
find_multicast(struct multicast* multicasts, const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  struct multicast* multicast;

  for (multicast = multicasts; multicast != NULL; multicast = multicast->next) {
    if (memcmp(multicast->id, id, CUINTERPOSE_ALLOCATION_ID_SIZE) == 0)
      return multicast;
  }
  return NULL;
}

static struct multicast_device*
find_multicast_device(struct multicast* multicast, int32_t device)
{
  struct multicast_device* current;

  for (current = multicast->device_list; current != NULL; current = current->next) {
    if (current->device == device)
      return current;
  }
  return NULL;
}

static void
free_multicasts(struct multicast* multicasts)
{
  while (multicasts != NULL) {
    struct multicast* next = multicasts->next;
    while (multicasts->device_list != NULL) {
      struct multicast_device* device_next = multicasts->device_list->next;
      free(multicasts->device_list);
      multicasts->device_list = device_next;
    }
    free(multicasts);
    multicasts = next;
  }
}

static int
exchange(struct participant* participant, uint16_t operation, struct cuinterpose_record** records, uint32_t* count)
{
  struct cuinterpose_header request;
  struct cuinterpose_header response;
  uint64_t payload_size;
  int fd = -1;
  int result = -1;
  bool strings_terminated;

  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  if (operation != CUINTERPOSE_IDENTIFY)
    snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant->id);
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || cuinterpose_send_all(fd, &request, sizeof(request)) != 0 ||
      cuinterpose_read_all(fd, &response, sizeof(response)) != 0)
    goto done;
  strings_terminated = cuinterpose_header_strings_terminated(&response);
  if (!strings_terminated || response.magic != CUINTERPOSE_MAGIC || response.version != CUINTERPOSE_VERSION ||
      response.operation != operation || response.status != 0 || response.count > CUINTERPOSE_MAX_RECORDS ||
      response.payload_size != (uint64_t)response.count * sizeof(struct cuinterpose_record)) {
    if (strings_terminated && response.message[0] != '\0')
      fprintf(stderr, "%s: %s\n", participant->endpoint, response.message);
    goto done;
  }
  if (operation == CUINTERPOSE_IDENTIFY) {
    snprintf(participant->id, sizeof(participant->id), "%s", response.participant_id);
  } else if (strcmp(response.participant_id, participant->id) != 0) {
    goto done;
  }
  if (operation == CUINTERPOSE_IDENTIFY || operation == CUINTERPOSE_INSPECT) {
    participant->live_raw_imports = response.live_raw_imports;
    participant->passthrough_creations = response.passthrough_creations;
    participant->phase = response.phase;
  }
  payload_size = response.payload_size;
  if (payload_size != 0) {
    *records = calloc(response.count, sizeof(**records));
    if (*records == NULL || cuinterpose_read_all(fd, *records, (size_t)payload_size) != 0)
      goto done;
  }
  *count = response.count;
  result = 0;
done:
  if (fd >= 0)
    close(fd);
  if (result != 0) {
    free(*records);
    *records = NULL;
    *count = 0;
  }
  return result;
}

static struct allocation*
find_allocation(struct allocation* allocations, const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  struct allocation* allocation;

  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (memcmp(allocation->id, id, CUINTERPOSE_ALLOCATION_ID_SIZE) == 0)
      return allocation;
  }
  return NULL;
}

static int
validate_topology(struct participant* participants, size_t participant_count, struct allocation** output)
{
  struct allocation* allocations = NULL;
  struct multicast* multicasts = NULL;
  const char* reason = NULL;
  uint64_t value = UINT64_MAX;
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
    if (!cuinterpose_is_lower_hex_id(participant->id)) {
      reason = "invalid participant identity";
      goto failed;
    }
    for (previous = 0; previous < participant_index; previous++) {
      if (strcmp(participants[previous].id, participant->id) == 0) {
        reason = "duplicate participant identity";
        goto failed;
      }
    }
    for (record_index = 0; record_index < participant->count; record_index++) {
      const struct cuinterpose_record* record = &participant->records[record_index];
      struct allocation* allocation = find_allocation(allocations, record->allocation_id);
      struct multicast* multicast = find_multicast(multicasts, record->allocation_id);

      if (record->kind == CUINTERPOSE_ALLOCATION) {
        if (allocation == NULL) {
          allocation = calloc(1, sizeof(*allocation));
          if (allocation == NULL) {
            reason = "allocation metadata allocation failed";
            goto failed;
          }
          memcpy(allocation->id, record->allocation_id, sizeof(allocation->id));
          allocation->next = allocations;
          allocations = allocation;
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0) {
          if (record->requested_handle_types != CUINTERPOSE_POSIX_HANDLE_TYPE) {
            reason = "non-POSIX requested handle type";
            value = record->requested_handle_types;
            goto failed;
          }
          if (record->allocation_size == 0) {
            reason = "zero creator allocation size";
            goto failed;
          }
          if (allocation->creator[0] != '\0' && strcmp(allocation->creator, participant->id) != 0) {
            reason = "conflicting creators";
            goto failed;
          }
          snprintf(allocation->creator, sizeof(allocation->creator), "%s", participant->id);
          allocation->size = record->allocation_size;
          allocation->creator_handle = (record->flags & CUINTERPOSE_APPLICATION_HANDLE_LIVE) != 0;
          allocation->host_carrier = (record->flags & CUINTERPOSE_HOST_CARRIER) != 0;
        } else if ((record->flags & CUINTERPOSE_HOST_CARRIER) != 0) {
          reason = "host carrier flag on importer";
          goto failed;
        }
      } else if (record->kind == CUINTERPOSE_MAPPING) {
        if (record->address == 0) {
          reason = "zero mapping address";
          goto failed;
        }
        if (record->size == 0) {
          reason = "zero mapping size";
          goto failed;
        }
        if (record->access_count > CUINTERPOSE_MAX_ACCESS) {
          reason = "mapping access count exceeds limit";
          value = record->access_count;
          goto failed;
        }
        if (allocation == NULL) {
          allocation = calloc(1, sizeof(*allocation));
          if (allocation == NULL) {
            reason = "allocation metadata allocation failed";
            goto failed;
          }
          memcpy(allocation->id, record->allocation_id, sizeof(allocation->id));
          allocation->next = allocations;
          allocations = allocation;
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0)
          allocation->creator_mapping = true;
      } else if (record->kind == CUINTERPOSE_MULTICAST) {
        if (record->handle_types != CUINTERPOSE_POSIX_HANDLE_TYPE || record->num_devices == 0 ||
            record->allocation_size == 0 || !cuinterpose_is_lower_hex_id(record->creator_participant)) {
          reason = "invalid multicast properties";
          goto failed;
        }
        if (multicast == NULL) {
          multicast = calloc(1, sizeof(*multicast));
          if (multicast == NULL) {
            reason = "multicast metadata allocation failed";
            goto failed;
          }
          memcpy(multicast->id, record->allocation_id, sizeof(multicast->id));
          multicast->size = record->allocation_size;
          multicast->handle_types = record->handle_types;
          multicast->flags = record->object_flags;
          multicast->num_devices = record->num_devices;
          snprintf(multicast->creator, sizeof(multicast->creator), "%s", record->creator_participant);
          multicast->next = multicasts;
          multicasts = multicast;
        } else if (
            multicast->size != record->allocation_size || multicast->handle_types != record->handle_types ||
            multicast->flags != record->object_flags || multicast->num_devices != record->num_devices ||
            strcmp(multicast->creator, record->creator_participant) != 0) {
          reason = "inconsistent multicast properties";
          goto failed;
        }
        if ((record->flags & CUINTERPOSE_CREATOR) != 0) {
          if (strcmp(participant->id, multicast->creator) != 0) {
            reason = "invalid multicast creator";
            goto failed;
          }
          multicast->creators++;
        }
      } else if (record->kind == CUINTERPOSE_MULTICAST_DEVICE) {
        struct multicast_device* device;
        if (multicast == NULL) {
          reason = "multicast device precedes object";
          goto failed;
        }
        if (find_multicast_device(multicast, record->device) != NULL) {
          reason = "duplicate multicast device";
          goto failed;
        }
        device = calloc(1, sizeof(*device));
        if (device == NULL) {
          reason = "multicast device metadata allocation failed";
          goto failed;
        }
        device->device = record->device;
        device->next = multicast->device_list;
        multicast->device_list = device;
        multicast->devices++;
      } else if (record->kind == CUINTERPOSE_MULTICAST_BINDING) {
        struct allocation* member = find_allocation(allocations, record->member_id);
        struct multicast_device* device;
        if (multicast == NULL || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset ||
            (record->binding_kind != CUINTERPOSE_MULTICAST_BIND_MEM &&
             record->binding_kind != CUINTERPOSE_MULTICAST_BIND_ADDR) ||
            (record->api_version != 1 && record->api_version != 2)) {
          reason = "invalid multicast binding";
          goto failed;
        }
        if ((record->binding_kind == CUINTERPOSE_MULTICAST_BIND_MEM && (member == NULL || record->address != 0)) ||
            (record->binding_kind == CUINTERPOSE_MULTICAST_BIND_ADDR && record->address == 0)) {
          reason = "invalid multicast member";
          goto failed;
        }
        device = find_multicast_device(multicast, record->device);
        if (device == NULL) {
          reason = "multicast binding device is absent";
          goto failed;
        }
        device->bound = true;
        multicast->bindings++;
      } else if (record->kind == CUINTERPOSE_MULTICAST_MAPPING) {
        if (multicast == NULL || record->address == 0 || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset || record->access_count > CUINTERPOSE_MAX_ACCESS) {
          reason = "invalid multicast mapping";
          goto failed;
        }
      } else {
        reason = "unknown record kind";
        value = record->kind;
        goto failed;
      }
    }
  }
  /* Whole-topology checks: no single participant or record is at fault. */
  participant_index = participant_count;
  record_index = UINT32_MAX;
  {
    struct allocation* allocation;
    for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
      struct participant* participant;

      if (allocation->creator[0] == '\0') {
        reason = "missing creator";
        goto failed;
      }
      if (allocation->size == 0) {
        reason = "missing allocation size";
        goto failed;
      }
      if (!allocation->creator_handle && !allocation->creator_mapping) {
        reason = "missing creator anchor";
        goto failed;
      }
      for (participant_index = 0; participant_index < participant_count; participant_index++) {
        participant = &participants[participant_index];
        for (record_index = 0; record_index < participant->count; record_index++) {
          const struct cuinterpose_record* record = &participant->records[record_index];
          if (record->kind == CUINTERPOSE_MAPPING &&
              memcmp(record->allocation_id, allocation->id, sizeof(allocation->id)) == 0 &&
              (record->offset > allocation->size || record->size > allocation->size - record->offset)) {
            reason = "mapping out of bounds";
            goto failed;
          }
          /* A multicast binding of this allocation must lie inside it. */
          if (record->kind == CUINTERPOSE_MULTICAST_BINDING && record->binding_kind == CUINTERPOSE_MULTICAST_BIND_MEM &&
              memcmp(record->member_id, allocation->id, sizeof(allocation->id)) == 0 &&
              (record->member_offset > allocation->size || record->size > allocation->size - record->member_offset)) {
            reason = "multicast binding out of member bounds";
            goto failed;
          }
        }
      }
      participant_index = participant_count;
      record_index = UINT32_MAX;
    }
  }
  {
    struct multicast* multicast;
    for (multicast = multicasts; multicast != NULL; multicast = multicast->next) {
      if (multicast->creators != 1) {
        reason = "multicast group must have exactly one creator";
        value = multicast->creators;
        goto failed;
      }
      if (multicast->devices != multicast->num_devices) {
        reason = "incomplete multicast device group";
        value = multicast->devices;
        goto failed;
      }
      if (multicast->bindings < multicast->num_devices) {
        reason = "incomplete multicast binding group";
        value = multicast->bindings;
        goto failed;
      }
      {
        struct multicast_device* device;
        for (device = multicast->device_list; device != NULL; device = device->next) {
          if (!device->bound) {
            reason = "multicast device has no binding";
            value = (uint64_t)(uint32_t)device->device;
            goto failed;
          }
        }
      }
    }
  }
  *output = allocations;
  free_multicasts(multicasts);
  return 0;
failed:
  if (value != UINT64_MAX)
    fprintf(
        stderr, "topology validate failed: %s (value %llu, participant index %zu, record index %u)\n", reason,
        (unsigned long long)value, participant_index, (unsigned)record_index);
  else if (participant_index < participant_count && record_index != UINT32_MAX)
    fprintf(
        stderr, "topology validate failed: %s (participant index %zu, record index %u)\n", reason, participant_index,
        (unsigned)record_index);
  else if (participant_index < participant_count)
    fprintf(stderr, "topology validate failed: %s (participant index %zu)\n", reason, participant_index);
  else
    fprintf(stderr, "topology validate failed: %s\n", reason);
  while (allocations != NULL) {
    struct allocation* next = allocations->next;
    free(allocations);
    allocations = next;
  }
  free_multicasts(multicasts);
  return -1;
}

static int write_state(struct participant* participants, size_t count, FILE* output);

static int
write_state_atomic(const char* path, struct participant* participants, size_t count)
{
  char temporary[PATH_MAX];
  FILE* output = NULL;
  int fd;
  int length;
  int result = -1;

  length = snprintf(temporary, sizeof(temporary), "%s.tmp.XXXXXX", path);
  if (length < 0 || (size_t)length >= sizeof(temporary))
    return -1;
  fd = mkstemp(temporary);
  if (fd < 0)
    return -1;
  output = fdopen(fd, "w");
  if (output == NULL) {
    close(fd);
    unlink(temporary);
    return -1;
  }
  if (write_state(participants, count, output) != 0 || fsync(fileno(output)) != 0)
    goto done;
  if (fclose(output) != 0) {
    output = NULL;
    goto done;
  }
  output = NULL;
  if (rename(temporary, path) != 0)
    goto done;
  /* The rename is only durable once the directory entry itself is on disk. */
  {
    char directory[PATH_MAX];
    char* slash;
    int dir_fd;

    snprintf(directory, sizeof(directory), "%s", path);
    slash = strrchr(directory, '/');
    if (slash != NULL)
      *slash = '\0';
    dir_fd = open(slash != NULL ? directory : ".", O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (dir_fd < 0 || fsync(dir_fd) != 0) {
      if (dir_fd >= 0)
        close(dir_fd);
      goto done;
    }
    close(dir_fd);
  }
  result = 0;
done:
  if (output != NULL)
    fclose(output);
  if (result != 0)
    unlink(temporary);
  return result;
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
  return strcmp(a->id, b->id);
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
hex_digit(int value)
{
  if (value >= '0' && value <= '9')
    return value - '0';
  if (value >= 'a' && value <= 'f')
    return value - 'a' + 10;
  return -1;
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
read_state(FILE* input, struct participant** output, size_t* output_count)
{
  char line[128];
  struct participant* participants = NULL;
  size_t count = 0;

  if (fgets(line, sizeof(line), input) == NULL || strncmp(line, CUINTERPOSE_STATE_HEADER, strlen(CUINTERPOSE_STATE_HEADER)) != 0 ||
      line[strlen(CUINTERPOSE_STATE_HEADER)] != '\n')
    return -1;
  while (fgets(line, sizeof(line), input) != NULL) {
    struct participant* participant;
    struct participant* expanded;
    char id[CUINTERPOSE_ID_SIZE];
    unsigned int record_count;
    unsigned int index;

    if (sscanf(line, "participant %32s %u", id, &record_count) != 2 || !cuinterpose_is_lower_hex_id(id) ||
        record_count > CUINTERPOSE_MAX_RECORDS)
      goto failed;
    expanded = realloc(participants, (count + 1) * sizeof(*participants));
    if (expanded == NULL)
      goto failed;
    participants = expanded;
    participant = &participants[count++];
    memset(participant, 0, sizeof(*participant));
    snprintf(participant->id, sizeof(participant->id), "%s", id);
    participant->records = calloc(record_count == 0 ? 1 : record_count, sizeof(*participant->records));
    if (participant->records == NULL)
      goto failed;
    participant->count = record_count;
    for (index = 0; index < record_count; index++) {
      if (read_record(input, &participant->records[index]) != 0)
        goto failed;
    }
  }
  *output = participants;
  *output_count = count;
  return count == 0 ? -1 : 0;
failed:
  if (participants != NULL) {
    size_t index;
    for (index = 0; index < count; index++) free(participants[index].records);
  }
  free(participants);
  return -1;
}

static int
identify(struct participant* participants, size_t count)
{
  size_t index;

  for (index = 0; index < count; index++) {
    struct cuinterpose_record* records = NULL;
    uint32_t record_count = 0;
    if (exchange(&participants[index], CUINTERPOSE_IDENTIFY, &records, &record_count) != 0)
      return -1;
    free(records);
  }
  return 0;
}

static int
inspect(struct participant* participants, size_t count)
{
  size_t index;

  if (identify(participants, count) != 0)
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
free_allocations(struct allocation* allocations)
{
  while (allocations != NULL) {
    struct allocation* next = allocations->next;
    free(allocations);
    allocations = next;
  }
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
  struct command* commands = NULL;
  pthread_t* threads = NULL;
  size_t started = 0;
  size_t index;
  int result = 0;

  commands = calloc(count, sizeof(*commands));
  threads = calloc(count, sizeof(*threads));
  if (commands == NULL || threads == NULL) {
    result = -1;
    goto done;
  }
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
done:
  free(threads);
  free(commands);
  return result;
}

static int
exchange_carrier(
    struct participant* participant, uint16_t operation, const uint8_t allocation_id[CUINTERPOSE_ALLOCATION_ID_SIZE],
    uint64_t size, uint32_t* copy_us)
{
  struct cuinterpose_header request;
  struct cuinterpose_header response;
  int response_fd = -1;
  int fd = -1;
  int result = -1;

  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSE_MAGIC;
  request.version = CUINTERPOSE_VERSION;
  request.operation = operation;
  request.payload_size = size;
  snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant->id);
  memcpy(request.allocation_id, allocation_id, sizeof(request.allocation_id));
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || cuinterpose_set_socket_timeouts(fd, carrier_timeout_seconds()) != 0 ||
      cuinterpose_send_header(fd, &request, -1) != 0 || cuinterpose_receive_header(fd, &response, &response_fd) != 0)
    goto done;
  if (response_fd >= 0 || !cuinterpose_header_strings_terminated(&response) || response.magic != CUINTERPOSE_MAGIC ||
      response.version != CUINTERPOSE_VERSION || response.operation != operation || response.status != 0 ||
      response.count != 0 || strcmp(response.participant_id, participant->id) != 0) {
    if (cuinterpose_header_strings_terminated(&response) && response.message[0] != '\0')
      fprintf(stderr, "%s: %s\n", participant->endpoint, response.message);
    goto done;
  }
  if (response.payload_size != size) {
    fprintf(
        stderr, "%s: carrier transfer moved %llu bytes, expected %llu\n", participant->endpoint,
        (unsigned long long)response.payload_size, (unsigned long long)size);
    goto done;
  }
  *copy_us = response.copy_us;
  result = 0;
done:
  if (response_fd >= 0)
    close(response_fd);
  if (fd >= 0)
    close(fd);
  return result;
}

/*
 * One request per participant: the shim copies every carrier allocation it
 * owns in one batch (all copies issued on one stream, one wait), and reports
 * the byte count in the reply's payload_size. The expected total is checked
 * against the topology so a shim that silently skipped an allocation fails
 * here rather than at restore.
 */
static void*
run_carrier_job(void* argument)
{
  struct carrier_job* job = argument;
  struct allocation* allocation;
  uint64_t expected = 0;
  uint8_t all_allocations[CUINTERPOSE_ALLOCATION_ID_SIZE] = {0};

  for (allocation = job->allocations; allocation != NULL; allocation = allocation->next) {
    if (allocation->host_carrier && strcmp(allocation->creator, job->participant->id) == 0)
      expected += allocation->size;
  }
  job->result = exchange_carrier(job->participant, job->operation, all_allocations, expected, &job->copy_us);
  return NULL;
}

static int
transfer_host_carriers(
    struct participant* participants, size_t participant_count, struct allocation* allocations,
    uint16_t operation)
{
  struct carrier_job* jobs;
  pthread_t* threads;
  bool* launched;
  size_t index;
  int result = 0;

  last_carrier_copy_us = 0;
  jobs = calloc(participant_count, sizeof(*jobs));
  threads = calloc(participant_count, sizeof(*threads));
  launched = calloc(participant_count, sizeof(*launched));
  if (jobs == NULL || threads == NULL || launched == NULL) {
    result = -1;
    goto done;
  }
  for (index = 0; index < participant_count; index++) {
    struct allocation* allocation;
    bool owns_carrier = false;

    for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
      if (allocation->host_carrier && strcmp(allocation->creator, participants[index].id) == 0) {
        owns_carrier = true;
        break;
      }
    }
    if (!owns_carrier)
      continue;
    jobs[index].participant = &participants[index];
    jobs[index].allocations = allocations;
    jobs[index].operation = operation;
    if (pthread_create(&threads[index], NULL, run_carrier_job, &jobs[index]) != 0) {
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
    else if (jobs[index].copy_us > last_carrier_copy_us)
      last_carrier_copy_us = jobs[index].copy_us;
  }
done:
  free(launched);
  free(threads);
  free(jobs);
  return result;
}

static int
save_host_carriers(
    struct participant* participants, size_t participant_count, struct allocation* allocations)
{
  return transfer_host_carriers(
      participants, participant_count, allocations, CUINTERPOSE_SAVE_HOST_CARRIER);
}

static int
restore_host_carriers(
    struct participant* participants, size_t participant_count, struct allocation* allocations)
{
  return transfer_host_carriers(
      participants, participant_count, allocations, CUINTERPOSE_RESTORE_HOST_CARRIER);
}

static int
restore_unicast(struct participant* participants, size_t count)
{
  /* Creators must have re-exported and be listening again before any importer
   * asks them for a fresh descriptor, hence the barrier between the two. */
  if (command_all_parallel(participants, count, CUINTERPOSE_RESTORE_CREATORS) != 0)
    return -1;
  return command_all_parallel(participants, count, CUINTERPOSE_RESTORE_IMPORTERS);
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
  return command_all_parallel(participants, count, CUINTERPOSE_RESTORE_MULTICAST);
}

/* Sum of host-carrier bytes and count, for the progress lines. */
static void
carrier_totals(const struct allocation* allocations, size_t* count, uint64_t* bytes)
{
  const struct allocation* allocation;

  *count = 0;
  *bytes = 0;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (allocation->host_carrier) {
      (*count)++;
      *bytes += allocation->size;
    }
  }
}

static void
carrier_extra(char* buffer, size_t size, const struct allocation* allocations, const struct timespec* start)
{
  size_t count;
  uint64_t bytes;
  double ms = elapsed_ms(start);

  carrier_totals(allocations, &count, &bytes);
  /* gb_per_s covers the whole phase (memory setup, copies, teardown, sockets);
   * copy_gb_per_s is the copies alone, as timed by the slowest shim. */
  snprintf(
      buffer, size, "carrier_count=%zu carrier_bytes=%llu gb_per_s=%.2f copy_gb_per_s=%.2f", count,
      (unsigned long long)bytes, ms > 0.0 ? ((double)bytes / 1e9) / (ms / 1000.0) : 0.0,
      last_carrier_copy_us > 0 ? ((double)bytes / 1e9) / ((double)last_carrier_copy_us / 1e6) : 0.0);
}

/* Prepare must not start while any participant still holds an untracked import:
 * native restore would give that process a private copy and silently break the
 * sharing. */
static int
refuse_live_raw_imports(const struct participant* participants, size_t count)
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
    if (strcmp(expected[index].id, actual[index].id) != 0)
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
  struct participant* participants = NULL;
  struct participant* expected = NULL;
  struct allocation* allocations = NULL;
  struct allocation* restored_allocations = NULL;
  size_t participant_count = 0;
  size_t expected_count = 0;
  size_t index;
  bool prepare;
  int result = EXIT_FAILURE;
  FILE* state = NULL;
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
        "               PATH/<observed-pid>/root (prepare, run from the agent); empty when running\n"
        "               inside the container's own mount namespace (restore).\n"
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
  participant_count = (size_t)(argc - 8) / 3;
  participants = calloc(participant_count, sizeof(*participants));
  if (participants == NULL)
    goto done;
  for (index = 0; index < participant_count; index++) {
    char endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
    char* end;
    long observed;
    long namespace;
    int length;

    if (strcmp(argv[8 + index * 3], "--process") != 0)
      goto done;
    errno = 0;
    observed = strtol(argv[9 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || observed <= 0 || observed > INT_MAX)
      goto done;
    errno = 0;
    namespace = strtol(argv[10 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || namespace <= 0 || namespace > INT_MAX)
      goto done;
    if (proc_root[0] == '\0')
      length = snprintf(endpoint, sizeof(endpoint), "%s/%s%ld.sock", control_dir, CUINTERPOSE_SOCKET_PREFIX, namespace);
    else
      length = snprintf(
          endpoint, sizeof(endpoint), "%s/%ld/root%s/%s%ld.sock", proc_root, observed, control_dir,
          CUINTERPOSE_SOCKET_PREFIX, namespace);
    if (length < 0 || (size_t)length >= sizeof(endpoint)) {
      fprintf(stderr, "control socket path for process %ld does not fit in sun_path\n", observed);
      goto done;
    }
    participants[index].endpoint = strdup(endpoint);
    if (participants[index].endpoint == NULL)
      goto done;
  }
  if (prepare) {
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (inspect(participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: participant inspect\n");
      goto done;
    }
    {
      size_t records = 0;
      uint32_t raw_imports = 0;
      uint32_t passthrough = 0;
      for (index = 0; index < participant_count; index++) {
        records += participants[index].count;
        raw_imports += participants[index].live_raw_imports;
        passthrough += participants[index].passthrough_creations;
      }
      snprintf(
          extra, sizeof(extra), "records=%zu live_raw_imports=%u passthrough_creations=%u", records, raw_imports,
          passthrough);
    }
    report_phase("inspect", &phase_start, participant_count, extra);
    if (refuse_live_raw_imports(participants, participant_count) != 0)
      goto done;
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (validate_topology(participants, participant_count, &allocations) != 0) {
      fprintf(stderr, "prepare failed: topology validate\n");
      goto done;
    }
    report_phase("validate", &phase_start, participant_count, "");
    /* Every rank must finish multicast teardown before PREPARE unmaps unicast
     * memory: multicast bindings sit on top of the unicast allocations. */
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (command_all_parallel(participants, participant_count, CUINTERPOSE_PREPARE_MULTICAST) != 0) {
      fprintf(stderr, "prepare failed: multicast teardown\n");
      goto done;
    }
    report_phase("prepare_multicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (save_host_carriers(participants, participant_count, allocations) != 0) {
      fprintf(stderr, "prepare failed: host carrier save\n");
      goto done;
    }
    carrier_extra(extra, sizeof(extra), allocations, &phase_start);
    report_phase("save_host_carrier", &phase_start, participant_count, extra);
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (command_all_parallel(participants, participant_count, CUINTERPOSE_PREPARE) != 0) {
      fprintf(stderr, "prepare failed: participant prepare\n");
      goto done;
    }
    report_phase("prepare", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (write_state_atomic(state_path, participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: atomic state write\n");
      goto done;
    }
    report_phase("state_write", &phase_start, participant_count, "");
  } else {
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (read_state(state, &expected, &expected_count) != 0) {
      fprintf(stderr, "restore failed: cannot parse %s\n", state_path);
      goto done;
    }
    if (fclose(state) != 0) {
      state = NULL;
      goto done;
    }
    state = NULL;
    if (identify(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: participant identify\n");
      goto done;
    }
    if (same_participants(expected, expected_count, participants, participant_count) != 0) {
      fprintf(
          stderr, "restore failed: the restored processes (%zu) do not match the checkpointed participants (%zu)\n",
          participant_count, expected_count);
      goto done;
    }
    if (validate_topology(expected, expected_count, &allocations) != 0) {
      fprintf(stderr, "restore failed: checkpointed topology is invalid\n");
      goto done;
    }
    report_phase("identify", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (restore_host_carriers(participants, participant_count, allocations) != 0) {
      fprintf(stderr, "restore failed: host carrier restore\n");
      goto done;
    }
    carrier_extra(extra, sizeof(extra), allocations, &phase_start);
    report_phase("restore_host_carrier", &phase_start, participant_count, extra);
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (restore_unicast(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: unicast restore\n");
      goto done;
    }
    report_phase("restore_unicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    if (restore_multicast(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: multicast restore\n");
      goto done;
    }
    report_phase("restore_multicast", &phase_start, participant_count, "");
    clock_gettime(CLOCK_MONOTONIC, &phase_start);
    for (index = 0; index < participant_count; index++) {
      free(participants[index].records);
      participants[index].records = NULL;
      participants[index].count = 0;
    }
    if (inspect(participants, participant_count) != 0 ||
        validate_topology(participants, participant_count, &restored_allocations) != 0 ||
        same_topology(expected, expected_count, participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: restored topology does not match the checkpoint\n");
      goto done;
    }
    report_phase("validate", &phase_start, participant_count, "");
  }
  result = EXIT_SUCCESS;
done:
  if (state != NULL)
    fclose(state);
  free_allocations(restored_allocations);
  free_allocations(allocations);
  free_participants(expected, expected_count);
  free_participants(participants, participant_count);
  return result;
}
