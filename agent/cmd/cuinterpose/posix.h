/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSER_POSIX_H
#define CUINTERPOSER_POSIX_H

#include <stddef.h>
#include <stdint.h>
#include <sys/un.h>

#include "protocol.h"

#define CUINTERPOSER_POSIX_TICKET_MAGIC 0x44564d43U
#define CUINTERPOSER_POSIX_TICKET_VERSION 1U

struct cuinterposer_posix_ticket {
  uint32_t magic;
  uint16_t version;
  uint8_t reserved[35];
  char creator_participant[CUINTERPOSER_ID_SIZE];
  uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  char creator_endpoint[sizeof(((struct sockaddr_un*)0)->sun_path)];
  uint8_t authorization[CUINTERPOSER_TOKEN_SIZE];
  uint8_t reserved_identity[42];
};

_Static_assert(sizeof(struct cuinterposer_posix_ticket) == 256, "cuinterposer POSIX ticket layout changed");

/* On success, output receives a ticket FD owned by the caller. */
int cuinterposer_posix_create_ticket(const struct cuinterposer_posix_ticket* ticket, int* output);
/* Reads without taking ownership of fd. */
int cuinterposer_posix_read_ticket(int fd, struct cuinterposer_posix_ticket* ticket);
/* On success, output receives a raw export FD owned by the caller. */
int cuinterposer_posix_request_export(
    const struct cuinterposer_posix_ticket* ticket, int* output, char* error, size_t error_size);

#endif
