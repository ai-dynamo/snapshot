#pragma once

#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

struct RestoreDirectory {
  Path relative;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
};

struct RestoreFile {
  std::string source_locator;
  Path relative;
  uintmax_t bytes = 0;
  std::filesystem::perms permissions = std::filesystem::perms::unknown;
};

// A transfer engine resolves its storage source into this neutral plan before
// submitting reads to Model Streamer. Future backends may use object URIs as
// source locators without changing the staging machinery.
struct RestorePlan {
  std::filesystem::perms root_permissions = std::filesystem::perms::unknown;
  std::vector<RestoreDirectory> directories;
  std::vector<RestoreFile> files;
};

}  // namespace snapshot::pagebroker
