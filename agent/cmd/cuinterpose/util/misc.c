/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "misc.h"

#include <errno.h>
#include <stdlib.h>
#include <sys/random.h>

#include "protocol.h"

int
random_bytes(void* output, size_t size)
{
  unsigned char* current = output;

  while (size != 0) {
    ssize_t count = getrandom(current, size, 0);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      return -1;
    current += count;
    size -= (size_t)count;
  }
  return 0;
}

/*
 * Hand-rolled rather than strtol: the shim is compiled with _GNU_SOURCE, which
 * turns on _ISOC23_SOURCE and makes <stdlib.h> route the strtol family to
 * __isoc23_strtol@GLIBC_2.38. That would raise the shim's glibc floor above the
 * 2.34 the Makefile asserts. Trailing garbage is rejected rather than ignored
 * so "3x" cannot silently arm a 3-second timeout.
 */
unsigned
bounded_seconds(const char* value, unsigned fallback)
{
  unsigned long parsed = 0;
  const char* digits;

  if (value == NULL)
    return fallback;
  while (*value == ' ' || *value == '\t' || *value == '\n' || *value == '\r')
    value++;
  for (digits = value; *value >= '0' && *value <= '9'; value++) {
    parsed = parsed * 10 + (unsigned long)(*value - '0');
    if (parsed > 86400)
      return fallback;
  }
  if (value == digits || parsed == 0)
    return fallback;
  while (*value == ' ' || *value == '\t' || *value == '\n' || *value == '\r')
    value++;
  return *value == '\0' ? (unsigned)parsed : fallback;
}

unsigned
cuinterpose_control_timeout_seconds(void)
{
  static unsigned cached;

  if (cached == 0)
    cached = bounded_seconds(getenv(CUINTERPOSE_CONTROL_TIMEOUT_ENV), CUINTERPOSE_CONTROL_TIMEOUT_SECONDS_DEFAULT);
  return cached;
}
