/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "posix.h"

#include <fcntl.h>
#include <stdbool.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>

#include "util.h"

#define EXPORT_TIMEOUT_SECONDS 30

static bool
zero_bytes(const void* value, size_t size)
{
  const uint8_t* bytes = value;
  size_t index;

  for (index = 0; index < size; index++) {
    if (bytes[index] != 0)
      return false;
  }
  return true;
}

int
cuinterposer_posix_create_ticket(const struct cuinterposer_posix_ticket* ticket, int* output)
{
  const int seals = F_SEAL_WRITE | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL;
  int fd;

  fd = memfd_create("cuinterposer-ticket", MFD_CLOEXEC | MFD_ALLOW_SEALING);
  if (fd < 0 || write_all(fd, ticket, sizeof(*ticket)) != 0 ||
      fcntl(fd, F_ADD_SEALS, seals) != 0) {
    if (fd >= 0)
      close(fd);
    return -1;
  }
  *output = fd;
  return 0;
}

int
cuinterposer_posix_read_ticket(int fd, struct cuinterposer_posix_ticket* ticket)
{
  const int seals = F_SEAL_WRITE | F_SEAL_SHRINK | F_SEAL_GROW | F_SEAL_SEAL;
  struct stat status;

  memset(ticket, 0, sizeof(*ticket));
  if (fd < 0 || fcntl(fd, F_GET_SEALS) != seals || fstat(fd, &status) != 0 ||
      status.st_size != (off_t)sizeof(*ticket) ||
      pread_all(fd, ticket, sizeof(*ticket)) != 0 ||
      ticket->magic != CUINTERPOSER_POSIX_TICKET_MAGIC ||
      ticket->version != CUINTERPOSER_POSIX_TICKET_VERSION ||
      !zero_bytes(ticket->reserved, sizeof(ticket->reserved)) ||
      !is_lower_hex_id(ticket->creator_participant) ||
      zero_bytes(ticket->allocation_id, sizeof(ticket->allocation_id)) ||
      ticket->creator_endpoint[0] != '/' ||
      memchr(ticket->creator_endpoint, '\0', sizeof(ticket->creator_endpoint)) == NULL ||
      zero_bytes(ticket->authorization, sizeof(ticket->authorization)) ||
      !zero_bytes(ticket->reserved_alignment, sizeof(ticket->reserved_alignment)) ||
      (ticket->resource_kind != CUINTERPOSER_RESOURCE_UNICAST &&
       ticket->resource_kind != CUINTERPOSER_RESOURCE_MULTICAST) ||
      !zero_bytes(ticket->reserved_identity, sizeof(ticket->reserved_identity)))
    return -1;
  if (ticket->resource_kind == CUINTERPOSER_RESOURCE_UNICAST &&
      (ticket->num_devices != 0 || ticket->allocation_size != 0 || ticket->handle_types != 0 ||
       ticket->object_flags != 0))
    return -1;
  if (ticket->resource_kind == CUINTERPOSER_RESOURCE_MULTICAST &&
      (ticket->num_devices == 0 || ticket->allocation_size == 0 ||
       ticket->handle_types != CUINTERPOSER_POSIX_HANDLE_TYPE))
    return -1;
  return 0;
}

int
cuinterposer_posix_request_export(
    const struct cuinterposer_posix_ticket* ticket, int* output, char* error, size_t error_size)
{
  struct sockaddr_un address = {.sun_family = AF_UNIX};
  struct cuinterposer_header request;
  struct cuinterposer_header response;
  int client = -1;
  int result = -1;

  *output = -1;
  if (error != NULL && error_size != 0)
    error[0] = '\0';
  memset(&request, 0, sizeof(request));
  request.magic = CUINTERPOSER_MAGIC;
  request.version = CUINTERPOSER_VERSION;
  request.operation = CUINTERPOSER_EXPORT;
  request.resource_kind = ticket->resource_kind;
  snprintf(request.participant_id, sizeof(request.participant_id), "%s", ticket->creator_participant);
  memcpy(request.authorization, ticket->authorization, sizeof(request.authorization));
  memcpy(request.allocation_id, ticket->allocation_id, sizeof(request.allocation_id));
  snprintf(address.sun_path, sizeof(address.sun_path), "%s", ticket->creator_endpoint);
  client = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
  if (client < 0 || set_socket_timeouts(client, EXPORT_TIMEOUT_SECONDS) != 0 ||
      connect(client, (const struct sockaddr*)&address, sizeof(address)) != 0 ||
      send_header(client, &request, -1) != 0 ||
      receive_header(client, &response, output) != 0) {
    if (error != NULL && error_size != 0)
      snprintf(error, error_size, "%s", "cannot contact creator endpoint");
    goto done;
  }
  if (!header_strings_terminated(&response) || response.magic != CUINTERPOSER_MAGIC ||
      response.version != CUINTERPOSER_VERSION || response.operation != CUINTERPOSER_EXPORT || response.count != 0 ||
      response.payload_size != 0 || strcmp(response.participant_id, ticket->creator_participant) != 0 ||
      response.resource_kind != ticket->resource_kind) {
    if (error != NULL && error_size != 0)
      snprintf(error, error_size, "%s", "invalid creator export response");
    if (*output >= 0) {
      close(*output);
      *output = -1;
    }
    goto done;
  }
  if (response.status != 0) {
    if (error != NULL && error_size != 0) {
      if (response.message[0] != '\0')
        snprintf(error, error_size, "%.*s", (int)sizeof(response.message), response.message);
      else
        snprintf(error, error_size, "%s", "creator export request failed");
    }
    if (*output >= 0) {
      close(*output);
      *output = -1;
    }
    goto done;
  }
  if (memcmp(response.allocation_id, ticket->allocation_id, sizeof(response.allocation_id)) != 0 || *output < 0) {
    if (error != NULL && error_size != 0)
      snprintf(error, error_size, "%s", "invalid creator export response");
    if (*output >= 0) {
      close(*output);
      *output = -1;
    }
    goto done;
  }
  result = 0;
done:
  if (client >= 0)
    close(client);
  return result;
}
