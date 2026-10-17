// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "artifact_store.hpp"

#include <fcntl.h>
#include <unistd.h>

#include <cerrno>
#include <filesystem>
#include <sstream>
#include <system_error>
#include <vector>

#include "file_descriptor.hpp"
#include "identity.hpp"
#include "posix_copy_engine.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr char kManifestFile[] = "manifest.yaml";

bool
IsSafePathComponent(const std::string& value)
{
  return !value.empty() && value != "." && value != ".." && value.find('/') == std::string::npos &&
         value.find('\\') == std::string::npos && value.find('\0') == std::string::npos;
}

Path
PartialPath(const Path& destination)
{
  Path partial = destination;
  partial += ".pagebroker-partial";
  return partial;
}

void
RejectSymlinks(const Path& directory)
{
  for (const auto& entry : fs::recursive_directory_iterator(directory)) {
    if (entry.is_symlink())
      throw ArtifactError(Failure::ARTIFACT_CORRUPT, "checkpoint contains symlink");
  }
}

// status-based probes report "not there" through the error code as well as
// the returned type; only other errors mean the store could not be consulted.
bool
Absent(const std::error_code& error)
{
  return error == std::errc::no_such_file_or_directory || error == std::errc::not_a_directory;
}

// A probe that fails for any reason other than "not there" says nothing about
// whether the publication exists: denied access stays denied, anything else
// is the store being unavailable.
[[noreturn]] void
ThrowStorageProbeError(const std::error_code& error, const std::string& what)
{
  if (error == std::errc::permission_denied)
    throw ArtifactError(Failure::ACCESS_DENIED, what + ": " + error.message());
  throw ArtifactError(Failure::STORAGE_UNAVAILABLE, what + ": " + error.message());
}

void
SyncPath(const Path& path, int flags)
{
  FileDescriptor fd(open(path.c_str(), flags | O_RDONLY | O_CLOEXEC | O_NOFOLLOW));
  if (fd.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open " + path.string() + " for fsync");
  if (fsync(fd.get()) != 0)
    throw std::system_error(errno, std::generic_category(), "fsync " + path.string());
}

void
SyncTree(const Path& directory)
{
  for (const auto& entry : fs::recursive_directory_iterator(directory)) {
    if (entry.is_directory())
      SyncPath(entry.path(), O_DIRECTORY);
    else if (entry.is_regular_file())
      SyncPath(entry.path(), 0);
  }
  SyncPath(directory, O_DIRECTORY);
}

}  // namespace

PVCArtifactStore::PVCArtifactStore(std::string store_id, Path storage_root)
    : store_id_(std::move(store_id)), storage_root_(fs::weakly_canonical(std::move(storage_root)))
{
}

void
PVCArtifactStore::RequireStorageRoot() const
{
  std::error_code error;
  const bool is_directory = fs::is_directory(storage_root_, error);
  if (error && !Absent(error))
    ThrowStorageProbeError(error, "probe artifact store root " + storage_root_.string());
  if (!is_directory)
    throw ArtifactError(
        Failure::STORAGE_UNAVAILABLE, "artifact store root " + storage_root_.string() + " is not a directory");
  fs::directory_iterator(storage_root_, error);
  if (error)
    ThrowStorageProbeError(error, "open artifact store root " + storage_root_.string());
}

Path
PVCArtifactStore::ValidateTarget(const ArtifactTarget& target) const
{
  if (target.store_id() != store_id_)
    throw ArtifactError(
        Failure::STORE_MISMATCH, "artifact target names store " + target.store_id() + ", configured store is " +
                                      store_id_);
  const auto& identity = target.artifact();
  if (!IsSafePathComponent(identity.artifact_uid()) || !IsSafePathComponent(identity.container_name()))
    throw ArtifactError(Failure::INVALID_REQUEST, "artifact target has an invalid artifact UID or container name");
  try {
    ValidateArtifactIdentity(store_id_, identity.artifact_uid(), identity.container_name());
  }
  catch (const std::invalid_argument& error) {
    throw ArtifactError(Failure::INVALID_REQUEST, std::string("artifact target: ") + error.what());
  }

  RequireStorageRoot();
  const Path published =
      storage_root_ / "artifacts" / identity.artifact_uid() / "containers" / identity.container_name();
  const std::string handle = published.lexically_relative(storage_root_).generic_string();
  // The fixed path holds exactly one publication per content and container.
  // A new capture attempt gets a new content UID, so an occupied path is never
  // this attempt's legitimate destination: refuse it before any capture runs,
  // since CRIU terminates the source and a late refusal would strand it.
  std::error_code error;
  const bool published_exists = fs::exists(published, error);
  if (error)
    ThrowStorageProbeError(error, "probe publication at " + handle);
  if (published_exists)
    throw ArtifactError(Failure::TRANSACTION_CONFLICT, "artifact is already published at " + handle);
  const Path partial = PartialPath(published);
  const bool partial_exists = fs::exists(partial, error);
  if (error)
    ThrowStorageProbeError(error, "probe " + partial.string());
  if (partial_exists)
    throw ArtifactError(
        Failure::TRANSACTION_CONFLICT, "artifact publication already in progress at " + partial.string());
  return published;
}

