/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_IO_H
#define CUINTERPOSE_UTIL_IO_H

#include <stddef.h>

/*
 * Full-buffer descriptor I/O shared by the shim and the coordinator. All of
 * these are internal: the shim is built with hidden visibility, and the
 * coordinator is a static binary.
 */

/* Loops until every byte is written. For files and memfds; not for sockets. */
int write_all(int fd, const void* value, size_t size);
/* Like write_all for a socket: uses send(MSG_NOSIGNAL) so a peer that hung up
 * produces EPIPE instead of killing the process with SIGPIPE. */
int send_all(int fd, const void* value, size_t size);
int read_all(int fd, void* value, size_t size);
/* Reads `size` bytes from offset 0 without moving the file position. */
int pread_all(int fd, void* value, size_t size);
/* Sets SO_SNDTIMEO and SO_RCVTIMEO together. */
int set_socket_timeouts(int fd, unsigned seconds);

#endif
