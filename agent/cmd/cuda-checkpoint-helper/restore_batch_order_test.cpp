/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "restore_batch_order.h"

#include <cassert>

int main() {
  using cuda_checkpoint_operation::RestoreBatchCompletionIndex;

  assert(RestoreBatchCompletionIndex(0, 1) == 0);
  assert(RestoreBatchCompletionIndex(0, 4) == 3);
  assert(RestoreBatchCompletionIndex(1, 4) == 2);
  assert(RestoreBatchCompletionIndex(2, 4) == 1);
  assert(RestoreBatchCompletionIndex(3, 4) == 0);
  return 0;
}
