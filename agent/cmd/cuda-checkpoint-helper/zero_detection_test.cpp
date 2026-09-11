/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "zero_detection.h"

#include <array>
#include <cassert>

int main() {
  using cuda_checkpoint_transfer::AllBytesZero;
  std::array<unsigned char, 4096> bytes{};
  assert(AllBytesZero(bytes.data(), bytes.size()));
  for (size_t index : {size_t{0}, bytes.size() / 2, bytes.size() - 1}) {
    bytes[index] = 1;
    assert(!AllBytesZero(bytes.data(), bytes.size()));
    bytes[index] = 0;
  }
  assert(AllBytesZero(bytes.data(), 0));
}
