/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#include "protocol.h"
#include "util.h"

#define TIMEOUT_SECONDS 30
#define BROKER_TIMEOUT_SECONDS 300
#define STATE_FILENAME "cuinterposer.state"
#define BROKER_STATE_FILENAME "cuinterposer.broker"
#define BROKER_DIRECTORY "/run/snapshot-cuinterposer"

struct participant {
  char* endpoint;
  char id[CUINTERPOSER_ID_SIZE];
  struct cuinterposer_record* records;
  uint32_t count;
};

struct multicast {
  uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  char creator[CUINTERPOSER_ID_SIZE];
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
  uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  char creator[CUINTERPOSER_ID_SIZE];
  uint64_t size;
  bool creator_handle;
  bool creator_mapping;
  bool multicast_member;
  struct allocation* next;
};

struct broker_resource {
  uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  int fd;
  struct broker_resource* next;
};

static int exchange_fd(
    struct participant* participant, uint16_t operation,
    const uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE], int input_fd,
    int* output_fd);
static int command_all(
    struct participant* participants, size_t count, uint16_t operation);

static int
connect_endpoint(const char* endpoint)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  int fd;

  if (strlen(endpoint) >= sizeof(address.sun_path))
    return -1;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", endpoint);
  fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (fd < 0 || set_socket_timeouts(fd, TIMEOUT_SECONDS) != 0 ||
      connect(fd, (const struct sockaddr*)&address, sizeof(address)) != 0) {
    if (fd >= 0)
      close(fd);
    return -1;
  }
  return fd;
}

static void
free_broker_resources(struct broker_resource* resources)
{
  while (resources != NULL) {
    struct broker_resource* next = resources->next;
    if (resources->fd >= 0)
      close(resources->fd);
    free(resources);
    resources = next;
  }
}

static struct participant*
find_participant(struct participant* participants, size_t count, const char* id)
{
  size_t index;

  for (index = 0; index < count; index++) {
    if (strcmp(participants[index].id, id) == 0)
      return &participants[index];
  }
  return NULL;
}

static struct broker_resource*
find_broker_resource(
    struct broker_resource* resources,
    const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  for (; resources != NULL; resources = resources->next) {
    if (memcmp(resources->id, id, CUINTERPOSER_ALLOCATION_ID_SIZE) == 0)
      return resources;
  }
  return NULL;
}

static int
collect_broker_resources(
    struct participant* participants, size_t participant_count,
    struct allocation* allocations, struct broker_resource** output)
{
  struct broker_resource* resources = NULL;
  struct allocation* allocation;

  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    struct participant* creator = find_participant(participants, participant_count, allocation->creator);
    struct broker_resource* resource;

    if (creator == NULL)
      goto failed;
    resource = calloc(1, sizeof(*resource));
    if (resource == NULL)
      goto failed;
    resource->fd = -1;
    memcpy(resource->id, allocation->id, sizeof(resource->id));
    if (exchange_fd(
            creator, CUINTERPOSER_EXPORT_BROKER, allocation->id, -1,
            &resource->fd) != 0) {
      free(resource);
      goto failed;
    }
    resource->next = resources;
    resources = resource;
  }
  *output = resources;
  return 0;
failed:
  free_broker_resources(resources);
  return -1;
}

static int
broker_reply(int client, const struct cuinterposer_header* request, struct broker_resource* resources)
{
  struct cuinterposer_header response = *request;
  struct broker_resource* resource;

  response.status = 0;
  response.message[0] = '\0';
  if (request->operation == CUINTERPOSER_BROKER_GET) {
    resource = find_broker_resource(resources, request->allocation_id);
    if (resource == NULL) {
      header_error(&response, "broker allocation unavailable");
      return send_header(client, &response, -1);
    }
    return send_header(client, &response, resource->fd);
  }
  if (request->operation == CUINTERPOSER_BROKER_CLOSE)
    return send_header(client, &response, -1);
  header_error(&response, "unknown broker operation");
  return send_header(client, &response, -1);
}

