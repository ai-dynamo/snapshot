// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "filesystem_storage.hpp"

#include <algorithm>
#include <filesystem>
#include <stdexcept>
#include <string>
#include <system_error>

namespace snapshot::pagebroker::filesystem_storage {
namespace fs = std::filesystem;
namespace {
bool
IsContainedRelativePath(const Path& relative)
{
  if (relative.empty() || relative == ".")
    return false;
  return relative != ".." && !relative.string().starts_with("../");
}

bool
IsPathWithinStorageRoot(const Path& path, const Path& relative)
{
  if (!path.is_absolute() || path.lexically_normal() != path)
    return false;
  return IsContainedRelativePath(relative);
}

void
ValidateSourceRoot(const Path& source)
{
  if (source.empty() || source.native().find('\0') != std::string::npos)
    throw std::invalid_argument("checkpoint source must be a directory path");
  // Check before any canonicalization, including a root with a trailing slash.
  Path prefix;
  for (const auto& component : fs::absolute(source)) {
    prefix /= component;
    if (fs::is_symlink(fs::symlink_status(prefix)))
      throw std::invalid_argument("checkpoint source contains a symlink");
  }
  if (!fs::is_directory(fs::symlink_status(source)))
    throw std::invalid_argument("checkpoint source must be a directory");
}

Path
StoragePath(const StorageBackend& storage, const Path& storage_root, const char* label)
{
  if (!storage.has_filesystem() || storage.filesystem().directory().empty())
    throw std::invalid_argument(std::string("filesystem ") + label + " is required");
  const Path path(storage.filesystem().directory());
  const Path relative = path.lexically_relative(storage_root);
  if (!IsPathWithinStorageRoot(path, relative))
    throw std::invalid_argument(std::string(label) + " must be within storage root");

  Path component = storage_root;
  for (const auto& part : relative) {
    component /= part;
    if (fs::is_symlink(component))
      throw std::invalid_argument(std::string(label) + " contains symlink");
  }
  return path;
}

Path
PartialPath(const Path& destination)
{
  Path partial = destination;
  partial += ".pagebroker-partial";
  return partial;
}

Path
PreviousPath(const Path& destination)
{
  Path previous = destination;
  previous += ".pagebroker-previous";
  return previous;
}

class RestorePreviousOnFailure {
 public:
  RestorePreviousOnFailure(const Path& previous, const Path& published) : previous_(previous), published_(published) {}

  ~RestorePreviousOnFailure()
  {
    if (!cancelled_) {
      std::error_code error;
      std::filesystem::rename(previous_, published_, error);
    }
  }

  void Cancel() { cancelled_ = true; }

 private:
  const Path& previous_;
  const Path& published_;
  bool cancelled_ = false;
};

void
CopyDirectory(const Path& source, const Path& destination)
{
  fs::copy(source, destination, fs::copy_options::recursive);
}
}  // namespace

Path
SourcePath(const StorageBackend& source, const Path& storage_root)
{
  const Path path = StoragePath(source, storage_root, "source");
  if (!fs::is_directory(path))
    throw std::invalid_argument("source must be a storage directory");
  return path;
}

Path
DestinationPath(const StorageBackend& destination, const Path& storage_root)
{
  return StoragePath(destination, storage_root, "destination");
}

void
StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control)
{
  control.Check();
  for (const auto& directory : plan.directories) {
    if (!IsSafeRelativePath(directory.relative_path))
      throw std::invalid_argument("invalid restore directory");
  }
  for (const auto& file : plan.files) {
    if (!IsSafeRelativePath(file.relative_path))
      throw std::invalid_argument("invalid restore file");
  }
  fs::create_directory(destination);
  for (const auto& directory : plan.directories)
    fs::create_directory(destination / directory.relative_path);
  for (const auto& file : plan.files) {
    control.Check();
    fs::copy_file(file.source_locator, destination / file.relative_path);
    fs::permissions(destination / file.relative_path, file.permissions);
  }
  // Apply directory modes only after all children have been populated.
  for (auto directory = plan.directories.rbegin(); directory != plan.directories.rend(); ++directory)
    fs::permissions(destination / directory->relative_path, directory->permissions);
  fs::permissions(destination, plan.root_permissions);
}

RestorePlan
BuildRestorePlan(const Path& source, TransferControl control, std::size_t limit)
{
  control.Check();
  ValidateSourceRoot(source);
  RestorePlan plan;
  plan.root_permissions = fs::symlink_status(source).permissions();
  for (const auto& entry : fs::recursive_directory_iterator(source)) {
    control.Check();
    if (plan.files.size() + plan.directories.size() >= limit)
      throw std::invalid_argument("checkpoint exceeds entry limit");
    const auto status = entry.symlink_status();
    if (fs::is_symlink(status))
      throw std::runtime_error("checkpoint contains symlink");

    const Path relative = entry.path().lexically_relative(source);
    if (!IsContainedRelativePath(relative))
      throw std::runtime_error("checkpoint contains invalid path");
    if (fs::is_directory(status)) {
      plan.directories.push_back(RestoreDirectory{relative, status.permissions()});
      continue;
    }
    if (!fs::is_regular_file(status))
      throw std::runtime_error("checkpoint contains unsupported file type");

    plan.files.push_back(
        RestoreFile{entry.path().string(), relative, entry.file_size(), status.permissions()});
  }
  // Lexicographic path ordering is deterministic and puts parents first.
  std::sort(plan.directories.begin(), plan.directories.end(), [](const auto& a, const auto& b) {
    return a.relative_path < b.relative_path;
  });
  std::sort(plan.files.begin(), plan.files.end(), [](const auto& a, const auto& b) {
    return a.relative_path < b.relative_path;
  });
  return plan;
}

bool
CheckpointDestinationConflicts(const StorageBackend& destination, const Path& storage_root)
{
  return fs::exists(PartialPath(DestinationPath(destination, storage_root)));
}

void
PublishCheckpoint(const Path& source, const StorageBackend& destination, const Path& storage_root)
{
  const Path published = DestinationPath(destination, storage_root);
  const Path partial = PartialPath(published);
  const Path previous = PreviousPath(published);
  try {
    fs::create_directories(published.parent_path());
    CopyDirectory(source, partial);
    if (fs::exists(published)) {
      fs::rename(published, previous);
      RestorePreviousOnFailure restore_previous(previous, published);
      fs::rename(partial, published);
      restore_previous.Cancel();
      std::error_code cleanup_error;
      fs::remove_all(previous, cleanup_error);
      return;
    }
    fs::rename(partial, published);
  }
  catch (...) {
    std::error_code cleanup_error;
    fs::remove_all(partial, cleanup_error);
    throw;
  }
}
}  // namespace snapshot::pagebroker::filesystem_storage
