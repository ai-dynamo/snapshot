// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "filesystem_storage.hpp"

#include <fcntl.h>

#include <algorithm>
#include <filesystem>
#include <stdexcept>
#include <string>
#include <system_error>
#include <utility>

#include "transfer/engine/transfer_engine.hpp"
#include "gpu/storage_manifest.hpp"

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

void
CheckPublicationConflict(const Path& destination)
{
  // The partial belongs to another publisher, including when it is a dangling link.
  if (fs::exists(fs::symlink_status(PartialPath(destination))))
    throw TransferError(Failure::TRANSACTION_CONFLICT, "checkpoint destination conflicts");
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
ReplaceDirectory(const Path& from, const Path& to)
{
  if (!fs::exists(to)) {
    fs::rename(from, to);
    return;
  }
  const Path previous = PreviousPath(to);
  fs::rename(to, previous);
  RestorePreviousOnFailure restore_previous(previous, to);
  fs::rename(from, to);
  restore_previous.Cancel();
  std::error_code cleanup_error;
  fs::remove_all(previous, cleanup_error);
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
RemoveStaging(const Path& directory)
{
  // Restore directory access before cleanup, including after a restart.
  // Only chmod directories; files may have hard-linked aliases.
  const auto options = fs::perm_options::replace | fs::perm_options::nofollow;
  if (fs::is_directory(fs::symlink_status(directory))) {
    fs::permissions(directory, fs::perms::owner_all, options);
    for (const auto& entry : fs::recursive_directory_iterator(directory)) {
      if (fs::is_directory(entry.symlink_status()))
        fs::permissions(entry.path(), fs::perms::owner_all, options);
    }
  }
  fs::remove_all(directory);
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
  for (const auto& directory : plan.directories) {
    control.Check();
    fs::create_directory(destination / directory.relative_path);
  }
  for (const auto& file : plan.files) {
    control.Check();
    fs::copy_file(file.source_locator, destination / file.relative_path);
    control.Check();
    fs::permissions(destination / file.relative_path, file.permissions);
  }
  // Apply directory modes only after all children have been populated.
  for (auto directory = plan.directories.rbegin(); directory != plan.directories.rend(); ++directory) {
    control.Check();
    fs::permissions(destination / directory->relative_path, directory->permissions);
  }
  control.Check();
  fs::permissions(destination, plan.root_permissions);
  control.Check();
}

namespace {
RestorePlan
BuildPlan(const Path& source, TransferControl control, std::size_t limit, bool cpu_restore)
{
  control.Check();
  ValidateSourceRoot(source);
  RestorePlan plan;
  plan.root_permissions = fs::symlink_status(source).permissions();
  for (auto it = fs::recursive_directory_iterator(source); it != fs::recursive_directory_iterator(); ++it) {
    const auto& entry = *it;
    control.Check();
    if (plan.files.size() + plan.directories.size() >= limit)
      throw std::invalid_argument("checkpoint exceeds entry limit");
    const auto status = entry.symlink_status();
    if (fs::is_symlink(status))
      throw std::runtime_error("checkpoint contains symlink");
    // The GPU engine reads its payload separately from CPU restore staging.
    if (cpu_restore && entry.path() == source / gpu::kDataDirectory) {
      it.disable_recursion_pending();
      continue;
    }

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
}  // namespace

RestorePlan
BuildRestorePlan(const Path& source, TransferControl control, std::size_t limit)
{
  return BuildPlan(source, control, limit, false);
}

RestorePlan
BuildCpuRestorePlan(const Path& source, TransferControl control)
{
  return BuildPlan(source, control, std::numeric_limits<std::size_t>::max(), true);
}

FileDescriptor
OpenDirectory(const Path& directory)
{
  FileDescriptor descriptor(open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  if (descriptor.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open artifact directory");
  return descriptor;
}

PreparedRestore
PrepareRestore(const StorageBackend& source, const Path& storage_root, TransferMode mode, TransferControl control)
{
  control.Check();
  PreparedRestore prepared;
  prepared.mode = mode;
  prepared.local_source = SourcePath(source, storage_root);
  prepared.source = OpenDirectory(prepared.local_source);
  if (mode == TransferMode::Staged)
    prepared.plan = BuildCpuRestorePlan(prepared.local_source, control);
  return prepared;
}

RestoreAccess
CompleteRestore(PreparedRestore preparation, const Path& destination)
{
  if (preparation.mode == TransferMode::Direct)
    return {std::move(preparation.local_source), {}, std::move(preparation.source)};
  if (preparation.source.get() < 0)
    preparation.source = OpenDirectory(destination);
  return {destination, destination, std::move(preparation.source)};
}

Path
PrepareCheckpoint(const StorageBackend& destination, const Path& storage_root,
    const Path& staging_directory, const std::string& transaction_id, TransferMode mode)
{
  const Path published = DestinationPath(destination, storage_root);
  if (mode == TransferMode::Staged)
    return staging_directory;
  if (transaction_id.empty() || transaction_id == "." || transaction_id == ".." ||
      transaction_id.find_first_of("/\\") != std::string::npos || transaction_id.find('\0') != std::string::npos)
    throw std::invalid_argument("invalid transaction path component");
  return Path(published.string() + ".pagebroker-tx-" + transaction_id);
}

void
PublishCheckpoint(const Path& source, const StorageBackend& destination, const Path& storage_root,
    const RestorePlan& plan, TransferMode mode, TransferControl control)
{
  control.Check();
  const Path published = DestinationPath(destination, storage_root);
  CheckPublicationConflict(published);
  if (mode == TransferMode::Direct) {
    // Direct output is beside its destination, so publication preserves file inodes.
    ReplaceDirectory(source, published);
    return;
  }
  const Path partial = PartialPath(published);
  fs::create_directories(published.parent_path());
  if (!fs::create_directory(partial))
    throw TransferError(Failure::TRANSACTION_CONFLICT, "checkpoint destination conflicts");
  try {
    StageRestore(plan, partial, control);
    control.Check();
    ReplaceDirectory(partial, published);
  }
  catch (...) {
    try { RemoveStaging(partial); } catch (const fs::filesystem_error&) {}
    throw;
  }
}
}  // namespace snapshot::pagebroker::filesystem_storage
