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
  CopyDirectory(SourcePath(source, storage_root_), destination);
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
  try {
    std::filesystem::create_directories(published.parent_path());
    CopyDirectory(source, partial);
    std::filesystem::remove_all(published);
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
