/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef CUINTERPOSER_UTIL_H
#define CUINTERPOSER_UTIL_H

#include <stdbool.h>
#include <stddef.h>

#include "protocol.h"

int write_all(int fd, const void* value, size_t size);
int read_all(int fd, void* value, size_t size);
int pread_all(int fd, void* value, size_t size);
int random_bytes(void* output, size_t size);
int random_id(char output[CUINTERPOSER_ID_SIZE]);
bool is_lower_hex_id(const char value[CUINTERPOSER_ID_SIZE]);
bool header_strings_terminated(const struct cuinterposer_header* header);
void header_error(struct cuinterposer_header* header, const char* message);
int set_socket_timeouts(int fd, int seconds);
int send_header(int fd, const struct cuinterposer_header* header, int passed_fd);
int receive_header(int fd, struct cuinterposer_header* header, int* passed_fd);

#endif
