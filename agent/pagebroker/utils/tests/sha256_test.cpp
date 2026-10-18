// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "utils/sha256.hpp"

#include <gtest/gtest.h>

#include <string>

namespace snapshot::pagebroker::utils {
namespace {
std::string
Hex(const Sha256Digest& digest)
{
  constexpr char digits[] = "0123456789abcdef";
  std::string result;
  for (const auto byte : digest) {
    result += digits[byte >> 4];
    result += digits[byte & 15];
  }
  return result;
}

TEST(Sha256Test, MatchesPublishedIdentityHashVectors)
{
  EXPECT_EQ(Hex(ComputeSha256({})), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  const std::string abc = "abc";
  EXPECT_EQ(Hex(ComputeSha256(std::as_bytes(std::span(abc)))),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
}
}  // namespace
}  // namespace snapshot::pagebroker::utils