static void
broker_loop(int listener, const char* path, struct broker_resource* resources)
{
  bool closed = false;

  (void)setsid();
  close(STDIN_FILENO);
  close(STDOUT_FILENO);
  close(STDERR_FILENO);
  alarm(3600);
  while (!closed) {
    int client = accept4(listener, NULL, NULL, SOCK_CLOEXEC);
    if (client < 0) {
      if (errno == EINTR)
        continue;
      break;
    }
    /*
     * The restore agent connects before CRIU and native CUDA restore. Keep the
     * connection idle long enough for those phases to finish before the
     * coordinator sends its first allocation request.
     */
    if (set_socket_timeouts(client, BROKER_TIMEOUT_SECONDS) == 0) {
      for (;;) {
        struct cuinterposer_header request;
        int passed_fd = -1;

        if (receive_header(client, &request, &passed_fd) != 0)
          break;
        if (passed_fd >= 0)
          close(passed_fd);
        if (passed_fd >= 0 || !header_strings_terminated(&request) ||
            request.magic != CUINTERPOSER_MAGIC ||
            request.version != CUINTERPOSER_VERSION ||
            request.status != 0 || request.count != 0 ||
            request.payload_size != 0 ||
            (request.operation != CUINTERPOSER_BROKER_GET &&
             request.operation != CUINTERPOSER_BROKER_CLOSE) ||
            broker_reply(client, &request, resources) != 0)
          break;
        if (request.operation == CUINTERPOSER_BROKER_CLOSE) {
          closed = true;
          break;
        }
      }
    }
    close(client);
  }
  close(listener);
  unlink(path);
  free_broker_resources(resources);
  _exit(closed ? EXIT_SUCCESS : EXIT_FAILURE);
}

static int
write_broker_state(const char* checkpoint_dir, const char* socket_path)
{
  char path[PATH_MAX];
  char temporary[PATH_MAX];
  int fd;
  int length;
  int result = -1;

  length = snprintf(path, sizeof(path), "%s/%s", checkpoint_dir, BROKER_STATE_FILENAME);
  if (length < 0 || (size_t)length >= sizeof(path))
    return -1;
  length = snprintf(temporary, sizeof(temporary), "%s.tmp.XXXXXX", path);
  if (length < 0 || (size_t)length >= sizeof(temporary))
    return -1;
  fd = mkstemp(temporary);
  if (fd < 0)
    return -1;
  if (dprintf(fd, "%s\n", socket_path) >= 0 && fsync(fd) == 0) {
    if (close(fd) == 0) {
      fd = -1;
      if (rename(temporary, path) == 0)
        result = 0;
    }
  }
  if (fd >= 0)
    close(fd);
  if (result != 0)
    unlink(temporary);
  return result;
}

static int
start_broker(
    const char* checkpoint_dir, struct participant* participants,
    size_t participant_count, struct allocation* allocations,
    pid_t* broker_pid, char socket_path[sizeof(((struct sockaddr_un*)0)->sun_path)])
{
  struct broker_resource* resources = NULL;
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  uint8_t random[16];
  char suffix[33];
  mode_t previous_umask;
  int listener = -1;
  size_t index;
  pid_t pid;

  *broker_pid = -1;
  if (collect_broker_resources(participants, participant_count, allocations, &resources) != 0 ||
      random_bytes(random, sizeof(random)) != 0)
    goto failed;
  for (index = 0; index < sizeof(random); index++)
    snprintf(suffix + index * 2, sizeof(suffix) - index * 2, "%02x", random[index]);
  if (mkdir(BROKER_DIRECTORY, 0700) != 0 && errno != EEXIST)
    goto failed;
  if (snprintf(socket_path, sizeof(address.sun_path), "%s/%s.sock", BROKER_DIRECTORY, suffix) >=
      (int)sizeof(address.sun_path))
    goto failed;
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", socket_path);
  listener = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  previous_umask = umask(0077);
  if (listener < 0 || bind(listener, (const struct sockaddr*)&address, sizeof(address)) != 0 ||
      listen(listener, 4) != 0) {
    umask(previous_umask);
    goto failed;
  }
  umask(previous_umask);
  pid = fork();
  if (pid < 0)
    goto failed;
  if (pid == 0)
    broker_loop(listener, socket_path, resources);
  close(listener);
  listener = -1;
  free_broker_resources(resources);
  resources = NULL;
  if (write_broker_state(checkpoint_dir, socket_path) != 0) {
    kill(pid, SIGTERM);
    (void)waitpid(pid, NULL, 0);
    unlink(socket_path);
    return -1;
  }
  *broker_pid = pid;
  return 0;
failed:
  if (listener >= 0)
    close(listener);
  if (socket_path[0] != '\0')
    unlink(socket_path);
  free_broker_resources(resources);
  return -1;
}

