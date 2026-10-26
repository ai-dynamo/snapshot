// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint_index.hpp"

#include <algorithm>
#include <set>

#include "utils/json.hpp"

namespace snapshot::pagebroker {
namespace {
using Json = nlohmann::json;
namespace fs = std::filesystem;

constexpr char kFormatKey[] = "format";
constexpr char kRootModeKey[] = "rootMode";
constexpr char kDirectoriesKey[] = "directories";
constexpr char kFilesKey[] = "files";
constexpr char kPathKey[] = "path";
constexpr char kModeKey[] = "mode";
constexpr char kSizeKey[] = "size";

void
Fields(const Json& value, std::initializer_list<const char*> fields)
{
  if (!value.is_object() || value.size() != fields.size())
    throw std::invalid_argument("invalid checkpoint index fields");
  for (const auto* field : fields) {
    if (!value.contains(field))
      throw std::invalid_argument("missing checkpoint index field");
  }
}

bool
HasValidInventoryLimits(const CheckpointInventory& inventory)
{
  return inventory.files.size() + inventory.directories.size() <= kMaxCheckpointEntries &&
         static_cast<unsigned>(inventory.root_permissions) <= 0777;
}

// Registers a path only after its syntax and permissions pass. Keep parent and
// UTF-8 checks after registration, matching validation before serialization.
bool
RegisterCheckpointEntry(const Path& path, fs::perms mode,
    std::set<Path>& paths, const std::set<Path>& directories)
{
  const auto text = path.generic_string();
  if (!IsSafeRelativePath(path) || text.size() > 512)
    return false;
  if (text.find('\\') != std::string::npos)
    return false;
  if (std::any_of(text.begin(), text.end(), [](unsigned char c) { return c < 32 || c == 127; }))
    return false;
  if (static_cast<unsigned>(mode) > 0777 || !paths.insert(path).second)
    return false;
  if (!path.parent_path().empty() && !directories.contains(path.parent_path()))
    return false;
  try {
    (void)Json(text).dump(); // Reject invalid UTF-8 before paths reach storage.
  }
  catch (const Json::type_error&) {
    return false;
  }
  return true;
}

bool
CheckpointEntriesAreOrdered(const CheckpointInventory& inventory)
{
  auto less = [](const auto& a, const auto& b) { return a.relative_path < b.relative_path; };
  return std::is_sorted(inventory.directories.begin(), inventory.directories.end(), less) &&
         std::is_sorted(inventory.files.begin(), inventory.files.end(), less);
}

bool
HasValidIndexEntries(const Json& value)
{
  if (!value.at(kDirectoriesKey).is_array() || !value.at(kFilesKey).is_array())
    return false;
  return value.at(kDirectoriesKey).size() + value.at(kFilesKey).size() <= kMaxCheckpointEntries;
}

}  // namespace

std::uint64_t
ValidateCheckpointInventory(const CheckpointInventory& inventory)
{
  if (!HasValidInventoryLimits(inventory))
    throw std::invalid_argument("checkpoint exceeds index limits");
  std::set<Path> directories, paths;
  auto add = [&](const Path& path, fs::perms mode) {
    if (!RegisterCheckpointEntry(path, mode, paths, directories))
      throw std::invalid_argument("invalid checkpoint path or permissions");
  };
  for (const auto& directory : inventory.directories) {
    add(directory.relative_path, directory.permissions);
    directories.insert(directory.relative_path);
  }
  if (!CheckpointEntriesAreOrdered(inventory))
    throw std::invalid_argument("checkpoint entries are not ordered");
  bool manifest = false;
  std::uint64_t bytes = 0;
  for (const auto& file : inventory.files) {
    add(file.relative_path, file.permissions);
    if (file.size_bytes > kMaxCheckpointBytes - bytes)
      throw std::invalid_argument("checkpoint has invalid size");
    bytes += file.size_bytes;
    if (file.relative_path == kCheckpointManifestName) {
      if (file.size_bytes == 0 || file.size_bytes > kMaxManifestBytes)
        throw std::invalid_argument("invalid checkpoint manifest size");
      manifest = true;
    }
  }
  if (!manifest)
    throw std::invalid_argument("checkpoint requires manifest.yaml");
  return bytes;
}

std::string
SerializeCheckpointIndex(const CheckpointInventory& inventory)
{
  (void)ValidateCheckpointInventory(inventory);
  Json value{{kFormatKey, kCheckpointFormat}, {kRootModeKey, static_cast<unsigned>(inventory.root_permissions)},
             {kDirectoriesKey, Json::array()}, {kFilesKey, Json::array()}};
  for (const auto& directory : inventory.directories)
    value[kDirectoriesKey].push_back({{kPathKey, directory.relative_path.generic_string()},
        {kModeKey, static_cast<unsigned>(directory.permissions)}});
  for (const auto& file : inventory.files)
    value[kFilesKey].push_back({{kPathKey, file.relative_path.generic_string()},
        {kModeKey, static_cast<unsigned>(file.permissions)}, {kSizeKey, file.size_bytes}});
  const auto text = value.dump();
  if (text.size() > kMaxIndexBytes)
    throw std::invalid_argument("checkpoint index exceeds size limit");
  return text;
}

CheckpointInventory
ParseCheckpointIndex(std::string_view text)
{
  try {
    const auto value = utils::ParseJson(text, kMaxIndexBytes);
    Fields(value, {kFormatKey, kRootModeKey, kDirectoriesKey, kFilesKey});
    if (!value.at(kFormatKey).is_string())
      throw std::invalid_argument("checkpoint index format must be a string");
    if (value.at(kFormatKey) != kCheckpointFormat)
      throw CheckpointError(Failure::UNSUPPORTED_ARTIFACT, "unsupported checkpoint index format");
    CheckpointInventory inventory;
    inventory.root_permissions = static_cast<fs::perms>(utils::Unsigned(value.at(kRootModeKey), 0777));
    if (!HasValidIndexEntries(value))
      throw std::invalid_argument("invalid checkpoint entry count");
    for (const auto& directory : value.at(kDirectoriesKey)) {
      Fields(directory, {kPathKey, kModeKey});
      inventory.directories.push_back({directory.at(kPathKey).get<std::string>(),
          static_cast<fs::perms>(utils::Unsigned(directory.at(kModeKey), 0777))});
    }
    for (const auto& file : value.at(kFilesKey)) {
      Fields(file, {kPathKey, kModeKey, kSizeKey});
      inventory.files.push_back({"", file.at(kPathKey).get<std::string>(), utils::Unsigned(file.at(kSizeKey), kMaxCheckpointBytes),
          static_cast<fs::perms>(utils::Unsigned(file.at(kModeKey), 0777))});
    }
    (void)ValidateCheckpointInventory(inventory);
    return inventory;
  }
  catch (const CheckpointError&) {
    throw;
  }
  catch (...) {
    throw CheckpointError(Failure::ARTIFACT_CORRUPT, "invalid checkpoint index");
  }
}
}  // namespace snapshot::pagebroker
