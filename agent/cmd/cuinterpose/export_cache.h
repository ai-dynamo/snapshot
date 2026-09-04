/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_EXPORT_CACHE_H
#define CUINTERPOSE_EXPORT_CACHE_H

/*
 * The export cache holds, for every shared allocation this process created,
 * the real file descriptor the driver produced when the allocation was first
 * exported. The listener thread answers a peer's request by duplicating that
 * descriptor. It never calls into the driver and never takes the shim's main
 * lock, so a creator that is busy inside a long driver call cannot make its
 * peers wait.
 *
 * Lock order is state_lock (interpose.c) -> the cache's own lock. The listener
 * takes only the cache lock. A descriptor is closed only after every in-flight
 * request that duplicated it has finished, so a request can never duplicate a
 * descriptor number that has been closed and reused for something else.
 */

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "protocol.h"

/* Store fd (ownership passes to the cache) for the allocation. Replaces an
 * earlier entry for the same id after draining requests that use it. */
int cuinterpose_export_cache_put(
    const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE], const uint8_t authorization[CUINTERPOSE_TOKEN_SIZE], int fd);
/* Does the cache hold a descriptor for this allocation? */
bool cuinterpose_export_cache_has(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE]);
/*
 * Begin serving a peer: on success *dup is a fresh close-on-exec duplicate the
 * caller must close, and the entry is pinned until cuinterpose_export_cache_end.
 * Returns -1 with a short reason when the id or authorization is unknown, or
 * the cache is not accepting requests.
 */
int cuinterpose_export_cache_begin(
    const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE], const uint8_t authorization[CUINTERPOSE_TOKEN_SIZE], int* dup,
    const char** reason);
void cuinterpose_export_cache_end(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE]);
/* Drop one allocation's descriptor, waiting for its in-flight requests. */
void cuinterpose_export_cache_drop(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE]);
/* Stop accepting requests, wait for all in flight, close every descriptor. */
void cuinterpose_export_cache_quiesce(void);
/* Accept requests again (after restore has refilled the cache). */
void cuinterpose_export_cache_resume(void);
size_t cuinterpose_export_cache_count(void);

/* pthread_atfork hooks: the parent holds the lock across fork; the child owns
 * none of the inherited descriptors and starts empty. */
void cuinterpose_export_cache_fork_prepare(void);
void cuinterpose_export_cache_fork_parent(void);
void cuinterpose_export_cache_fork_child(void);

#endif
