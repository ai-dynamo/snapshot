// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "artifact_store.hpp"

#include <google/protobuf/struct.pb.h>
#include <google/protobuf/util/json_util.h>

#include <filesystem>
#include <fstream>
#include <iterator>
#include <sstream>
#include <utility>

#include "identity.hpp"
#include "posix_copy_engine.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr char kPublicationFile[] = "publication.json";

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

Path
PreviousPath(const Path& destination)
{
  Path previous = destination;
  previous += ".pagebroker-previous";
  return previous;
}

class RestorePreviousOnFailure {
 public:
  RestorePreviousOnFailure(const Path& previous, const Path& published) : previous_(previous), published_(published)
  {
  }

  ~RestorePreviousOnFailure()
  {
    if (!cancelled_) {
      std::error_code error;
      fs::rename(previous_, published_, error);
    }
  }

  void Cancel() { cancelled_ = true; }

 private:
  const Path& previous_;
  const Path& published_;
  bool cancelled_ = false;
};

void
RejectSymlinks(const Path& directory)
{
  for (const auto& entry : fs::recursive_directory_iterator(directory)) {
    if (entry.is_symlink())
      throw ArtifactError(Failure::ARTIFACT_CORRUPT, "checkpoint contains symlink");
  }
}

void
WritePublication(const Path& path, const PublishedArtifact& artifact, const std::string& commit_id)
{
  google::protobuf::Struct document;
  auto& fields = *document.mutable_fields();
  fields["storeId"].set_string_value(artifact.store_id());
  fields["artifactHandle"].set_string_value(artifact.artifact_handle());
  fields["artifactFormatVersion"].set_string_value(artifact.artifact_format_version());
  fields["commitId"].set_string_value(commit_id);

  std::string json;
  google::protobuf::util::JsonPrintOptions options;
  options.add_whitespace = true;
  const auto status = google::protobuf::util::MessageToJsonString(document, &json, options);
  if (!status.ok())
    throw ArtifactError(Failure::STORAGE_ERROR, "encode publication evidence: " + status.ToString());

  std::ofstream file(path, std::ios::trunc);
  if (!file.is_open())
    throw ArtifactError(Failure::STORAGE_ERROR, "write publication evidence: open " + path.string() + " failed");
  file << json;
  if (!file.good())
    throw ArtifactError(Failure::STORAGE_ERROR, "write publication evidence: write " + path.string() + " failed");
}

struct Publication {
  std::string store_id;
  std::string artifact_handle;
  std::string artifact_format_version;
  std::string commit_id;
};

Publication
ReadPublication(const Path& path)
{
  std::ifstream file(path);
  if (!file.is_open())
    throw ArtifactError(Failure::ARTIFACT_NOT_FOUND, "publication evidence not found at " + path.string());
  const std::string json((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());

  google::protobuf::Struct document;
  const auto status = google::protobuf::util::JsonStringToMessage(json, &document);
  if (!status.ok())
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "publication evidence is not valid JSON: " + status.ToString());

  const auto& fields = document.fields();
  for (const char* key : {"storeId", "artifactHandle", "artifactFormatVersion", "commitId"}) {
    if (!fields.contains(key) || fields.at(key).kind_case() != google::protobuf::Value::kStringValue)
      throw ArtifactError(Failure::ARTIFACT_CORRUPT, std::string("publication evidence missing field ") + key);
  }
  return Publication{
      fields.at("storeId").string_value(), fields.at("artifactHandle").string_value(),
      fields.at("artifactFormatVersion").string_value(), fields.at("commitId").string_value()};
}

}  // namespace

PVCArtifactStore::PVCArtifactStore(std::string store_id, Path storage_root)
    : store_id_(std::move(store_id)), storage_root_(fs::weakly_canonical(std::move(storage_root)))
{
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
  return storage_root_ / "artifacts" / identity.artifact_uid() / "containers" / identity.container_name();
}

