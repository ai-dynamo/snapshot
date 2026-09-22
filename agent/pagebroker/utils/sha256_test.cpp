// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "sha256.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>

#include <fstream>
#include <string>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker::utils {
namespace {
std::string
Hex(const Sha256Digest& digest)
{
  const char* digits = "0123456789abcdef";
  std::string result;
  for (const auto byte : digest) {
    result += digits[byte >> 4];
    result += digits[byte & 15];
  }
  return result;
}

TEST(Sha256Test, MatchesPublishedVectors)
{
  EXPECT_EQ(Hex(ComputeSha256({})), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  const std::string abc = "abc";
  EXPECT_EQ(Hex(ComputeSha256(std::as_bytes(std::span(abc)))),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  test::TemporaryDirectory root;
  const auto path = root.path() / "million-a";
  std::ofstream(path, std::ios::binary) << std::string(1'000'000, 'a');
  EXPECT_EQ(Hex(ComputeFileSha256(path)), "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0");
}

TEST(Sha256Test, HandlesEmptyAndBinaryFiles)
{
  test::TemporaryDirectory root;
  const auto path = root.path() / "data";
  std::ofstream(path).close();
  EXPECT_EQ(ComputeFileSha256(path), ComputeSha256({}));
  std::string data(300'123, '\0');
  for (std::size_t i = 0; i < data.size(); ++i)
    data[i] = static_cast<char>(i % 256);
  std::ofstream(path, std::ios::binary).write(data.data(), data.size());
  EXPECT_EQ(ComputeFileSha256(path), ComputeSha256(std::as_bytes(std::span(data))));
}

TEST(Sha256Test, RejectsMissingAndUnsupportedSources)
{
  test::TemporaryDirectory root;
  EXPECT_THROW(ComputeFileSha256(root.path() / "missing"), std::system_error);
  EXPECT_THROW(ComputeFileSha256(root.path()), std::invalid_argument);
  const auto fifo = root.path() / "fifo";
  ASSERT_EQ(mkfifo(fifo.c_str(), 0600), 0);
  EXPECT_THROW(ComputeFileSha256(fifo), std::invalid_argument);
  std::filesystem::create_symlink("missing", root.path() / "link");
  EXPECT_THROW(ComputeFileSha256(root.path() / "link"), std::system_error);
}
}  // namespace
}  // namespace snapshot::pagebroker::utils
