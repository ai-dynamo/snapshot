// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include <algorithm>
#include <exception>
#include <filesystem>
#include <stdexcept>
#include <thread>
#include <utility>
#include <vector>

namespace snapshot::pagebroker {
namespace {
// Workers split the file list between them, to overlap network round-trips
// on the many small CRIU metadata files without the added complication of
// chunking the few large pages-*.img files that dominate checkpoint size
// (see the PVC<->tmpfs copy's benchmark write-up). 2 workers measured
// within noise of 1; trying 4 to see whether the bottleneck is
// concurrency at all, or storage/network throughput regardless of thread
// count (e.g. a non-multichannel SMB session).
constexpr size_t kCopyWorkerCount = 4;
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

// Copies files.at(begin..end) sequentially, capturing the first failure
// instead of letting an exception cross the thread boundary.
void
CopyFileRange(const std::vector<std::pair<Path, Path>>& files, size_t begin, size_t end, std::exception_ptr& error)
{
  try {
    for (size_t i = begin; i < end; ++i)
      std::filesystem::copy_file(files[i].first, files[i].second);
  }
  catch (...) {
    error = std::current_exception();
  }
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
  std::filesystem::create_directories(destination);

  // Walk once to mirror the directory structure and collect the file list,
  // then copy files across a small worker pool: network-backed storage
  // gets its throughput from concurrent in-flight requests, not from one
  // sequential stream.
  std::vector<std::pair<Path, Path>> files;
  for (const auto& entry : std::filesystem::recursive_directory_iterator(source)) {
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    const Path relative = std::filesystem::relative(entry.path(), source);
    const Path target = destination / relative;
    if (entry.is_directory())
      std::filesystem::create_directories(target);
    else if (entry.is_regular_file())
      files.emplace_back(entry.path(), target);
  }

  if (files.empty())
    return;

  const size_t worker_count = std::min(kCopyWorkerCount, files.size());
  const size_t chunk_size = (files.size() + worker_count - 1) / worker_count;
  std::vector<std::exception_ptr> errors(worker_count);
  std::vector<std::thread> workers;
  workers.reserve(worker_count);
  for (size_t worker = 0; worker < worker_count; ++worker) {
    const size_t begin = worker * chunk_size;
    const size_t end = std::min(begin + chunk_size, files.size());
    workers.emplace_back(CopyFileRange, std::cref(files), begin, end, std::ref(errors[worker]));
  }
  for (auto& thread : workers)
    thread.join();
  for (const auto& error : errors) {
    if (error)
      std::rethrow_exception(error);
  }
}
}  // namespace snapshot::pagebroker