void
PVCArtifactStore::PublishCheckpoint(
    const Path& staging_directory, const ArtifactTarget& target, PublishedArtifact* out) const
{
  const Path published = ValidateTarget(target);
  const std::string commit_id =
      ComputeCommitID(store_id_, target.artifact().artifact_uid(), target.artifact().container_name());
  const std::string handle = published.lexically_relative(storage_root_).generic_string();

  PublishedArtifact artifact;
  artifact.set_store_id(store_id_);
  artifact.set_artifact_handle(handle);
  artifact.set_artifact_format_version(kFilesystemFormatVersion);
  WritePublication(staging_directory / kPublicationFile, artifact, commit_id);

  const Path partial = PartialPath(published);
  const Path previous = PreviousPath(published);
  try {
    fs::create_directories(published.parent_path());
    // staging_directory (tmpfs) and published (the PVC) are different
    // filesystems: a rename across them would fail with EXDEV. Copy into
    // partial first; only the partial/previous/published shuffle below,
    // entirely within the PVC, uses rename for its atomicity. The caller
    // removes staging_directory afterward — this does not consume it.
    fs::copy(staging_directory, partial, fs::copy_options::recursive);
    if (fs::exists(published)) {
      fs::rename(published, previous);
      RestorePreviousOnFailure restore_previous(previous, published);
      fs::rename(partial, published);
      restore_previous.Cancel();
      std::error_code cleanup_error;
      fs::remove_all(previous, cleanup_error);
    } else {
      fs::rename(partial, published);
    }
  }
  catch (const std::exception& error) {
    std::error_code cleanup_error;
    fs::remove_all(partial, cleanup_error);
    throw ArtifactError(Failure::STORAGE_ERROR, std::string("publish artifact checkpoint: ") + error.what());
  }
  *out = std::move(artifact);
}

RestorePlan
PVCArtifactStore::ResolveRestorePlan(const PublishedArtifact& artifact) const
{
  if (artifact.store_id() != store_id_)
    throw ArtifactError(
        Failure::STORE_MISMATCH, "published artifact names store " + artifact.store_id() + ", configured store is " +
                                      store_id_);

  // The handle is a relative path this store itself produced (see
  // PublishCheckpoint); it must stay within storage_root_ with no symlink
  // component, the same containment rule PosixCopyEngine applies to a
  // caller-supplied legacy filesystem path.
  const Path requested(artifact.artifact_handle());
  const Path container_directory = storage_root_ / requested;
  const Path normalized = container_directory.lexically_normal();
  const Path relative = normalized.lexically_relative(storage_root_);
  if (requested.is_absolute() || relative.empty() || relative == "." || relative.native().starts_with("..") ||
      normalized != container_directory)
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "published artifact handle is not a valid locator");

  Path component = storage_root_;
  for (const auto& part : relative) {
    component /= part;
    if (fs::is_symlink(component))
      throw ArtifactError(Failure::ARTIFACT_CORRUPT, "published artifact handle contains a symlink");
  }

  if (!fs::is_directory(container_directory))
    throw ArtifactError(Failure::ARTIFACT_NOT_FOUND, "no publication at " + artifact.artifact_handle());

  const Publication publication = ReadPublication(container_directory / kPublicationFile);
  if (publication.artifact_format_version != kFilesystemFormatVersion)
    throw ArtifactError(
        Failure::UNSUPPORTED_ARTIFACT, "publication format " + publication.artifact_format_version + " is not supported");
  if (publication.store_id != store_id_ || publication.artifact_handle != artifact.artifact_handle())
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "publication evidence does not match its own location");

  return RestorePlan{container_directory};
}

void
PVCArtifactStore::StageMetadata(const RestorePlan& plan, const Path& destination) const
{
  const Path manifest = plan.container_directory / "manifest.yaml";
  if (!fs::is_regular_file(manifest))
    throw ArtifactError(Failure::ARTIFACT_CORRUPT, "publication is missing manifest.yaml");
  fs::create_directories(destination);
  std::error_code error;
  fs::copy_file(manifest, destination / "manifest.yaml", fs::copy_options::overwrite_existing, error);
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
