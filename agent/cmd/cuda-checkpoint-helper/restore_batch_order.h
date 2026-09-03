/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>

namespace cuda_checkpoint_operation {

// Batch restore requests preserve the captured process-tree order: parents
// precede children. CUDA operation completion resumes a target, so targets must
// be completed in the opposite order to keep a resumed parent from terminating
// a child whose CUDA operation is still pending.
constexpr std::size_t RestoreBatchCompletionIndex(std::size_t completed,
                                                  std::size_t target_count) {
  return target_count - completed - 1;
}

} // namespace cuda_checkpoint_operation
