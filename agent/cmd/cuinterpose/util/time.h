/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_TIME_H
#define CUINTERPOSE_UTIL_TIME_H

#include <time.h>

double elapsed_milliseconds(const struct timespec* start, const struct timespec* end);
double elapsed_since_milliseconds(const struct timespec* start);

#endif
