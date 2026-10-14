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
HasValidPlanLimits(const RestorePlan& plan)
{
  return plan.files.size() + plan.directories.size() <= kMaxCheckpointEntries &&
         static_cast<unsigned>(plan.root_permissions) <= 0777;
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
  (void)Json(text).dump(); // Reject invalid UTF-8 before paths reach storage.
  return true;
}

bool
CheckpointEntriesAreOrdered(const RestorePlan& plan)
{
  auto less = [](const auto& a, const auto& b) { return a.relative_path < b.relative_path; };
  return std::is_sorted(plan.directories.begin(), plan.directories.end(), less) &&
         std::is_sorted(plan.files.begin(), plan.files.end(), less);
}

bool
HasValidFileMetadata(const RestoreFile& file, bool require_digests, std::uint64_t bytes)
{
  if (require_digests && !file.expected_sha256)
    return false;
  return file.size_bytes <= kMaxCheckpointBytes - bytes;
}

bool
HasValidIndexEntries(const Json& value)
{
  if (!value.at("directories").is_array() || !value.at("files").is_array())
    return false;
  return value.at("directories").size() + value.at("files").size() <= kMaxCheckpointEntries;
}

std::string
Digest(const utils::Sha256Digest& digest)
{
  constexpr char hex[] = "0123456789abcdef";
  std::string result;
  for (auto byte : digest) {
    const auto value = static_cast<unsigned>(byte);
    result += hex[value >> 4];
    result += hex[value & 15];
  }
  return result;
}

utils::Sha256Digest
ParseDigest(const std::string& text)
{
  if (text.size() != 64 || text.find_first_not_of("0123456789abcdef") != std::string::npos)
    throw std::invalid_argument("invalid checkpoint digest");
  utils::Sha256Digest result;
  auto nibble = [](char c) { return c <= '9' ? c - '0' : c - 'a' + 10; };
  for (std::size_t i = 0; i < result.size(); ++i)
    result[i] = static_cast<std::uint8_t>((nibble(text[2 * i]) << 4) | nibble(text[2 * i + 1]));
  return result;
}
}  // namespace

void
ValidateCheckpointID(std::string_view id)
{
  if (id.size() != 64 || id.find_first_not_of("0123456789abcdef") != std::string_view::npos)
    throw std::invalid_argument("checkpoint ID must be 64 lowercase hexadecimal characters");
}

std::uint64_t
ValidateCheckpointPlan(const RestorePlan& plan, bool require_digests)
{
  if (!HasValidPlanLimits(plan))
    throw std::invalid_argument("checkpoint exceeds index limits");
  std::set<Path> directories, paths;
  auto add = [&](const Path& path, fs::perms mode) {
    if (!RegisterCheckpointEntry(path, mode, paths, directories))
      throw std::invalid_argument("invalid checkpoint path or permissions");
  };
  for (const auto& directory : plan.directories) {
    add(directory.relative_path, directory.permissions);
    directories.insert(directory.relative_path);
  }
  if (!CheckpointEntriesAreOrdered(plan))
    throw std::invalid_argument("checkpoint entries are not ordered");
  bool manifest = false;
  std::uint64_t bytes = 0;
  for (const auto& file : plan.files) {
    add(file.relative_path, file.permissions);
    if (!HasValidFileMetadata(file, require_digests, bytes))
      throw std::invalid_argument("checkpoint has invalid size or missing digest");
    bytes += file.size_bytes;
    if (file.relative_path == "manifest.yaml") {
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
SerializeCheckpointIndex(const RestorePlan& plan)
{
  (void)ValidateCheckpointPlan(plan);
  Json value{{"format", kCheckpointFormat}, {"rootMode", static_cast<unsigned>(plan.root_permissions)},
             {"directories", Json::array()}, {"files", Json::array()}};
  for (const auto& directory : plan.directories)
    value["directories"].push_back({{"path", directory.relative_path.generic_string()}, {"mode", static_cast<unsigned>(directory.permissions)}});
  for (const auto& file : plan.files)
    value["files"].push_back({{"path", file.relative_path.generic_string()}, {"mode", static_cast<unsigned>(file.permissions)},
                              {"size", file.size_bytes}, {"sha256", Digest(*file.expected_sha256)}});
  const auto text = value.dump();
  if (text.size() > kMaxIndexBytes)
    throw std::invalid_argument("checkpoint index exceeds size limit");
  return text;
}

RestorePlan
ParseCheckpointIndex(std::string_view text)
{
  try {
    const auto value = utils::ParseJson(text, kMaxIndexBytes);
    Fields(value, {"format", "rootMode", "directories", "files"});
    if (value.at("format") != kCheckpointFormat)
      throw CheckpointError(Failure::UNSUPPORTED_ARTIFACT, "unsupported checkpoint index format");
    RestorePlan plan;
    plan.root_permissions = static_cast<fs::perms>(utils::Unsigned(value.at("rootMode"), 0777));
    if (!HasValidIndexEntries(value))
      throw std::invalid_argument("invalid checkpoint entry count");
    for (const auto& directory : value.at("directories")) {
      Fields(directory, {"path", "mode"});
      plan.directories.push_back({directory.at("path").get<std::string>(), static_cast<fs::perms>(utils::Unsigned(directory.at("mode"), 0777))});
    }
    for (const auto& file : value.at("files")) {
      Fields(file, {"path", "mode", "size", "sha256"});
      plan.files.push_back({"", file.at("path").get<std::string>(), utils::Unsigned(file.at("size"), kMaxCheckpointBytes),
          static_cast<fs::perms>(utils::Unsigned(file.at("mode"), 0777)), ParseDigest(file.at("sha256").get<std::string>())});
    }
    (void)ValidateCheckpointPlan(plan);
    return plan;
  }
  catch (const CheckpointError&) {
    throw;
  }
  catch (...) {
    throw CheckpointError(Failure::ARTIFACT_CORRUPT, "invalid checkpoint index");
  }
}
}  // namespace snapshot::pagebroker
