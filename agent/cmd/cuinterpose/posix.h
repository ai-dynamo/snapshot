/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_POSIX_H
#define CUINTERPOSE_POSIX_H

#include <stddef.h>
#include <stdint.h>
#include <sys/un.h>

#include "protocol.h"

/*
 * Tickets. When an application exports a tracked allocation, the shim hands it
 * a "ticket" instead of the driver's file descriptor: a sealed memfd holding
 * this struct. The application passes the ticket to another process exactly as
 * it would pass a real fd. When that process imports it, the shim there reads
 * the ticket, connects to the creator's control socket named inside it, and
 * asks for the real fd. The authorization bytes prove the importer holds the
 * ticket; the creator refuses requests without them.
 */

#define CUINTERPOSE_POSIX_TICKET_MAGIC 0x44564d43U
#define CUINTERPOSE_POSIX_TICKET_VERSION 2U

struct cuinterpose_posix_ticket {
  uint32_t magic;
  uint16_t version;
  uint8_t reserved[35];
  char creator_participant[CUINTERPOSE_ID_SIZE];
  uint8_t allocation_id[CUINTERPOSE_ALLOCATION_ID_SIZE];
  char creator_endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
  uint8_t authorization[CUINTERPOSE_TOKEN_SIZE];
  uint8_t reserved_alignment[2];
  uint32_t resource_kind;
  uint32_t num_devices;
  uint64_t allocation_size;
  uint64_t handle_types;
  uint64_t object_flags;
  uint8_t reserved_identity[8];
};

CUINTERPOSE_STATIC_ASSERT(sizeof(struct cuinterpose_posix_ticket) == 256, "cuinterpose POSIX ticket layout changed");

/* On success, *output is a sealed memfd owned by the caller. */
int cuinterpose_posix_create_ticket(const struct cuinterpose_posix_ticket* ticket, int* output);
/* Validates and reads a ticket without taking ownership of fd. Returns -1 for
 * anything that is not a well-formed ticket, including real driver fds. */
int cuinterpose_posix_read_ticket(int fd, struct cuinterpose_posix_ticket* ticket);
/* Asks the creator named in ticket for the real fd. On success *output is the
 * raw driver fd owned by the caller; on failure *output is -1 and, when error
 * is non-NULL, a short reason is written into it. */
int cuinterpose_posix_request_export(
    const struct cuinterpose_posix_ticket* ticket, int* output, char* error, size_t error_size);

#endif
