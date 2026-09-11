/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Placeholder shim. It builds and is packaged as libcuinterpose.so so that the
 * delivery path (init container, LD_PRELOAD, restore mount) is final before
 * the interposer exists, and it intercepts nothing. The following changes
 * replace it: symbol resolution and forwarding, allocation tracking, the
 * checkpoint lifecycle, multicast objects.
 */

#include <cuda.h>
#include <string.h>

#include "export.h"
#include "protocol.h"

CUINTERPOSE_API const struct cuinterpose_build_info cuinterpose_build_info = {CUDA_VERSION, CUINTERPOSE_VERSION};

CUINTERPOSE_API void
cuinterpose_debug_stats(struct cuinterpose_debug_stats* stats)
{
  memset(stats, 0, sizeof(*stats));
}
