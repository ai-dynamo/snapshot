// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Only the ABI prefix is accessible. Reading any function-table field faults,
// so these fixtures prove rejection precedes access to the complete table.
#include <stdint.h>
#include <sys/mman.h>
#include <unistd.h>

static void *mapping;
static size_t length;

int cuinterpose_core_init(const void *host, const void **output)
{
    if (!host || !output)
        return 1;
    long page_size = sysconf(_SC_PAGESIZE);
    if (page_size <= 0)
        return 999;
    length = (size_t)page_size * 2;
    mapping = mmap(NULL, length, PROT_READ | PROT_WRITE,
                   MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (mapping == MAP_FAILED)
        return 999;
    if (mprotect((char *)mapping + page_size, (size_t)page_size, PROT_NONE) != 0)
        return 999;
    uint32_t *prefix = (uint32_t *)((char *)mapping + page_size - 2 * sizeof(uint32_t));
#ifdef WRONG_VERSION
    prefix[0] = 999;
    // Prefix, debug, three fork hooks, readiness, and 17 CUDA callbacks.
    prefix[1] = 8 + 22 * sizeof(void (*)(void));
#else
    prefix[0] = 5;
    prefix[1] = 8;
#endif
    *output = prefix;
    return 0;
}

__attribute__((destructor)) static void release_mapping(void)
{
    if (mapping && mapping != MAP_FAILED)
        munmap(mapping, length);
}