static int
broker_request(int broker_fd, uint16_t operation, const uint8_t* allocation_id, int* output_fd)
{
  struct cuinterposer_header request;
  struct cuinterposer_header response;
  int received_fd = -1;

  if (output_fd != NULL)
    *output_fd = -1;
  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSER_MAGIC;
  request.version = CUINTERPOSER_VERSION;
  request.operation = operation;
  if (allocation_id != NULL)
    memcpy(request.allocation_id, allocation_id, sizeof(request.allocation_id));
  if (send_header(broker_fd, &request, -1) != 0) {
    fprintf(stderr, "broker request %u: send failed\n", operation);
    return -1;
  }
  if (receive_header(broker_fd, &response, &received_fd) != 0) {
    fprintf(stderr, "broker request %u: receive failed\n", operation);
    return -1;
  }
  if (
      !header_strings_terminated(&response) ||
      response.magic != CUINTERPOSER_MAGIC ||
      response.version != CUINTERPOSER_VERSION ||
      response.operation != operation || response.status != 0 ||
      response.count != 0 || response.payload_size != 0 ||
      (allocation_id != NULL &&
       memcmp(response.allocation_id, allocation_id, sizeof(response.allocation_id)) != 0) ||
      ((output_fd != NULL) != (received_fd >= 0))) {
    fprintf(
        stderr, "broker request %u: invalid response status=%d message=%s fd=%d\n",
        operation, response.status, response.message, received_fd);
    if (received_fd >= 0)
      close(received_fd);
    return -1;
  }
  if (output_fd != NULL)
    *output_fd = received_fd;
  return 0;
}

static bool
participant_needs_allocation(
    const struct participant* participant,
    const uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  uint32_t index;

  for (index = 0; index < participant->count; index++) {
    const struct cuinterposer_record* record = &participant->records[index];
    if ((record->kind == CUINTERPOSER_ALLOCATION ||
         record->kind == CUINTERPOSER_MAPPING) &&
        memcmp(record->allocation_id, allocation_id, sizeof(record->allocation_id)) == 0)
      return true;
  }
  return false;
}

static int
restore_brokered(
    struct participant* participants, size_t participant_count,
    struct participant* expected, size_t expected_count,
    struct allocation* allocations, int broker_fd)
{
  struct allocation* allocation;
  size_t participant_index;

  if (broker_fd < 0)
    return -1;
  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    int multicast_member_fd = -1;

