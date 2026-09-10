/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "zero_detection.h"

#include <array>
#include <cassert>

int main() {
  using cuda_checkpoint_transfer::AllBytesZero;

  std::array<unsigned char, 8192> data{};
  assert(AllBytesZero(data.data(), data.size()));
  assert(AllBytesZero(data.data(), 0));

  for (const size_t index : {size_t{0}, data.size() / 2, data.size() - 1}) {
    data[index] = 1;
    assert(!AllBytesZero(data.data(), data.size()));
    data[index] = 0;
  }
  return 0;
}
