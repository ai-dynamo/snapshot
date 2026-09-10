/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <algorithm>
#include <cstddef>

namespace cuda_checkpoint_transfer {

inline bool AllBytesZero(const void *data, size_t size) {
  const auto *bytes = static_cast<const unsigned char *>(data);
  return std::all_of(bytes, bytes + size,
                     [](unsigned char value) { return value == 0; });
}

} // namespace cuda_checkpoint_transfer
