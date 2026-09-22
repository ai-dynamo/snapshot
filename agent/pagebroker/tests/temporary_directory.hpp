// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdlib>

#include <filesystem>
#include <stdexcept>
#include <string>

namespace snapshot::pagebroker::test {
class TemporaryDirectory {
 public:
  TemporaryDirectory()
  {
    std::string pattern = (std::filesystem::temp_directory_path() / "pagebroker-test-XXXXXX").string();
    if (mkdtemp(pattern.data()) == nullptr)
      throw std::runtime_error("create test directory");
    path_ = pattern;
  }
  ~TemporaryDirectory() noexcept
  {
    // Only directories need write/traversal permissions for cleanup. Never
    // chmod through a symlink or change a regular file's hard-linked aliases.
    std::error_code error;
    const auto options = std::filesystem::perm_options::replace | std::filesystem::perm_options::nofollow;
    if (std::filesystem::is_directory(std::filesystem::symlink_status(path_, error))) {
      std::filesystem::permissions(path_, std::filesystem::perms::owner_all, options, error);
      auto entry = std::filesystem::recursive_directory_iterator(path_, error);
      while (entry != std::filesystem::recursive_directory_iterator()) {
        if (std::filesystem::is_directory(entry->symlink_status(error)))
          std::filesystem::permissions(entry->path(), std::filesystem::perms::owner_all, options, error);
        entry.increment(error);
      }
    }
    std::filesystem::remove_all(path_, error);
  }
  TemporaryDirectory(const TemporaryDirectory&) = delete;
  TemporaryDirectory& operator=(const TemporaryDirectory&) = delete;
  const std::filesystem::path& path() const { return path_; }

 private:
  std::filesystem::path path_;
};
}  // namespace snapshot::pagebroker::test