    /*
     * Native CUDA restore may reconstruct the creator's local allocation with
     * new backing storage. A multicast member must bind that local allocation
     * (the driver rejects imported members), so peers must import a fresh
     * export of the restored carrier rather than the broker's pre-checkpoint
     * object. This keeps the multicast binding and peer mappings on the same
     * physical allocation without copying device memory.
     */
    if (allocation->multicast_member) {
      struct participant* creator =
          find_participant(participants, participant_count, allocation->creator);
      if (creator == NULL ||
          exchange_fd(
              creator, CUINTERPOSER_EXPORT_BROKER, allocation->id, -1,
              &multicast_member_fd) != 0) {
        fprintf(stderr, "broker restore: restored multicast member export failed\n");
        return -1;
      }
    }
    for (participant_index = 0; participant_index < participant_count; participant_index++) {
      struct participant* expected_participant =
          find_participant(expected, expected_count, participants[participant_index].id);
      int raw_fd = -1;
      if (expected_participant == NULL)
      {
        fprintf(stderr, "broker restore: participant %s missing from expected topology\n", participants[participant_index].id);
        if (multicast_member_fd >= 0)
          close(multicast_member_fd);
        return -1;
      }
      if (!participant_needs_allocation(expected_participant, allocation->id))
        continue;
      /*
       * Imported allocations cannot be passed to cuMulticastBindMem or
       * cuMulticastBindAddr. Keep the creator's local allocation handle through
       * native CUDA checkpoint and restore peer imports from the broker.
       */
      if (allocation->multicast_member &&
          strcmp(participants[participant_index].id, allocation->creator) == 0)
        continue;
      if (multicast_member_fd >= 0) {
        raw_fd = multicast_member_fd;
      } else if (broker_request(broker_fd, CUINTERPOSER_BROKER_GET, allocation->id, &raw_fd) != 0) {
        fprintf(stderr, "broker restore: allocation fetch failed\n");
        return -1;
      }
      if (exchange_fd(
              &participants[participant_index], CUINTERPOSER_RESTORE_BROKERED,
              allocation->id, raw_fd, NULL) != 0) {
        fprintf(
            stderr, "broker restore: participant %s rejected allocation\n",
            participants[participant_index].id);
        if (multicast_member_fd >= 0)
          close(multicast_member_fd);
        else if (raw_fd >= 0)
          close(raw_fd);
        return -1;
      }
      if (multicast_member_fd < 0)
        close(raw_fd);
    }
    if (multicast_member_fd >= 0)
      close(multicast_member_fd);
  }
  if (command_all(participants, participant_count, CUINTERPOSER_RESTORE_BROKERED_COMPLETE) != 0) {
    fprintf(stderr, "broker restore: participant completion failed\n");
    return -1;
  }
  return 0;
}

static int
exchange_fd(
    struct participant* participant, uint16_t operation,
    const uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE], int input_fd, int* output_fd)
{
  struct cuinterposer_header request;
  struct cuinterposer_header response;
  int fd = -1;
  int received_fd = -1;
  int result = -1;

  if (output_fd != NULL)
    *output_fd = -1;
  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSER_MAGIC;
  request.version = CUINTERPOSER_VERSION;
  request.operation = operation;
  snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant->id);
  memcpy(request.allocation_id, allocation_id, sizeof(request.allocation_id));
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || send_header(fd, &request, input_fd) != 0 ||
      receive_header(fd, &response, &received_fd) != 0)
    goto done;
  if (!header_strings_terminated(&response) || response.magic != CUINTERPOSER_MAGIC ||
      response.version != CUINTERPOSER_VERSION || response.operation != operation ||
      response.status != 0 || response.count != 0 || response.payload_size != 0 ||
      strcmp(response.participant_id, participant->id) != 0 ||
      memcmp(response.allocation_id, allocation_id, sizeof(response.allocation_id)) != 0)
    goto done;
  if ((output_fd != NULL) != (received_fd >= 0))
    goto done;
  if (output_fd != NULL) {
    *output_fd = received_fd;
    received_fd = -1;
  }
  result = 0;
done:
  if (received_fd >= 0)
    close(received_fd);
  if (fd >= 0)
    close(fd);
  return result;
}

