// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <optional>
#include <limits>
#include <stdexcept>
#include <string>
#include <vector>

#include "utils/sha256.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

class RestoreIntegrityError : public std::runtime_error {
 public:
  RestoreIntegrityError() : std::runtime_error("restored file SHA-256 mismatch") {}
};

// Shared by upload preflight and restore staging, before paths reach syscalls.
inline bool
IsSafeRelativePath(const Path& path)
{
  if (path.empty() || path.is_absolute())
    return false;
  if (path.has_root_name() || path.has_root_directory())
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

struct RestoreDirectory {
  // Path within the checkpoint tree; appended to the destination staging root.
  Path relative_path;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
};

struct RestoreFile {
  // Source to read: a full filesystem path or a native Model Streamer object URI.
  std::string source_locator;
  // Path within the checkpoint tree; appended to the destination staging root.
  Path relative_path;
  uintmax_t size_bytes = 0;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
  // Verified after native access finishes, before applying final permissions.
  std::optional<utils::Sha256Digest> expected_sha256;
};

// A transfer engine resolves its storage source into this neutral plan before
// submitting reads to Model Streamer. Destination paths must be unique,
// normalized relative paths without dot components, and directories must be
// ordered parent-first. Source locators are backend-specific and are never
// joined to the destination; future backends may therefore use object URIs.
struct RestorePlan {
  std::filesystem::perms root_permissions = std::filesystem::perms::unknown;
  std::vector<RestoreDirectory> directories;
  std::vector<RestoreFile> files;

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

}  // namespace snapshot::pagebroker
