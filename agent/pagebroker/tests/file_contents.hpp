// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <gtest/gtest.h>

#include <algorithm>
#include <array>
#include <filesystem>
#include <fstream>

namespace snapshot::pagebroker::test {
// Compare binary fixtures with bounded memory, independently of transfer metadata.
inline ::testing::AssertionResult
FilesEqual(const std::filesystem::path& expected, const std::filesystem::path& actual)
{
  std::ifstream source(expected, std::ios::binary), restored(actual, std::ios::binary);
  if (!source || !restored)
    return ::testing::AssertionFailure() << "cannot open " << expected << " or " << actual;
  std::array<char, 64 * 1024> left, right;
  while (source && restored) {
    source.read(left.data(), left.size());
    restored.read(right.data(), right.size());
    if (source.gcount() != restored.gcount() ||
        !std::equal(left.begin(), left.begin() + source.gcount(), right.begin()))
      return ::testing::AssertionFailure() << "contents differ: " << expected << " and " << actual;
  }
  if (source.bad() || restored.bad())
    return ::testing::AssertionFailure() << "read failed: " << expected << " or " << actual;
  return ::testing::AssertionSuccess();
}
}  // namespace snapshot::pagebroker::test
