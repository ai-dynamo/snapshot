/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "util.h"

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/random.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

int
cuinterpose_write_all(int fd, const void* value, size_t size)
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
cuinterpose_send_all(int fd, const void* value, size_t size)
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
cuinterpose_read_all(int fd, void* value, size_t size)
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
cuinterpose_pread_all(int fd, void* value, size_t size)
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
cuinterpose_random_bytes(void* output, size_t size)
{
  unsigned char* current = output;

  while (size != 0) {
    ssize_t count = getrandom(current, size, 0);
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
cuinterpose_random_id(char output[CUINTERPOSE_ID_SIZE])
{
  uint8_t value[16];
  size_t index;

  if (cuinterpose_random_bytes(value, sizeof(value)) != 0)
    return -1;
  for (index = 0; index < sizeof(value); index++)
    snprintf(output + index * 2, CUINTERPOSE_ID_SIZE - index * 2, "%02x", value[index]);
  return 0;
}

bool
cuinterpose_is_lower_hex_id(const char value[CUINTERPOSE_ID_SIZE])
{
  size_t index;

  if (value == NULL)
    return false;
  for (index = 0; index < CUINTERPOSE_ID_SIZE - 1; index++) {
    if (!((value[index] >= '0' && value[index] <= '9') || (value[index] >= 'a' && value[index] <= 'f')))
      return false;
  }
  return value[CUINTERPOSE_ID_SIZE - 1] == '\0';
}

bool
cuinterpose_header_strings_terminated(const struct cuinterpose_header* header)
{
  return memchr(header->participant_id, '\0', sizeof(header->participant_id)) != NULL &&
         memchr(header->message, '\0', sizeof(header->message)) != NULL;
}

void
cuinterpose_header_error(struct cuinterpose_header* header, const char* message)
{
  header->status = -1;
  snprintf(header->message, sizeof(header->message), "%s", message);
}

int
cuinterpose_set_socket_timeouts(int fd, unsigned seconds)
{
  struct timeval timeout = {.tv_sec = (time_t)seconds};

  return setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)) == 0 &&
                 setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0
             ? 0
             : -1;
}

/*
 * Hand-rolled rather than strtol: the shim is compiled with _GNU_SOURCE, which
 * turns on _ISOC23_SOURCE and makes <stdlib.h> route the strtol family to
 * __isoc23_strtol@GLIBC_2.38. That would raise the shim's glibc floor above the
 * 2.34 the Makefile asserts. Trailing garbage is rejected rather than ignored
 * so "3x" cannot silently arm a 3-second timeout.
 */
unsigned
cuinterpose_bounded_seconds(const char* value, unsigned fallback)
{
  unsigned long parsed = 0;
  const char* digits;

  if (value == NULL)
    return fallback;
  while (*value == ' ' || *value == '\t' || *value == '\n' || *value == '\r')
    value++;
  for (digits = value; *value >= '0' && *value <= '9'; value++) {
    parsed = parsed * 10 + (unsigned long)(*value - '0');
    if (parsed > 86400)
      return fallback;
  }
  if (value == digits || parsed == 0)
    return fallback;
  while (*value == ' ' || *value == '\t' || *value == '\n' || *value == '\r')
    value++;
  return *value == '\0' ? (unsigned)parsed : fallback;
}

unsigned
cuinterpose_control_timeout_seconds(void)
{
  static unsigned cached;

  if (cached == 0)
    cached = cuinterpose_bounded_seconds(
        getenv(CUINTERPOSE_CONTROL_TIMEOUT_ENV), CUINTERPOSE_CONTROL_TIMEOUT_SECONDS_DEFAULT);
  return cached;
}

int
cuinterpose_send_header(int fd, const struct cuinterpose_header* header, int passed_fd)
{
  char control[CMSG_SPACE(sizeof(int))] = {0};
  struct iovec vector = {.iov_base = (void*)header, .iov_len = sizeof(*header)};
  struct msghdr message = {.msg_iov = &vector, .msg_iovlen = 1};
  ssize_t count;

  if (passed_fd >= 0) {
    struct cmsghdr* item;
    message.msg_control = control;
    message.msg_controllen = sizeof(control);
    item = CMSG_FIRSTHDR(&message);
    item->cmsg_level = SOL_SOCKET;
    item->cmsg_type = SCM_RIGHTS;
    item->cmsg_len = CMSG_LEN(sizeof(int));
    memcpy(CMSG_DATA(item), &passed_fd, sizeof(passed_fd));
  }
  do {
    count = sendmsg(fd, &message, MSG_NOSIGNAL);
  } while (count < 0 && errno == EINTR);
  return count == (ssize_t)sizeof(*header) ? 0 : -1;
}

/*
 * The kernel installs any SCM_RIGHTS descriptors into this process before
 * recvmsg returns, even when the message is truncated or carries more than
 * expected. Every failure path below therefore closes whatever arrived so a
 * misbehaving peer cannot make the process leak descriptors.
 */
int
cuinterpose_receive_header(int fd, struct cuinterpose_header* header, int* passed_fd)
{
  /* Room for two descriptors so a second one is detected instead of truncated. */
  char control[CMSG_SPACE(sizeof(int) * 2)] = {0};
  struct iovec vector = {.iov_base = header, .iov_len = sizeof(*header)};
  struct msghdr message = {
      .msg_iov = &vector,
      .msg_iovlen = 1,
      .msg_control = control,
      .msg_controllen = sizeof(control),
  };
  struct cmsghdr* item;
  ssize_t count;
  int received[2] = {-1, -1};
  size_t received_count = 0;
  bool ok;

  *passed_fd = -1;
  do {
    count = recvmsg(fd, &message, MSG_WAITALL | MSG_CMSG_CLOEXEC);
  } while (count < 0 && errno == EINTR);
  if (count < 0)
    return -1;
  for (item = CMSG_FIRSTHDR(&message); item != NULL; item = CMSG_NXTHDR(&message, item)) {
    if (item->cmsg_level != SOL_SOCKET || item->cmsg_type != SCM_RIGHTS)
      continue;
    size_t bytes = item->cmsg_len - CMSG_LEN(0);
    size_t index;
    for (index = 0; index < bytes / sizeof(int); index++) {
      int value;
      memcpy(&value, CMSG_DATA(item) + index * sizeof(int), sizeof(value));
      if (received_count < 2)
        received[received_count] = value;
      else
        close(value);
      received_count++;
    }
  }
  ok = count == (ssize_t)sizeof(*header) && (message.msg_flags & (MSG_TRUNC | MSG_CTRUNC)) == 0 &&
       received_count <= 1;
  if (!ok) {
    if (received[0] >= 0)
      close(received[0]);
    if (received[1] >= 0)
      close(received[1]);
    return -1;
  }
  *passed_fd = received[0];
  return 0;
}
