// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include <filesystem>
#include <stdexcept>

namespace snapshot::pagebroker {
namespace {
Path
StoragePath(const StorageBackend& storage, const Path& storage_root, const char* label)
{
  if (!storage.has_filesystem() || storage.filesystem().directory().empty())
    throw std::invalid_argument(std::string("filesystem ") + label + " is required");
  const Path path(storage.filesystem().directory());
  const Path relative = path.lexically_relative(storage_root);
  if (!path.is_absolute() || path.lexically_normal() != path || relative.empty() ||
      relative == "." || relative.string().starts_with("../") || relative == "..")
    throw std::invalid_argument(std::string(label) + " must be within storage root");

  Path component = storage_root;
  for (const auto& part : relative) {
    component /= part;
    if (std::filesystem::is_symlink(component))
      throw std::invalid_argument(std::string(label) + " contains symlink");
  }
  return path;
}

Path
SourcePath(const StorageBackend& source, const Path& storage_root)
{
  const Path path = StoragePath(source, storage_root, "source");
  if (!std::filesystem::is_directory(path))
    throw std::invalid_argument("source must be a storage directory");
  return path;
}

Path
DestinationPath(const StorageBackend& destination, const Path& storage_root)
{
  return StoragePath(destination, storage_root, "destination");
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

uintmax_t
DirectorySize(const Path& path)
{
  uintmax_t bytes = 0;
  for (auto it = std::filesystem::recursive_directory_iterator(path); it != std::filesystem::recursive_directory_iterator(); ++it) {
    const auto& entry = *it;
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    if (entry.path() == path / "native") {
      it.disable_recursion_pending();
      continue;
    }
    if (entry.is_regular_file())
      bytes += entry.file_size();
  }
  return bytes;
}
}  // namespace

PosixCopyEngine::PosixCopyEngine(Path storage_root) : storage_root_(std::filesystem::weakly_canonical(std::move(storage_root))) {}

TransferEngineType
PosixCopyEngine::type() const
{
  return TransferEngineType::POSIX_COPY;
}

Path
PosixCopyEngine::SourceDirectory(const StorageBackend& source) const
{
  return SourcePath(source, storage_root_);
}

uintmax_t
PosixCopyEngine::RestoreSize(const StorageBackend& source) const
{
  return DirectorySize(SourcePath(source, storage_root_));
}

void
PosixCopyEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  const Path root = SourcePath(source, storage_root_);
  std::filesystem::create_directory(destination);
  for (auto it = std::filesystem::recursive_directory_iterator(root); it != std::filesystem::recursive_directory_iterator(); ++it) {
    const auto& entry = *it;
    const Path target = destination / entry.path().lexically_relative(root);
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    if (entry.path() == root / "native") {
      it.disable_recursion_pending();
      continue;
    }
    if (entry.is_directory()) {
      std::filesystem::create_directory(target);
    } else if (entry.is_regular_file()) {
      std::filesystem::copy_file(entry.path(), target);
    } else {
      throw std::runtime_error("checkpoint contains non-regular entry");
    }
  }
}

Path
PosixCopyEngine::SourceDirectory(const StorageBackend& source) const
{
  return SourcePath(source, storage_root_);
}

Path
PosixCopyEngine::DestinationDirectory(const StorageBackend& destination) const
{
  return DestinationPath(destination, storage_root_);
}

void
PosixCopyEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  DestinationPath(destination, storage_root_);
}

bool
PosixCopyEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  return std::filesystem::exists(PartialPath(DestinationPath(destination, storage_root_)));
}

void
PosixCopyEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  const Path published = DestinationPath(destination, storage_root_);
  const Path partial = PartialPath(published);
  const Path previous = PreviousPath(published);
  try {
    std::filesystem::create_directories(published.parent_path());
    CopyDirectory(source, partial);
    if (std::filesystem::exists(published)) {
      std::filesystem::rename(published, previous);
      RestorePreviousOnFailure restore_previous(previous, published);
      std::filesystem::rename(partial, published);
      restore_previous.Cancel();
      std::error_code cleanup_error;
      std::filesystem::remove_all(previous, cleanup_error);
      return;
    }
    std::filesystem::rename(partial, published);
  }
  catch (...) {
    std::error_code cleanup_error;
    std::filesystem::remove_all(partial, cleanup_error);
    throw;
  }
}

void
PosixCopyEngine::CopyDirectory(const Path& source, const Path& destination) const
{
  std::filesystem::copy(source, destination, std::filesystem::copy_options::recursive);
}
}  // namespace snapshot::pagebroker
