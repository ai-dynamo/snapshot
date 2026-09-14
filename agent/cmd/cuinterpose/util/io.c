/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "io.h"

#include <errno.h>
#include <stdint.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

int
write_all(int fd, const void* value, size_t size)
{
  const uint8_t* current = value;

  while (size != 0) {
    ssize_t count = write(fd, current, size);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      return -1;
    current += count;
    size -= (size_t)count;
  }
  return 0;
}

int
send_all(int fd, const void* value, size_t size)
{
  const uint8_t* current = value;

  while (size != 0) {
    ssize_t count = send(fd, current, size, MSG_NOSIGNAL);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      return -1;
    current += count;
    size -= (size_t)count;
  }
  return 0;
}

int
read_all(int fd, void* value, size_t size)
{
  uint8_t* current = value;

  while (size != 0) {
    ssize_t count = read(fd, current, size);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      return -1;
    current += count;
    size -= (size_t)count;
  }
  return 0;
}

int
pread_all(int fd, void* value, size_t size)
{
  uint8_t* current = value;
  off_t offset = 0;

  while (size != 0) {
    ssize_t count = pread(fd, current, size, offset);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      return -1;
    current += count;
    offset += count;
    size -= (size_t)count;
  }
  return 0;
}

int
set_socket_timeouts(int fd, unsigned seconds)
{
  struct timeval timeout = {.tv_sec = (time_t)seconds};

  return setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) == 0 &&
                 setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0
             ? 0
             : -1;
}
