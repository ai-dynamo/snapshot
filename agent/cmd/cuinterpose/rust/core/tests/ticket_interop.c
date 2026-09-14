// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Compiled with unmodified posix.c/protocol.c from the pinned C stack, not a
// reimplementation of the reference ticket checks. fd 0 is the Rust ticket;
// fd 1 is a Unix socket used to return a newly created C ticket.
#include "posix.h"
#include <string.h>
#include <unistd.h>

int main(void)
{
    struct cuinterpose_posix_ticket ticket;
    if (cuinterpose_posix_read_ticket(STDIN_FILENO, &ticket) != 0 ||
        ticket.resource_kind != CUINTERPOSE_RESOURCE_UNICAST)
        return 1;
    int fd = -1;
    if (cuinterpose_posix_create_ticket(&ticket, &fd) != 0)
        return 2;
    struct cuinterpose_header header = {
        .magic = CUINTERPOSE_MAGIC,
        .version = CUINTERPOSE_VERSION,
        .operation = CUINTERPOSE_EXPORT,
        .resource_kind = ticket.resource_kind,
    };
    memcpy(header.participant_id, ticket.creator_participant, sizeof(header.participant_id));
    memcpy(header.allocation_id, ticket.allocation_id, sizeof(header.allocation_id));
    int result = cuinterpose_send_header(STDOUT_FILENO, &header, fd);
    close(fd);
    return result == 0 ? 0 : 3;
}
