/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSE_UTIL_H
#define CUINTERPOSE_UTIL_H

#include <stdbool.h>
#include <stddef.h>

#include "protocol.h"

/*
 * Small I/O and identity helpers shared by the shim and the coordinator. All
 * of these are internal: the shim is built with hidden visibility, and the
 * coordinator is a static binary.
 */

/* Loops until every byte is written. For files and memfds; not for sockets. */
int cuinterpose_write_all(int fd, const void* value, size_t size);
/* Like cuinterpose_write_all for a socket: uses send(MSG_NOSIGNAL) so a peer
 * that hung up produces EPIPE instead of killing the process with SIGPIPE. */
int cuinterpose_send_all(int fd, const void* value, size_t size);
int cuinterpose_read_all(int fd, void* value, size_t size);
/* Reads `size` bytes from offset 0 without moving the file position. */
int cuinterpose_pread_all(int fd, void* value, size_t size);
int cuinterpose_random_bytes(void* output, size_t size);
/* Writes 32 lowercase hex characters plus the terminator. */
int cuinterpose_random_id(char output[CUINTERPOSE_ID_SIZE]);
bool cuinterpose_is_lower_hex_id(const char value[CUINTERPOSE_ID_SIZE]);
bool cuinterpose_header_strings_terminated(const struct cuinterpose_header* header);
void cuinterpose_header_error(struct cuinterpose_header* header, const char* message);
int cuinterpose_set_socket_timeouts(int fd, unsigned seconds);
/* Parses a whole-second override; anything malformed yields `fallback`. */
unsigned cuinterpose_bounded_seconds(const char* value, unsigned fallback);
/* CUINTERPOSE_CONTROL_TIMEOUT_ENV or the default, read once. */
unsigned cuinterpose_control_timeout_seconds(void);
/* Sends one header and, when passed_fd >= 0, that descriptor as SCM_RIGHTS. */
int cuinterpose_send_header(int fd, const struct cuinterpose_header* header, int passed_fd);
/* Receives one header. On success *passed_fd is the received descriptor or -1.
 * On failure no descriptor is left open, whatever the peer sent. */
int cuinterpose_receive_header(int fd, struct cuinterpose_header* header, int* passed_fd);

#endif
