/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_ID_H
#define CUINTERPOSE_UTIL_ID_H

#include <stdbool.h>
#include <stdint.h>
#include <string.h>

#include "protocol.h"

/* Participant identities are fixed-size NUL-terminated buffers. */
static inline bool
id_eq(const char a[CUINTERPOSE_ID_SIZE], const char b[CUINTERPOSE_ID_SIZE])
{
  return memcmp(a, b, CUINTERPOSE_ID_SIZE) == 0;
}

static inline void
id_copy(char destination[CUINTERPOSE_ID_SIZE], const char source[CUINTERPOSE_ID_SIZE])
{
  memcpy(destination, source, CUINTERPOSE_ID_SIZE);
}

/* Allocation identities are fixed-size binary. */
static inline bool
allocation_id_eq(
    const uint8_t a[CUINTERPOSE_ALLOCATION_ID_SIZE], const uint8_t b[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  return memcmp(a, b, CUINTERPOSE_ALLOCATION_ID_SIZE) == 0;
}

static inline void
allocation_id_copy(
    uint8_t destination[CUINTERPOSE_ALLOCATION_ID_SIZE], const uint8_t source[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  memcpy(destination, source, CUINTERPOSE_ALLOCATION_ID_SIZE);
}

/* The lowercase-hex value of one character, or -1 for anything else. */
int hex_digit(int value);
bool is_lower_hex_id(const char value[CUINTERPOSE_ID_SIZE]);
/* Writes 32 lowercase hex characters plus the terminator. */
int random_id(char output[CUINTERPOSE_ID_SIZE]);

#endif