void
PVCArtifactStore::PublishCheckpoint(
    const Path& staging_directory, const ArtifactTarget& target, PublishedArtifact* out) const
{
  const Path published = ValidateTarget(target);
  const std::string handle = published.lexically_relative(storage_root_).generic_string();
  const Path partial = PartialPath(published);
  const Path containers = published.parent_path();
  std::error_code error;
  fs::create_directories(containers, error);
  if (error)
    throw ArtifactError(
        Failure::STORAGE_ERROR, "publish artifact checkpoint: create " + containers.string() + ": " + error.message());
  // The partial is created exclusively: one that already exists belongs to a
  // concurrent publication of this target and is left alone.
  const bool created = fs::create_directory(partial, error);
  if (error)
    throw ArtifactError(
        Failure::STORAGE_ERROR, "publish artifact checkpoint: create " + partial.string() + ": " + error.message());
  if (!created)
    throw ArtifactError(
        Failure::TRANSACTION_CONFLICT, "artifact publication already in progress at " + partial.string());
  try {
    // staging_directory (tmpfs) and published (the PVC) are different
    // filesystems: a rename across them would fail with EXDEV. Copy into
    // partial first; only the final rename, entirely within the PVC, provides
    // the atomicity. The caller removes staging_directory afterward — this
    // does not consume it.
    fs::copy(staging_directory, partial, fs::copy_options::recursive);
    SyncTree(partial);
    fs::rename(partial, published, error);
    if (error == std::errc::file_exists || error == std::errc::directory_not_empty)
      throw ArtifactError(Failure::TRANSACTION_CONFLICT, "artifact is already published at " + handle);
    if (error)
      throw std::system_error(error, "rename " + partial.string());
    // The rename is durable once containers/ is synced; the directories
    // create_directories may have added above it need their parents synced too.
    SyncPath(containers, O_DIRECTORY);
    SyncPath(containers.parent_path(), O_DIRECTORY);
    SyncPath(storage_root_ / "artifacts", O_DIRECTORY);
  }
  catch (const ArtifactError&) {
    std::error_code cleanup_error;
    fs::remove_all(partial, cleanup_error);
    throw;
  }
  catch (const std::exception& error) {
    std::error_code cleanup_error;
    fs::remove_all(partial, cleanup_error);
    throw ArtifactError(Failure::STORAGE_ERROR, std::string("publish artifact checkpoint: ") + error.what());
  }
  out->Clear();
  out->set_store_id(store_id_);
  out->set_artifact_handle(handle);
}

RestorePlan
PVCArtifactStore::ResolveRestorePlan(const PublishedArtifact& artifact) const
{
  if (artifact.store_id() != store_id_)
    throw ArtifactError(
        Failure::STORE_MISMATCH, "published artifact names store " + artifact.store_id() + ", configured store is " +
                                      store_id_);

  // The handle is a relative path this store itself produced (see
  // PublishCheckpoint): exactly artifacts/<uid>/containers/<name>, which keeps
  // it within storage_root_, with no symlink component, the same containment
  // rule PosixCopyEngine applies to a caller-supplied legacy filesystem path.
  const std::string& handle = artifact.artifact_handle();
  std::vector<std::string> parts;
  std::istringstream segments(handle);
  for (std::string part; std::getline(segments, part, '/');) parts.push_back(part);
  if (parts.size() != 4 || parts[0] != "artifacts" || parts[2] != "containers" || !IsSafePathComponent(parts[1]) ||
      !IsSafePathComponent(parts[3]))
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "published artifact handle is not a valid locator");
  try {
    ValidateArtifactIdentity(store_id_, parts[1], parts[3]);
  }
  catch (const std::invalid_argument&) {
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "published artifact handle names an invalid artifact identity");
  }
  const Path container_directory = storage_root_ / "artifacts" / parts[1] / "containers" / parts[3];

  RequireStorageRoot();
  std::error_code error;
  Path component = storage_root_;
  for (const auto& part : parts) {
    component /= part;
    const bool is_symlink = fs::is_symlink(component, error);
    if (error && !Absent(error))
      ThrowStorageProbeError(error, "probe " + component.string());
    if (is_symlink)
      throw ArtifactError(Failure::ARTIFACT_CORRUPT, "published artifact handle contains a symlink");
  }

  const bool is_directory = fs::is_directory(container_directory, error);
  if (error && !Absent(error))
    ThrowStorageProbeError(error, "probe publication at " + handle);
  if (!is_directory)
    throw ArtifactError(Failure::ARTIFACT_NOT_FOUND, "no publication at " + handle);
  const bool has_manifest = fs::is_regular_file(container_directory / kManifestFile, error);
  if (error && !Absent(error))
    ThrowStorageProbeError(error, "probe manifest at " + handle);
  if (!has_manifest)
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "publication at " + handle + " is missing manifest.yaml");

  return RestorePlan{container_directory};
}

void
PVCArtifactStore::StageMetadata(const RestorePlan& plan, const Path& destination) const
{
  fs::create_directories(destination);
  std::error_code error;
  fs::copy_file(
      plan.container_directory / kManifestFile, destination / kManifestFile, fs::copy_options::overwrite_existing,
      error);
  if (error)
    throw ArtifactError(Failure::STORAGE_ERROR, "stage artifact metadata: " + error.message());
}

void
PVCArtifactStore::StageRestore(const RestorePlan& plan, const Path& destination) const
{
  RejectSymlinks(plan.container_directory);
  // GPU restore reads its payload through the retained source directory fd.
  // Only CPU images belong in tmpfs, just as for legacy filesystem restores.
  StorageBackend source;
  source.mutable_filesystem()->set_directory(plan.container_directory.string());
  PosixCopyEngine(storage_root_).StageRestore(source, destination);
}

}  // namespace snapshot::pagebroker
