/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_EXPORT_H
#define CUINTERPOSE_EXPORT_H

#include <stdint.h>

/*
 * The shim is built with -fvisibility=hidden, so nothing leaks into the
 * workload's global symbol table unless it is marked with CUINTERPOSE_API. Only the
 * CUDA entry points the shim replaces, dlsym, and cuinterpose_build_info carry
 * it; the Makefile asserts that no other symbol is exported. An LD_PRELOAD
 * library sits first in symbol lookup for the whole process, so an accidental
 * export named like a common helper would silently hijack another library's
 * call.
 */
#define CUINTERPOSE_API __attribute__((visibility("default")))

/*
 * cuinterpose_build_info lets an operator or a test confirm which CUDA headers
 * the loaded shim was compiled against, for example with
 * `nm -D libcuinterpose.so` or by dlsym()ing the symbol.
 */
struct cuinterpose_build_info {
  uint32_t cuda_version; /* CUDA_VERSION from cuda.h at build time */
  uint32_t protocol_version; /* CUINTERPOSE_VERSION from protocol.h */
};

extern CUINTERPOSE_API const struct cuinterpose_build_info cuinterpose_build_info;

/*
 * Live counters for tests and operators: how much the shim is tracking in this
 * process right now. Reading them takes the shim's lock briefly.
 */
struct cuinterpose_debug_stats {
  uint64_t allocations; /* tracked allocation records */
  uint64_t handles; /* logical handles handed to the application */
  uint64_t mappings; /* tracked mappings */
  uint64_t multicasts; /* tracked multicast objects */
  uint64_t cached_exports; /* descriptors held in the export cache */
  uint64_t live_raw_imports; /* untracked imports not yet released */
  uint64_t passthrough_creations; /* allocations created with non-POSIX handle types */
  uint32_t phase; /* enum cuinterpose_phase */
};

CUINTERPOSE_API void cuinterpose_debug_stats(struct cuinterpose_debug_stats* stats);

#endif
