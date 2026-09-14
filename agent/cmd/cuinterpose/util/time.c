/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "time.h"

double
elapsed_milliseconds(const struct timespec* start, const struct timespec* end)
{
  return (double)(end->tv_sec - start->tv_sec) * 1e3 +
         (double)(end->tv_nsec - start->tv_nsec) / 1e6;
}

double
elapsed_since_milliseconds(const struct timespec* start)
{
  struct timespec end;

  clock_gettime(CLOCK_MONOTONIC, &end);
  return elapsed_milliseconds(start, &end);
}
