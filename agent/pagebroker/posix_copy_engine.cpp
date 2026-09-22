// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include "checkpoint_archive.hpp"

#include <filesystem>
#include <stdexcept>

namespace snapshot::pagebroker {
namespace {
// Written by the Go agent directly into the checkpoint staging directory
// (agent/internal/types/manifest.go's manifestFilename) and read back
// directly off the PVC by the operator controller
// (agent/internal/controller/podsnapshotcontent.go's artifactPresent),
// neither of which goes through PageBroker. Must stay a real, uncompressed,
// standalone file at this exact name for both of those to keep working.
const Path kManifestFilename = "manifest.yaml";
const Path kArchiveFilename = "checkpoint.tar.gz";

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

// StoragePath only guards the path *to* a checkpoint directory; this guards
// the two well-known files PageBroker actually reads or writes inside it,
// since it no longer walks the directory's full contents the way a generic
// recursive copy did.
Path
NotSymlink(const Path& path)
{
  if (std::filesystem::is_symlink(path))
    throw std::runtime_error(path.string() + " is a symlink");
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
  const Path published = SourcePath(source, storage_root_);
  // The uncompressed size that will actually land in the tmpfs staging
  // volume, not the smaller compressed size on the PVC -- this sizes the
  // capacity reservation in broker.cpp's StageRestore.
  return ArchiveUncompressedSize(NotSymlink(published / kArchiveFilename)) +
         std::filesystem::file_size(NotSymlink(published / kManifestFilename));
}

void
PosixCopyEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  const Path published = SourcePath(source, storage_root_);
  std::filesystem::create_directories(destination);
  std::filesystem::copy_file(NotSymlink(published / kManifestFilename), destination / kManifestFilename);
  ExtractArchive(NotSymlink(published / kArchiveFilename), destination);
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
    std::filesystem::create_directories(partial);
    std::filesystem::copy_file(NotSymlink(source / kManifestFilename), partial / kManifestFilename);
    ArchiveDirectory(source, partial / kArchiveFilename, kManifestFilename);
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
}  // namespace snapshot::pagebroker
