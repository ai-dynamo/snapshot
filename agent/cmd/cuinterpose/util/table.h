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
 * table: open-addressing hash map from a 128-bit key to a pointer.
 * Used for logical handles (key = handle, 0) and allocation ids (16 bytes).
 *
 * ranges: sorted array of non-overlapping address ranges, each
 * carrying a pointer. Used for mappings, where the questions are "which
 * mapping contains this address" and "which mappings does this range cover".
 */

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

struct key {
  uint64_t low;
  uint64_t high;
};

struct slot {
  struct key key;
  void* value; /* NULL: empty; TOMBSTONE: deleted */
};

struct table {
  struct slot* slots;
  size_t capacity; /* power of two, or 0 */
  size_t count; /* live entries */
  size_t used; /* live entries + tombstones */
};

static inline struct key
key_u64(uint64_t value)
{
  struct key key = {value, 0};
  return key;
}

struct key key_bytes(const uint8_t bytes[16]);

/* Insert or replace. Returns -1 only when memory allocation fails. */
int table_put(struct table* table, struct key key, void* value);
void* table_get(const struct table* table, struct key key);
/* Returns the removed value, or NULL when absent. */
void* table_remove(struct table* table, struct key key);
/* Calls fn for every live entry; stops early when fn returns non-zero. */
int table_each(
    const struct table* table, int (*fn)(struct key key, void* value, void* arg), void* arg);
void table_clear(struct table* table);

struct range {
  uint64_t start;
  uint64_t end; /* exclusive */
  void* value;
};

struct ranges {
  struct range* items;
  size_t count;
  size_t capacity;
};

/* Inserts [start, end). Returns 1 if it overlaps an existing range (nothing
 * inserted), -1 on allocation failure, 0 on success. */
int ranges_insert(struct ranges* ranges, uint64_t start, uint64_t end, void* value);
/* The range containing address, or NULL. */
struct range* ranges_at(const struct ranges* ranges, uint64_t address);
/*
 * Classifies [start, end) against the stored ranges. *first and *last receive the
 * index span of ranges that intersect it. Returns 0 when every intersecting
 * range is fully inside [start, end), 1 when some range only partly overlaps
 * it (the request cannot be expressed per range), and 0 with *first == *last
 * when nothing intersects.
 */
int ranges_cover(
    const struct ranges* ranges, uint64_t start, uint64_t end, size_t* first, size_t* last);
void ranges_remove_at(struct ranges* ranges, size_t index);
void ranges_clear(struct ranges* ranges);

#endif
