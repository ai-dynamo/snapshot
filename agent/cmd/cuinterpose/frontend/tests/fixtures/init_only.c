// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "cuda.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

int main(void) {
    char endpoint[256];
    snprintf(endpoint, sizeof(endpoint), "%s/cuinterpose-%d.sock",
             getenv("SNAPSHOT_CONTROL_DIR"), getpid());
    assert(access(endpoint, F_OK) == -1);
    assert(cuInit(0) == 0);
    assert(access(endpoint, F_OK) == 0);
    return 0;
}
