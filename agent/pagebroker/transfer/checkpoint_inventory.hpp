// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <limits>
#include <stdexcept>
#include <string>
#include <vector>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

// Checks a single path component, rejecting separators, NUL bytes and dot components.
inline bool
IsSafePathComponent(const std::string& value)
{
  return !value.empty() && value != "." && value != ".." && value.find('/') == std::string::npos &&
         value.find('\\') == std::string::npos && value.find('\0') == std::string::npos;
}

// Checks for a normalized relative path without NUL bytes or dot components.
inline bool
IsSafeRelativePath(const Path& path)
{
  if (path.empty() || path.is_absolute())
    return false;
  if (path.native().find('\0') != std::string::npos)
    return false;
  if (path.lexically_normal().native() != path.native())
    return false;
  for (const auto& component : path) {
    if (component.empty())
      return false;
    if (component == "." || component == "..")
      return false;
  }
  return true;
}

struct CheckpointDirectory {
  // Relative to the checkpoint tree root.
  Path relative_path;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
};

struct CheckpointFile {
  // Backend-specific source path or URI; never joined to the destination.
  std::string source_locator;
  // Relative to the checkpoint tree root.
  Path relative_path;
  uintmax_t size_bytes = 0;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
};

// Checkpoint inventory with unique, normalized relative paths and no dot components.
// Directories must be ordered parent-first.
struct CheckpointInventory {
  std::filesystem::perms root_permissions = std::filesystem::perms::unknown;
  std::vector<CheckpointDirectory> directories;
  std::vector<CheckpointFile> files;

  // Totals the planned file sizes and rejects integer overflow.
  uintmax_t size_bytes() const
  {
    uintmax_t bytes = 0;
    for (const auto& file : files) {
      if (file.size_bytes > std::numeric_limits<uintmax_t>::max() - bytes)
        throw std::invalid_argument("checkpoint size exceeds platform limit");
      bytes += file.size_bytes;
    }
    return bytes;
  }
};

enum class TransferMode { Staged, Direct };

// Preparation retains borrowed local access separately from the files to transfer.
struct PreparedRestore {
  CheckpointInventory inventory;
  TransferMode mode = TransferMode::Staged;
  Path local_source;
  FileDescriptor source{-1};
};

// Only staging_directory is owned by the transaction and eligible for cleanup.
struct RestoreAccess {
  Path image_directory;
  Path staging_directory;
  FileDescriptor source{-1};
};

}  // namespace snapshot::pagebroker
