/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "table.h"

#include <stdlib.h>
#include <string.h>

/* A slot that once held an entry; lookups skip it, inserts may reuse it. */
static char tombstone_storage;
#define TOMBSTONE ((void*)&tombstone_storage)

struct key
key_bytes(const uint8_t bytes[16])
{
  struct key key;

  memcpy(&key.low, bytes, sizeof(key.low));
  memcpy(&key.high, bytes + sizeof(key.low), sizeof(key.high));
  return key;
}

static bool
key_equal(struct key a, struct key b)
{
  return a.low == b.low && a.high == b.high;
}

static size_t
key_hash(struct key key)
{
  /* splitmix64 over both words: cheap and well distributed for small counts. */
  uint64_t x = key.low ^ (key.high * 0x9e3779b97f4a7c15ULL);
  x ^= x >> 30;
  x *= 0xbf58476d1ce4e5b9ULL;
  x ^= x >> 27;
  x *= 0x94d049bb133111ebULL;
  x ^= x >> 31;
  return (size_t)x;
}

static int
rehash(struct table* table, size_t capacity)
{
  struct slot* slots = calloc(capacity, sizeof(*slots));
  size_t index;

  if (slots == NULL)
    return -1;
  for (index = 0; index < table->capacity; index++) {
    struct slot* slot = &table->slots[index];
    size_t position;

    if (slot->value == NULL || slot->value == TOMBSTONE)
      continue;
    position = key_hash(slot->key) & (capacity - 1);
    while (slots[position].value != NULL)
      position = (position + 1) & (capacity - 1);
    slots[position] = *slot;
  }
  free(table->slots);
  table->slots = slots;
  table->capacity = capacity;
  table->used = table->count;
  return 0;
}

static struct slot*
find_slot(const struct table* table, struct key key)
{
  size_t position;
  size_t probes;

  if (table->capacity == 0)
    return NULL;
  position = key_hash(key) & (table->capacity - 1);
  for (probes = 0; probes < table->capacity; probes++) {
    struct slot* slot = &table->slots[position];
    if (slot->value == NULL)
      return NULL;
    if (slot->value != TOMBSTONE && key_equal(slot->key, key))
      return slot;
    position = (position + 1) & (table->capacity - 1);
  }
  return NULL;
}

int
table_put(struct table* table, struct key key, void* value)
{
  struct slot* existing = find_slot(table, key);
  size_t position;

  if (existing != NULL) {
    existing->value = value;
    return 0;
  }
  /* Keep the load, including tombstones, under 1/2 so probes stay short. */
  if ((table->used + 1) * 2 > table->capacity) {
    size_t capacity = table->capacity == 0 ? 16 : table->capacity;
    while ((table->count + 1) * 2 > capacity)
      capacity *= 2;
    if (rehash(table, capacity) != 0)
      return -1;
  }
  position = key_hash(key) & (table->capacity - 1);
  while (table->slots[position].value != NULL && table->slots[position].value != TOMBSTONE)
    position = (position + 1) & (table->capacity - 1);
  if (table->slots[position].value == NULL)
    table->used++;
  table->slots[position].key = key;
  table->slots[position].value = value;
  table->count++;
  return 0;
}

void*
table_get(const struct table* table, struct key key)
{
  struct slot* slot = find_slot(table, key);

  return slot == NULL ? NULL : slot->value;
}

void*
table_remove(struct table* table, struct key key)
{
  struct slot* slot = find_slot(table, key);
  void* value;

  if (slot == NULL)
    return NULL;
  value = slot->value;
  slot->value = TOMBSTONE;
  table->count--;
  /* Shrink when mostly empty; on failure the table simply stays large. */
  if (table->count == 0) {
    free(table->slots);
    table->slots = NULL;
    table->capacity = 0;
    table->used = 0;
  } else if (table->capacity > 16 && table->count * 8 < table->capacity) {
    (void)rehash(table, table->capacity / 2);
  }
  return value;
}

int
table_each(
    const struct table* table, int (*fn)(struct key key, void* value, void* arg), void* arg)
{
  size_t index;

  for (index = 0; index < table->capacity; index++) {
    struct slot* slot = &table->slots[index];
    int result;

    if (slot->value == NULL || slot->value == TOMBSTONE)
      continue;
    result = fn(slot->key, slot->value, arg);
    if (result != 0)
      return result;
  }
  return 0;
}

void
table_clear(struct table* table)
{
  free(table->slots);
  memset(table, 0, sizeof(*table));
}

/* Index of the first range whose end is greater than address. */
static size_t
lower_bound(const struct ranges* ranges, uint64_t address)
{
  size_t low = 0;
  size_t high = ranges->count;

  while (low < high) {
    size_t middle = low + (high - low) / 2;
    if (ranges->items[middle].end <= address)
      low = middle + 1;
    else
      high = middle;
  }
  return low;
}

int
ranges_insert(struct ranges* ranges, uint64_t start, uint64_t end, void* value)
{
  size_t index;

  if (end <= start)
    return 1;
  index = lower_bound(ranges, start);
  if (index < ranges->count && ranges->items[index].start < end)
    return 1;
  if (ranges->count == ranges->capacity) {
    size_t capacity = ranges->capacity == 0 ? 16 : ranges->capacity * 2;
    struct range* items = reallocarray(ranges->items, capacity, sizeof(*items));
    if (items == NULL)
      return -1;
    ranges->items = items;
    ranges->capacity = capacity;
  }
  memmove(&ranges->items[index + 1], &ranges->items[index], (ranges->count - index) * sizeof(*ranges->items));
  ranges->items[index].start = start;
  ranges->items[index].end = end;
  ranges->items[index].value = value;
  ranges->count++;
  return 0;
}

struct range*
ranges_at(const struct ranges* ranges, uint64_t address)
{
  size_t index = lower_bound(ranges, address);

  if (index < ranges->count && ranges->items[index].start <= address)
    return &ranges->items[index];
  return NULL;
}

int
ranges_cover(
    const struct ranges* ranges, uint64_t start, uint64_t end, size_t* first, size_t* last)
{
  size_t index = lower_bound(ranges, start);
  size_t stop = index;
  int partial = 0;

  while (stop < ranges->count && ranges->items[stop].start < end) {
    if (ranges->items[stop].start < start || ranges->items[stop].end > end)
      partial = 1;
    stop++;
  }
  *first = index;
  *last = stop;
  return partial;
}

void
ranges_remove_at(struct ranges* ranges, size_t index)
{
  if (index >= ranges->count)
    return;
  memmove(&ranges->items[index], &ranges->items[index + 1], (ranges->count - index - 1) * sizeof(*ranges->items));
  ranges->count--;
  if (ranges->count == 0) {
    free(ranges->items);
    ranges->items = NULL;
    ranges->capacity = 0;
  } else if (ranges->capacity > 16 && ranges->count * 4 < ranges->capacity) {
    struct range* items = reallocarray(ranges->items, ranges->capacity / 2, sizeof(*items));
    if (items != NULL) {
      ranges->items = items;
      ranges->capacity /= 2;
    }
  }
}

void
ranges_clear(struct ranges* ranges)
{
  free(ranges->items);
  memset(ranges, 0, sizeof(*ranges));
}
