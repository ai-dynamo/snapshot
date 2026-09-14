/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_TABLE_H
#define CUINTERPOSE_TABLE_H

/*
 * Two small containers for the shim's bookkeeping. Both grow and shrink with
 * use, so a long-running server that maps and unmaps constantly does not
 * accumulate dead entries or slow down over time.
 *
 * cuinterpose_table: open-addressing hash map from a 128-bit key to a pointer.
 * Used for logical handles (key = handle, 0) and allocation ids (16 bytes).
 *
 * cuinterpose_ranges: sorted array of non-overlapping address ranges, each
 * carrying a pointer. Used for mappings, where the questions are "which
 * mapping contains this address" and "which mappings does this range cover".
 */

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

struct cuinterpose_key {
  uint64_t low;
  uint64_t high;
};

struct cuinterpose_slot {
  struct cuinterpose_key key;
  void* value; /* NULL: empty; TOMBSTONE: deleted */
};

struct cuinterpose_table {
  struct cuinterpose_slot* slots;
  size_t capacity; /* power of two, or 0 */
  size_t count; /* live entries */
  size_t used; /* live entries + tombstones */
};

static inline struct cuinterpose_key
cuinterpose_key_u64(uint64_t value)
{
  struct cuinterpose_key key = {value, 0};
  return key;
}

struct cuinterpose_key cuinterpose_key_bytes(const uint8_t bytes[16]);

/* Insert or replace. Returns -1 only when memory allocation fails. */
int cuinterpose_table_put(struct cuinterpose_table* table, struct cuinterpose_key key, void* value);
void* cuinterpose_table_get(const struct cuinterpose_table* table, struct cuinterpose_key key);
/* Returns the removed value, or NULL when absent. */
void* cuinterpose_table_remove(struct cuinterpose_table* table, struct cuinterpose_key key);
/* Calls fn for every live entry; stops early when fn returns non-zero. */
int cuinterpose_table_each(
    const struct cuinterpose_table* table, int (*fn)(struct cuinterpose_key key, void* value, void* arg), void* arg);
void cuinterpose_table_clear(struct cuinterpose_table* table);

struct cuinterpose_range {
  uint64_t start;
  uint64_t end; /* exclusive */
  void* value;
};

struct cuinterpose_ranges {
  struct cuinterpose_range* items;
  size_t count;
  size_t capacity;
};

/* Inserts [start, end). Returns 1 if it overlaps an existing range (nothing
 * inserted), -1 on allocation failure, 0 on success. */
int cuinterpose_ranges_insert(struct cuinterpose_ranges* ranges, uint64_t start, uint64_t end, void* value);
/* The range containing address, or NULL. */
struct cuinterpose_range* cuinterpose_ranges_at(const struct cuinterpose_ranges* ranges, uint64_t address);
/*
 * Classifies [start, end) against the stored ranges. *first and *last receive the
 * index span of ranges that intersect it. Returns 0 when every intersecting
 * range is fully inside [start, end), 1 when some range only partly overlaps
 * it (the request cannot be expressed per range), and 0 with *first == *last
 * when nothing intersects.
 */
int cuinterpose_ranges_cover(
    const struct cuinterpose_ranges* ranges, uint64_t start, uint64_t end, size_t* first, size_t* last);
void cuinterpose_ranges_remove_at(struct cuinterpose_ranges* ranges, size_t index);
void cuinterpose_ranges_clear(struct cuinterpose_ranges* ranges);

#endif
