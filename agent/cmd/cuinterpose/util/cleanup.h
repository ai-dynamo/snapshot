/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_CLEANUP_H
#define CUINTERPOSE_UTIL_CLEANUP_H

#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

/*
 * Scoped cleanup: FUNCTION receives the variable's address on every exit from
 * its scope, however the scope is left. Transfer ownership out of a cleanup
 * scope with take_fd. This is the one macro in util/: the cleanup attribute
 * exists only on declarations, so no function can express it; everything that
 * can be a function below is one.
 *
 *   CUINTERPOSE_CLEANUP(close_fd) int fd = open(...);
 */
#define CUINTERPOSE_CLEANUP(FUNCTION) __attribute__((cleanup(FUNCTION)))

static inline void
close_fd(int* fd)
{
  if (*fd >= 0)
    close(*fd);
}

/* Hands the descriptor to the caller and disarms the cleanup variable. */
static inline int
take_fd(int* fd)
{
  int taken = *fd;

  *fd = -1;
  return taken;
}

static inline void
free_ptr(void* pointer)
{
  free(*(void**)pointer);
}

static inline void
close_file(FILE** file)
{
  if (*file != NULL)
    fclose(*file);
}

/*
 * Removes a temporary file at scope exit unless disarmed; backs the
 * write-temporary-then-rename protocol:
 *
 *   CUINTERPOSE_CLEANUP(unlink_guard_release) struct unlink_guard guard = {temporary, true};
 *   ... write, fsync, rename ...
 *   guard.armed = false;
 */
struct unlink_guard {
  const char* path;
  bool armed;
};

static inline void
unlink_guard_release(struct unlink_guard* guard)
{
  if (guard->armed && guard->path != NULL)
    unlink(guard->path);
}

#endif