static struct multicast*
find_multicast(struct multicast* multicasts, const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  struct multicast* multicast;

  for (multicast = multicasts; multicast != NULL; multicast = multicast->next) {
    if (memcmp(multicast->id, id, CUINTERPOSER_ALLOCATION_ID_SIZE) == 0)
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
exchange(struct participant* participant, uint16_t operation, struct cuinterposer_record** records, uint32_t* count)
{
  struct cuinterposer_header request;
  struct cuinterposer_header response;
  uint64_t payload_size;
  int fd = -1;
  int result = -1;
  bool strings_terminated;

  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSER_MAGIC;
  request.version = CUINTERPOSER_VERSION;
  request.operation = operation;
  if (operation != CUINTERPOSER_IDENTIFY)
    snprintf(request.participant_id, sizeof(request.participant_id), "%s", participant->id);
  fd = connect_endpoint(participant->endpoint);
  if (fd < 0 || write_all(fd, &request, sizeof(request)) != 0 ||
      read_all(fd, &response, sizeof(response)) != 0)
    goto done;
  strings_terminated = header_strings_terminated(&response);
  if (!strings_terminated || response.magic != CUINTERPOSER_MAGIC || response.version != CUINTERPOSER_VERSION ||
      response.operation != operation || response.status != 0 || response.count > CUINTERPOSER_MAX_RECORDS ||
      response.payload_size != (uint64_t)response.count * sizeof(struct cuinterposer_record)) {
    if (strings_terminated && response.message[0] != '\0')
      fprintf(stderr, "%s: %s\n", participant->endpoint, response.message);
    goto done;
  }
  if (operation == CUINTERPOSER_IDENTIFY) {
    snprintf(participant->id, sizeof(participant->id), "%s", response.participant_id);
  } else if (strcmp(response.participant_id, participant->id) != 0) {
    goto done;
  }
  payload_size = response.payload_size;
  if (payload_size != 0) {
    *records = calloc(response.count, sizeof(**records));
    if (*records == NULL || read_all(fd, *records, (size_t)payload_size) != 0)
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
find_allocation(struct allocation* allocations, const uint8_t id[CUINTERPOSER_ALLOCATION_ID_SIZE])
{
  struct allocation* allocation;

  for (allocation = allocations; allocation != NULL; allocation = allocation->next) {
    if (memcmp(allocation->id, id, CUINTERPOSER_ALLOCATION_ID_SIZE) == 0)
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
    if (!is_lower_hex_id(participant->id)) {
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
      const struct cuinterposer_record* record = &participant->records[record_index];
      struct allocation* allocation = find_allocation(allocations, record->allocation_id);
      struct multicast* multicast = find_multicast(multicasts, record->allocation_id);

      if (record->kind == CUINTERPOSER_ALLOCATION) {
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
        if ((record->flags & CUINTERPOSER_CREATOR) != 0) {
          if (record->requested_handle_types != CUINTERPOSER_POSIX_HANDLE_TYPE) {
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
          allocation->creator_handle = (record->flags & CUINTERPOSER_APPLICATION_HANDLE_LIVE) != 0;
        }
      } else if (record->kind == CUINTERPOSER_MAPPING) {
        if (record->address == 0) {
          reason = "zero mapping address";
          goto failed;
        }
        if (record->size == 0) {
          reason = "zero mapping size";
          goto failed;
        }
        if (record->access_count > CUINTERPOSER_MAX_ACCESS) {
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
        if ((record->flags & CUINTERPOSER_CREATOR) != 0)
          allocation->creator_mapping = true;
      } else if (record->kind == CUINTERPOSER_MULTICAST) {
        if (record->handle_types != CUINTERPOSER_POSIX_HANDLE_TYPE || record->num_devices == 0 ||
            record->allocation_size == 0 || !is_lower_hex_id(record->creator_participant)) {
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
        if ((record->flags & CUINTERPOSER_CREATOR) != 0) {
          if (strcmp(participant->id, multicast->creator) != 0) {
            reason = "invalid multicast creator";
            goto failed;
          }
          multicast->creators++;
        }
      } else if (record->kind == CUINTERPOSER_MULTICAST_DEVICE) {
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
      } else if (record->kind == CUINTERPOSER_MULTICAST_BINDING) {
        struct allocation* member = find_allocation(allocations, record->member_id);
        struct multicast_device* device;
        if (multicast == NULL || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset ||
            (record->binding_kind != CUINTERPOSER_MULTICAST_BIND_MEM &&
             record->binding_kind != CUINTERPOSER_MULTICAST_BIND_ADDR) ||
            (record->api_version != 1 && record->api_version != 2)) {
          reason = "invalid multicast binding";
          goto failed;
        }
        if ((record->binding_kind == CUINTERPOSER_MULTICAST_BIND_MEM && (member == NULL || record->address != 0)) ||
            (record->binding_kind == CUINTERPOSER_MULTICAST_BIND_ADDR && record->address == 0)) {
          reason = "invalid multicast member";
          goto failed;
        }
        if (member != NULL)
          member->multicast_member = true;
        device = find_multicast_device(multicast, record->device);
        if (device == NULL) {
          reason = "multicast binding device is absent";
          goto failed;
        }
        device->bound = true;
        multicast->bindings++;
      } else if (record->kind == CUINTERPOSER_MULTICAST_MAPPING) {
        if (multicast == NULL || record->address == 0 || record->size == 0 || record->offset > multicast->size ||
            record->size > multicast->size - record->offset || record->access_count > CUINTERPOSER_MAX_ACCESS) {
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
          const struct cuinterposer_record* record = &participant->records[record_index];
          if (record->kind == CUINTERPOSER_MAPPING &&
              memcmp(record->allocation_id, allocation->id, sizeof(allocation->id)) == 0 &&
              (record->offset > allocation->size || record->size > allocation->size - record->offset)) {
            reason = "mapping out of bounds";
            goto failed;
          }
        }
      }
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
  return memcmp(left, right, sizeof(struct cuinterposer_record));
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
  if (fprintf(output, "snapshot-cuda-posix-v2\n") < 0)
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
      for (byte_index = 0; byte_index < sizeof(struct cuinterposer_record); byte_index++) {
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
read_record(FILE* input, struct cuinterposer_record* record)
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

  if (fgets(line, sizeof(line), input) == NULL || strcmp(line, "snapshot-cuda-posix-v2\n") != 0)
    return -1;
  while (fgets(line, sizeof(line), input) != NULL) {
    struct participant* participant;
    struct participant* expanded;
    char id[CUINTERPOSER_ID_SIZE];
    unsigned int record_count;
    unsigned int index;

    if (sscanf(line, "participant %32s %u", id, &record_count) != 2 || !is_lower_hex_id(id) ||
        record_count > CUINTERPOSER_MAX_RECORDS)
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
    struct cuinterposer_record* records = NULL;
    uint32_t record_count = 0;
    if (exchange(&participants[index], CUINTERPOSER_IDENTIFY, &records, &record_count) != 0)
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
            &participants[index], CUINTERPOSER_INSPECT, &participants[index].records, &participants[index].count) != 0)
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

static int
command_all(struct participant* participants, size_t count, uint16_t operation)
{
  size_t index;

  for (index = 0; index < count; index++) {
    struct cuinterposer_record* records = NULL;
    uint32_t record_count = 0;
    if (exchange(&participants[index], operation, &records, &record_count) != 0)
      return -1;
    free(records);
  }
  return 0;
}

static int
command_all_parallel(
    struct participant* participants, size_t count, uint16_t operation)
{
  pid_t* children = NULL;
  size_t started = 0;
  size_t index;
  int result = -1;

  children = calloc(count == 0 ? 1 : count, sizeof(*children));
  if (children == NULL)
    goto done;
  for (index = 0; index < count; index++) {
    children[index] = fork();
    if (children[index] < 0)
      goto join;
    if (children[index] == 0) {
      struct cuinterposer_record* records = NULL;
      uint32_t record_count = 0;
      int child_result =
          exchange(&participants[index], operation, &records, &record_count);

      free(records);
      _exit(child_result == 0 ? EXIT_SUCCESS : EXIT_FAILURE);
    }
    started++;
  }
join:
  result = started == count ? 0 : -1;
  for (index = 0; index < started; index++) {
    int status;

    if (waitpid(children[index], &status, 0) < 0 ||
        !WIFEXITED(status) || WEXITSTATUS(status) != EXIT_SUCCESS)
      result = -1;
  }
done:
  free(children);
  return result;
}

static int
restore_multicast(struct participant* participants, size_t count)
{
  /* Create, then import (same accept/export constraint as unicast). AddDevice on
   * every rank, then bind: BindMem waits for the complete team, and the shim
   * handles one request at a time. */
  if (command_all(participants, count, CUINTERPOSER_RESTORE_MULTICAST_CREATORS) != 0)
    return -1;
  if (command_all(participants, count, CUINTERPOSER_RESTORE_MULTICAST_IMPORTERS) != 0)
    return -1;
  if (command_all(participants, count, CUINTERPOSER_RESTORE_MULTICAST_DEVICES) != 0)
    return -1;
  return command_all_parallel(
      participants, count, CUINTERPOSER_RESTORE_MULTICAST);
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
  size_t participant_count = 0;
  size_t expected_count = 0;
  size_t index;
  bool prepare;
  pid_t broker_pid = -1;
  char broker_socket[sizeof(((struct sockaddr_un*)0)->sun_path)] = {0};
  int broker_fd = -1;
  int result = EXIT_FAILURE;
  FILE* state = NULL;

  char state_path[PATH_MAX];
  const char* proc_root;
  const char* checkpoint_dir;
  const char* control_dir = "/snapshot-control";
  int length;

  if (argc < 9 || (argc - 6) % 3 != 0 || (strcmp(argv[1], "--prepare") != 0 && strcmp(argv[1], "--restore") != 0)) {
    fprintf(
        stderr,
        "usage: %s (--prepare|--restore) --proc-root PATH "
        "--checkpoint-dir PATH --process OBSERVED_PID NAMESPACE_PID...\n",
        argv[0]);
    return EXIT_FAILURE;
  }
  prepare = strcmp(argv[1], "--prepare") == 0;
  if (strcmp(argv[2], "--proc-root") != 0 || strcmp(argv[4], "--checkpoint-dir") != 0)
    return EXIT_FAILURE;
  proc_root = argv[3];
  checkpoint_dir = argv[5];
  if (!prepare) {
    const char* value = getenv("DYN_SNAPSHOT_CUINTERPOSER_BROKER_FD");
    char* end;
    long parsed;
    errno = 0;
    parsed = value == NULL ? -1 : strtol(value, &end, 10);
    if (errno != 0 || value == NULL || *value == '\0' || *end != '\0' ||
        parsed < 0 || parsed > INT_MAX)
      return EXIT_FAILURE;
    broker_fd = (int)parsed;
  }
  length = snprintf(state_path, sizeof(state_path), "%s/%s", checkpoint_dir, STATE_FILENAME);
  if (length < 0 || (size_t)length >= sizeof(state_path))
    return EXIT_FAILURE;
  if (!prepare) {
    state = fopen(state_path, "r");
    if (state == NULL)
      return errno == ENOENT ? EXIT_SUCCESS : EXIT_FAILURE;
  }
  if (proc_root[0] == '\0') {
    const char* control = getenv("SNAPSHOT_CONTROL_DIR");
    if (control == NULL || control[0] == '\0')
      control = getenv("DYN_SNAPSHOT_CONTROL_DIR");
    if (control != NULL && control[0] != '\0')
      control_dir = control;
  }
  participant_count = (size_t)(argc - 6) / 3;
  participants = calloc(participant_count, sizeof(*participants));
  if (participants == NULL)
    goto done;
  for (index = 0; index < participant_count; index++) {
    char endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
    char* end;
    long observed;
    long namespace;
    int length;

    if (strcmp(argv[6 + index * 3], "--process") != 0)
      goto done;
    errno = 0;
    observed = strtol(argv[7 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || observed <= 0 || observed > INT_MAX)
      goto done;
    errno = 0;
    namespace = strtol(argv[8 + index * 3], &end, 10);
    if (errno != 0 || *end != '\0' || namespace <= 0 || namespace > INT_MAX)
      goto done;
    if (proc_root[0] == '\0')
      length =
          snprintf(endpoint, sizeof(endpoint), "%s/%s%ld.sock", control_dir, CUINTERPOSER_SOCKET_PREFIX, namespace);
    else
      length = snprintf(
          endpoint, sizeof(endpoint), "%s/%ld/root%s/%s%ld.sock", proc_root, observed, control_dir,
          CUINTERPOSER_SOCKET_PREFIX, namespace);
    if (length < 0 || (size_t)length >= sizeof(endpoint))
      goto done;
    participants[index].endpoint = strdup(endpoint);
    if (participants[index].endpoint == NULL)
      goto done;
  }
  if (prepare) {
    if (inspect(participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: participant inspect\n");
      goto done;
    }
    if (validate_topology(participants, participant_count, &allocations) != 0) {
      fprintf(stderr, "prepare failed: topology validate\n");
      goto done;
    }
    if (start_broker(
            checkpoint_dir, participants, participant_count, allocations,
            &broker_pid, broker_socket) != 0) {
      fprintf(stderr, "prepare failed: allocation broker\n");
      goto done;
    }
    /* Carriers are local to this request. Every rank must finish multicast
     * teardown before PREPARE unmaps unicast. */
    if (command_all(participants, participant_count, CUINTERPOSER_PREPARE_MULTICAST) != 0) {
      fprintf(stderr, "prepare failed: multicast teardown\n");
      goto done;
    }
    if (command_all(participants, participant_count, CUINTERPOSER_PREPARE) != 0) {
      fprintf(stderr, "prepare failed: participant prepare\n");
      goto done;
    }
    if (write_state_atomic(state_path, participants, participant_count) != 0) {
      fprintf(stderr, "prepare failed: atomic state write\n");
      goto done;
    }
  } else {
    if (read_state(state, &expected, &expected_count) != 0) {
      fprintf(stderr, "restore failed: read checkpoint topology\n");
      goto done;
    }
    if (fclose(state) != 0) {
      state = NULL;
      goto done;
    }
    state = NULL;
    if (identify(participants, participant_count) != 0 ||
        same_participants(expected, expected_count, participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: participant identity mismatch\n");
      goto done;
    }
    if (validate_topology(expected, expected_count, &allocations) != 0) {
      fprintf(stderr, "restore failed: checkpoint topology invalid\n");
      goto done;
    }
    if (command_all(
            participants, participant_count,
            CUINTERPOSER_RESTORE_LOCAL_MULTICAST_MEMBERS) != 0) {
      fprintf(stderr, "restore failed: local multicast member restore\n");
      goto done;
    }
    if (restore_brokered(
            participants, participant_count, expected, expected_count,
            allocations, broker_fd) != 0) {
      fprintf(stderr, "restore failed: brokered unicast restore\n");
      goto done;
    }
    if (restore_multicast(participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: multicast restore\n");
      goto done;
    }
    for (index = 0; index < participant_count; index++) {
      free(participants[index].records);
      participants[index].records = NULL;
      participants[index].count = 0;
    }
    free_allocations(allocations);
    allocations = NULL;
    if (inspect(participants, participant_count) != 0 ||
        validate_topology(participants, participant_count, &allocations) != 0 ||
        same_topology(expected, expected_count, participants, participant_count) != 0) {
      fprintf(stderr, "restore failed: restored topology mismatch\n");
      goto done;
    }
    if (broker_request(broker_fd, CUINTERPOSER_BROKER_CLOSE, NULL, NULL) != 0) {
      fprintf(stderr, "restore failed: broker close\n");
      goto done;
    }
  }
  result = EXIT_SUCCESS;
done:
  if (result != EXIT_SUCCESS && prepare && broker_pid > 0) {
    kill(broker_pid, SIGTERM);
    (void)waitpid(broker_pid, NULL, 0);
    unlink(broker_socket);
  }
  if (state != NULL)
    fclose(state);
  free_allocations(allocations);
  free_participants(expected, expected_count);
  free_participants(participants, participant_count);
  return result;
}
