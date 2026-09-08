#include "model_streamer_transfer_engine.hpp"

#include <filesystem>
#include <stdexcept>
#include <string>
#include <system_error>
#include <utility>

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
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
    if (fs::is_symlink(component))
      throw std::invalid_argument(std::string(label) + " contains symlink");
  }
  return path;
}

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
  for (const auto& entry : fs::recursive_directory_iterator(path)) {
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    if (entry.is_regular_file())
      bytes += entry.file_size();
  }
  return bytes;
}

RestorePlan
BuildFilesystemRestorePlan(const Path& source)
{
  RestorePlan plan;
  plan.root_permissions = fs::symlink_status(source).permissions();
  for (const auto& entry : fs::recursive_directory_iterator(source)) {
    const auto status = entry.symlink_status();
    if (fs::is_symlink(status))
      throw std::runtime_error("checkpoint contains symlink");

    const Path relative = entry.path().lexically_relative(source);
    if (relative.empty() || relative == "." || relative == ".." || relative.string().starts_with("../"))
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
  return plan;
}
}  // namespace

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root)
    : storage_root_(fs::weakly_canonical(std::move(storage_root)))
{
}

TransferEngineType
ModelStreamerTransferEngine::type() const
{
  return TransferEngineType::MODEL_STREAMER;
}

uintmax_t
ModelStreamerTransferEngine::RestoreSize(const StorageBackend& source) const
{
  return DirectorySize(SourcePath(source, storage_root_));
}

void
ModelStreamerTransferEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  restore_.Stage(BuildFilesystemRestorePlan(SourcePath(source, storage_root_)), destination);
}

void
ModelStreamerTransferEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  DestinationPath(destination, storage_root_);
}

bool
ModelStreamerTransferEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  return fs::exists(PartialPath(DestinationPath(destination, storage_root_)));
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  const Path published = DestinationPath(destination, storage_root_);
  const Path partial = PartialPath(published);
  try {
    fs::create_directories(published.parent_path());
    fs::copy(source, partial, fs::copy_options::recursive);
    fs::remove_all(published);
    fs::rename(partial, published);
  }
  catch (...) {
    std::error_code cleanup_error;
    fs::remove_all(partial, cleanup_error);
    throw;
  }
}
}  // namespace snapshot::pagebroker
