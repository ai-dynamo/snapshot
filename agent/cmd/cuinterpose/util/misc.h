/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_MISC_H
#define CUINTERPOSE_UTIL_MISC_H

#include <stddef.h>

/*
 * Odds and ends with no better home. When two helpers here share a theme they
 * graduate to a file of their own; io, id, cleanup, and table all started as
 * recognizable themes rather than growing out of this file.
 */

int random_bytes(void* output, size_t size);
/* Parses a whole-second override; anything malformed yields `fallback`. */
unsigned bounded_seconds(const char* value, unsigned fallback);
/* CUINTERPOSE_CONTROL_TIMEOUT_ENV or the default, read once. The prefix is
 * deliberate: this is cuinterpose policy (one environment variable), shared
 * from here because the shim and the coordinator both read it. */
unsigned cuinterpose_control_timeout_seconds(void);

#endif
