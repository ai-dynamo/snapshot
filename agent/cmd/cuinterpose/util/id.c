/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "id.h"

#include <stdio.h>

#include "misc.h"

int
hex_digit(int value)
{
  if (value >= '0' && value <= '9')
    return value - '0';
  if (value >= 'a' && value <= 'f')
    return value - 'a' + 10;
  return -1;
}

bool
is_lower_hex_id(const char value[CUINTERPOSE_ID_SIZE])
{
  size_t index;

  if (value == NULL)
    return false;
  for (index = 0; index < CUINTERPOSE_ID_SIZE - 1; index++) {
    if (hex_digit((unsigned char)value[index]) < 0)
      return false;
  }
  return value[CUINTERPOSE_ID_SIZE - 1] == '\0';
}

int
random_id(char output[CUINTERPOSE_ID_SIZE])
{
  uint8_t value[16];
  size_t index;

  if (random_bytes(value, sizeof(value)) != 0)
    return -1;
  for (index = 0; index < sizeof(value); index++)
    snprintf(output + index * 2, CUINTERPOSE_ID_SIZE - index * 2, "%02x", value[index]);
  return 0;
}
