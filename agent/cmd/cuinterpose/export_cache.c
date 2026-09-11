/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "export_cache.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "util/table.h"

struct entry {
  int fd;
  unsigned in_flight; /* requests that duplicated fd and have not finished */
  bool draining; /* no new requests; waiting for in_flight to reach zero */
};

static pthread_mutex_t lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t idle = PTHREAD_COND_INITIALIZER;
static struct table entries;
static bool accepting = true;

/* Caller holds lock. Waits until nobody is using the entry, then closes it. */
static void
retire_locked(struct key key, struct entry* entry)
{
  entry->draining = true;
  while (entry->in_flight != 0)
    pthread_cond_wait(&idle, &lock);
  table_remove(&entries, key);
  close(entry->fd);
  free(entry);
}

int
cuinterpose_export_cache_put(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE], int fd)
{
  struct key key = key_bytes(id);
  struct entry* entry = calloc(1, sizeof(*entry));
  struct entry* existing;

  if (entry == NULL)
    return -1;
  entry->fd = fd;
  pthread_mutex_lock(&lock);
  existing = table_get(&entries, key);
  if (existing != NULL)
    retire_locked(key, existing);
  if (table_put(&entries, key, entry) != 0) {
    pthread_mutex_unlock(&lock);
    free(entry);
    return -1;
  }
  pthread_mutex_unlock(&lock);
  return 0;
}

bool
cuinterpose_export_cache_has(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  bool present;

  pthread_mutex_lock(&lock);
  present = table_get(&entries, key_bytes(id)) != NULL;
  pthread_mutex_unlock(&lock);
  return present;
}

int
cuinterpose_export_cache_begin(
    const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE], int* dup, const char** reason)
{
  struct entry* entry;
  int result = -1;

  *dup = -1;
  pthread_mutex_lock(&lock);
  entry = table_get(&entries, key_bytes(id));
  if (!accepting || entry == NULL || entry->draining) {
    *reason = "creator resource is unavailable";
  } else {
    *dup = fcntl(entry->fd, F_DUPFD_CLOEXEC, 0);
    if (*dup < 0) {
      *reason = "cannot duplicate creator descriptor";
    } else {
      entry->in_flight++;
      result = 0;
    }
  }
  pthread_mutex_unlock(&lock);
  return result;
}

void
cuinterpose_export_cache_end(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  struct entry* entry;

  pthread_mutex_lock(&lock);
  entry = table_get(&entries, key_bytes(id));
  if (entry != NULL && entry->in_flight != 0) {
    entry->in_flight--;
    if (entry->in_flight == 0)
      pthread_cond_broadcast(&idle);
  }
  pthread_mutex_unlock(&lock);
}

void
cuinterpose_export_cache_drop(const uint8_t id[CUINTERPOSE_ALLOCATION_ID_SIZE])
{
  struct key key = key_bytes(id);
  struct entry* entry;

  pthread_mutex_lock(&lock);
  entry = table_get(&entries, key);
  if (entry != NULL)
    retire_locked(key, entry);
  pthread_mutex_unlock(&lock);
}

struct collect {
  struct key* keys;
  size_t count;
};

static int
collect_key(struct key key, void* value, void* arg)
{
  struct collect* collect = arg;

  (void)value;
  collect->keys[collect->count++] = key;
  return 0;
}

void
cuinterpose_export_cache_quiesce(void)
{
  struct collect collect = {NULL, 0};
  size_t index;

  pthread_mutex_lock(&lock);
  accepting = false;
  collect.keys = calloc(entries.count == 0 ? 1 : entries.count, sizeof(*collect.keys));
  if (collect.keys != NULL) {
    table_each(&entries, collect_key, &collect);
    for (index = 0; index < collect.count; index++) {
      struct entry* entry = table_get(&entries, collect.keys[index]);
      if (entry != NULL)
        retire_locked(collect.keys[index], entry);
    }
    free(collect.keys);
  }
  pthread_mutex_unlock(&lock);
}

void
cuinterpose_export_cache_resume(void)
{
  pthread_mutex_lock(&lock);
  accepting = true;
  pthread_mutex_unlock(&lock);
}

size_t
cuinterpose_export_cache_count(void)
{
  size_t count;

  pthread_mutex_lock(&lock);
  count = entries.count;
  pthread_mutex_unlock(&lock);
  return count;
}

void
cuinterpose_export_cache_fork_prepare(void)
{
  pthread_mutex_lock(&lock);
}

void
cuinterpose_export_cache_fork_parent(void)
{
  pthread_mutex_unlock(&lock);
}

static int
close_entry(struct key key, void* value, void* arg)
{
  struct entry* entry = value;

  (void)key;
  (void)arg;
  close(entry->fd);
  free(entry);
  return 0;
}

void
cuinterpose_export_cache_fork_child(void)
{
  /* Inherited descriptors belong to the parent's allocations; the child owns
   * no shared memory yet. Locks are re-initialized because the other threads
   * that might have held them do not exist in the child. */
  table_each(&entries, close_entry, NULL);
  table_clear(&entries);
  accepting = true;
  pthread_mutex_init(&lock, NULL);
  pthread_cond_init(&idle, NULL);
}
