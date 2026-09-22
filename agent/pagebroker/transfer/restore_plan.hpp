// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <optional>
#include <string>
#include <vector>

#include "utils/sha256.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

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
};

}  // namespace snapshot::pagebroker
