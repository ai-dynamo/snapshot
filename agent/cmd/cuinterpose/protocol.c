/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "protocol.h"

#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

/*
 * The wire I/O half of protocol.h: one fixed-size header each way, optionally
 * carrying one descriptor as SCM_RIGHTS ancillary data.
 */

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
