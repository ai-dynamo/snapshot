/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSER_PROTOCOL_H
#define CUINTERPOSER_PROTOCOL_H

#include <stdint.h>

#define CUINTERPOSER_MAGIC 0x44564d4dU
#define CUINTERPOSER_VERSION 1U
#define CUINTERPOSER_SOCKET_PREFIX "cuinterposer-"
#define CUINTERPOSER_ID_SIZE 33U
#define CUINTERPOSER_ALLOCATION_ID_SIZE 16U
#define CUINTERPOSER_TOKEN_SIZE 16U
#define CUINTERPOSER_MAX_ACCESS 8U
#define CUINTERPOSER_MAX_RECORDS 4096U
#define CUINTERPOSER_POSIX_HANDLE_TYPE 1U

enum cuinterposer_operation {
  CUINTERPOSER_IDENTIFY = 1,
  CUINTERPOSER_INSPECT = 2,
  CUINTERPOSER_PREPARE = 3,
  CUINTERPOSER_RESTORE_CREATORS = 4,
  CUINTERPOSER_RESTORE_IMPORTERS = 5,
  CUINTERPOSER_EXPORT = 6,
};

enum cuinterposer_record_kind {
  CUINTERPOSER_ALLOCATION = 1,
  CUINTERPOSER_MAPPING = 2,
};

enum cuinterposer_record_flags {
  CUINTERPOSER_CREATOR = 1U << 0,
  CUINTERPOSER_APPLICATION_HANDLE_LIVE = 1U << 1,
};

struct cuinterposer_header {
  uint32_t magic;
  uint16_t version;
  uint16_t operation;
  int32_t status;
  uint32_t count;
  uint64_t payload_size;
  char participant_id[CUINTERPOSER_ID_SIZE];
  char message[96];
  uint8_t authorization[CUINTERPOSER_TOKEN_SIZE];
  uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  uint8_t reserved[71];
};

struct cuinterposer_access {
  int32_t location_type;
  int32_t location_id;
  uint64_t flags;
};

struct cuinterposer_record {
  uint32_t kind;
  uint32_t flags;
  uint8_t allocation_id[CUINTERPOSER_ALLOCATION_ID_SIZE];
  uint64_t address;
  uint64_t size;
  uint64_t offset;
  uint64_t allocation_size;
  int32_t allocation_type;
  uint32_t requested_handle_types;
  int32_t allocation_location_type;
  int32_t allocation_location_id;
  uint32_t access_count;
  uint32_t application_handle_count;
  struct cuinterposer_access access[CUINTERPOSER_MAX_ACCESS];
};

_Static_assert(sizeof(struct cuinterposer_header) == 256, "cuinterposer header layout changed");
_Static_assert(sizeof(struct cuinterposer_access) == 16, "cuinterposer access layout changed");
_Static_assert(sizeof(struct cuinterposer_record) == 208, "cuinterposer record layout changed");

#endif
