/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_cancellation.h"

#include <cassert>
#include <chrono>

int main() {
  using namespace std::chrono_literals;
  using cuda_checkpoint_transfer::TransferCancellation;

  TransferCancellation unlimited;
  assert(!unlimited.DeadlineExceeded());
  assert(!unlimited.IsCancelled());

  TransferCancellation active(TransferCancellation::Clock::now() + 1h);
  assert(!active.DeadlineExceeded());
  assert(!active.IsCancelled());

  TransferCancellation expired(TransferCancellation::Clock::now() - 1ms);
  assert(expired.DeadlineExceeded());
  assert(expired.IsCancelled());

  active.Cancel();
  assert(active.IsCancelled());
  return 0;
}
