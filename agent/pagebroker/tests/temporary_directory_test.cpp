// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "temporary_directory.hpp"

#include <gtest/gtest.h>

#include <fstream>

namespace snapshot::pagebroker::test {
namespace fs = std::filesystem;
namespace {
TEST(TemporaryDirectoryTest, PreservesExternalLinkTargets)
{
  TemporaryDirectory outside;
  const auto file = outside.path() / "file";
  const auto directory = outside.path() / "directory";
  std::ofstream(file) << "preserve";
  fs::create_directory(directory);
  fs::permissions(file, fs::perms::owner_read);
  const auto directory_permissions = fs::perms::owner_read | fs::perms::owner_exec;
  fs::permissions(directory, directory_permissions);
  fs::path removed;
  {
    TemporaryDirectory temporary;
    removed = temporary.path();
    fs::create_symlink(file, temporary.path() / "file-link");
    fs::create_directory_symlink(directory, temporary.path() / "directory-link");
    fs::create_hard_link(file, temporary.path() / "hard-link");
  }
  EXPECT_FALSE(fs::exists(removed));
  EXPECT_EQ(fs::status(file).permissions(), fs::perms::owner_read);
  EXPECT_EQ(fs::status(directory).permissions(), directory_permissions);
  EXPECT_EQ(fs::file_size(file), 8);
}

TEST(TemporaryDirectoryTest, RemovesRestrictedDirectories)
{
  fs::path removed;
  {
    TemporaryDirectory temporary;
    removed = temporary.path();
    const auto nested = removed / "parent/child";
    fs::create_directories(nested);
    std::ofstream(nested / "file") << "data";
    fs::permissions(nested / "file", fs::perms::none);
    fs::permissions(nested, fs::perms::none);
    fs::permissions(nested.parent_path(), fs::perms::none);
    fs::permissions(removed, fs::perms::none);
  }
  EXPECT_FALSE(fs::exists(removed));
}

TEST(TemporaryDirectoryTest, DoesNotTraverseReplacedRoot)
{
  TemporaryDirectory outside;
  const auto permissions = fs::perms::owner_read | fs::perms::owner_exec;
  fs::permissions(outside.path(), permissions);
  fs::path removed;
  {
    TemporaryDirectory temporary;
    removed = temporary.path();
    fs::remove(removed);
    fs::create_directory_symlink(outside.path(), removed);
  }
  EXPECT_FALSE(fs::exists(removed));
  EXPECT_EQ(fs::status(outside.path()).permissions(), permissions);
}
}  // namespace
}  // namespace snapshot::pagebroker::test
