// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include <filesystem>
#include <stdexcept>
#include <system_error>
#include <fcntl.h>
#include <linux/fs.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include "file_descriptor.hpp"

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
  for (const auto& entry : std::filesystem::recursive_directory_iterator(path)) {
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
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
  for (const auto& entry : std::filesystem::recursive_directory_iterator(root)) {
    const Path target = destination / entry.path().lexically_relative(root);
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    if (entry.is_directory()) {
      std::filesystem::create_directory(target);
    } else if (entry.is_regular_file()) {
      // A reflink retains the staged-restore contract: independently writable
      // files, even when a consumer rewrites metadata. Never substitute hard
      // links, which would let a restore corrupt the published checkpoint.
      FileDescriptor input(open(entry.path().c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW));
      FileDescriptor output(open(target.c_str(), O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600));
      if (input.get() < 0 || output.get() < 0)
        throw std::system_error(errno, std::generic_category(), "open restore clone");
      if (ioctl(output.get(), FICLONE, input.get()) != 0) {
        if (errno != EXDEV && errno != EOPNOTSUPP && errno != ENOTTY && errno != EINVAL)
          throw std::system_error(errno, std::generic_category(), "clone restore file");
        // NFSv4.2 can perform COPY server-side even when CLONE is unavailable.
        // Unlike hard links this still creates independently writable files.
        uintmax_t remaining = entry.file_size();
        while (remaining) {
          ssize_t copied = copy_file_range(input.get(), nullptr, output.get(), nullptr,
                                          std::min<uintmax_t>(remaining, 1ULL << 30), 0);
          if (copied > 0) {
            remaining -= copied;
          } else if (copied < 0 && errno == EINTR) {
            continue;
          } else if (copied == 0 || errno == EXDEV || errno == EOPNOTSUPP || errno == ENOSYS ||
                     errno == EINVAL || errno == EPERM) {
            std::filesystem::copy_file(entry.path(), target, std::filesystem::copy_options::overwrite_existing);
            break;
          } else {
            throw std::system_error(errno, std::generic_category(), "copy restore file");
          }
        }
      }
      std::filesystem::permissions(target, entry.status().permissions());
    } else {
      throw std::runtime_error("checkpoint contains non-regular entry");
    }
  }
}

FileDescriptor
PosixCopyEngine::OpenRestoreSource(const StorageBackend& source) const
{
  const auto path = SourcePath(source, storage_root_);
  // Match staged restore's source validation, without reading file contents.
  DirectorySize(path);
  FileDescriptor directory(open(path.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  if (directory.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open direct restore source");
  return directory;
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
  bool moved = false;
  try {
    std::filesystem::create_directories(published.parent_path());
    std::error_code error;
    std::filesystem::rename(source, partial, error);
    if (!error) {
      moved = true;
    } else if (error == std::errc::cross_device_link) {
      CopyDirectory(source, partial);
    } else {
      throw std::filesystem::filesystem_error("stage publication", source, partial, error);
    }
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
    if (moved) {
      // Preserve the transaction's input for retry or abort when publication
      // failed after the same-filesystem move.
      std::filesystem::rename(partial, source, cleanup_error);
    } else {
      std::filesystem::remove_all(partial, cleanup_error);
    }
    throw;
  }
}

void
PosixCopyEngine::CopyDirectory(const Path& source, const Path& destination) const
{
  std::filesystem::copy(source, destination, std::filesystem::copy_options::recursive);
}
}  // namespace snapshot::pagebroker
