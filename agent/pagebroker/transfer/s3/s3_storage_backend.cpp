// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_storage_backend.hpp"

#include <algorithm>
#include <filesystem>
#include <limits>
#include <map>
#include <set>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
bool
HasCompleteArtifactIdentity(const ArtifactTarget& target)
{
  if (target.store_id().empty())
    return false;
  const auto& identity = target.artifact();
  return !identity.artifact_uid().empty() && !identity.container_name().empty();
}

bool
FitsUploadPlatformSize(uintmax_t bytes)
{
  return bytes <= std::numeric_limits<std::uint64_t>::max() &&
         bytes <= std::numeric_limits<std::size_t>::max();
}

std::map<Path, const S3ObjectLocation*>
ValidateS3UploadPlan(const S3CheckpointUploadPlan& input, const RestorePlan& plan,
                     const S3Limits& limits)
{
  std::map<Path, const S3ObjectLocation*> destinations;
  std::set<std::pair<std::string, std::string>> keys;
  for (const auto& file : input.files) {
    if (!IsSafeRelativePath(file.relative_path) || !destinations.emplace(file.relative_path, &file.destination).second)
      throw std::invalid_argument("S3 checkpoint contains an unsafe or duplicate file mapping");
    file.destination.Validate();
    if (!keys.emplace(file.destination.bucket, file.destination.key).second)
      throw std::invalid_argument("S3 checkpoint contains a duplicate destination");
  }
  if (destinations.size() != plan.files.size())
    throw std::invalid_argument("S3 checkpoint must map every regular file exactly once");
  for (const auto& file : plan.files) {
    if (!destinations.contains(file.relative_path))
      throw std::invalid_argument("S3 checkpoint file mapping does not match the source tree");
    if (!FitsUploadPlatformSize(file.size_bytes))
      throw std::invalid_argument("S3 checkpoint file exceeds platform size limits");
    limits.ValidateFileSize(file.size_bytes);
  }
  return destinations;
}
}  // namespace

S3StorageBackend::S3StorageBackend(S3Config config) : config_(std::move(config))
{
  config_.transfer.connection.Validate();
  config_.transfer.upload_limits.Validate();
  if (!config_.bucket.empty())
    S3ObjectLocation{config_.bucket, "index.json"}.Validate();
}

const S3Config&
S3StorageBackend::config() const
{
  return config_;
}

S3Client&
S3StorageBackend::Client() const
{
  std::call_once(client_once_, [this] {
    client_ = std::make_unique<S3Client>(config_.transfer.connection, config_.transfer.upload_limits);
  });
  return *client_;
}

RestorePlan
S3StorageBackend::UploadCheckpoint(const S3CheckpointUploadPlan& input, const S3UploadOptions& options) const
{
  options.control.Check();
  auto plan = filesystem_storage::BuildRestorePlan(input.source_directory, options.control);

  // Validate the entire tree before any remote writes.
  const auto destinations = ValidateS3UploadPlan(input, plan, config_.transfer.upload_limits);
  for (auto& file : plan.files) {
    const auto& destination = *destinations.at(file.relative_path);
    file.source_locator = "s3://" + destination.bucket + "/" + destination.key;
  }
  return UploadFiles(input.source_directory, std::move(plan), options);
}

RestorePlan
S3StorageBackend::UploadFiles(const Path& source, RestorePlan plan, const S3UploadOptions& options) const
{
  for (auto& file : plan.files) {
    options.control.Check();
    const auto slash = file.source_locator.find('/', 5);
    const S3ObjectLocation destination{file.source_locator.substr(5, slash - 5), file.source_locator.substr(slash + 1)};
    const auto result = Client().UploadFile(source / file.relative_path, destination, options);
    if (result.size_bytes != file.size_bytes) {
      S3Error error({"upload checkpoint", "SourceSizeChanged"});
      error.destination = destination;
      error.remote_state = S3RemoteState::COMPLETED;
      throw error;
    }
    file.expected_sha256 = result.sha256;
  }
  return plan;
}

PublishedArtifact
S3StorageBackend::ResolveTarget(const ArtifactTarget& target) const
{
  const auto& identity = target.artifact();
  if (!HasCompleteArtifactIdentity(target))
    throw std::invalid_argument("artifact target requires store ID, UID, and container name");
  if (target.store_id() != config_.store_id)
    throw CheckpointError(Failure::STORE_MISMATCH, "target belongs to another store");
  // Length prefixes keep identity tuples distinct; names never become paths.
  std::string input;
  for (const auto* part : {&target.store_id(), &identity.artifact_uid(), &identity.container_name()})
    input += std::to_string(part->size()) + ":" + *part;
  const auto digest = utils::ComputeSha256(std::as_bytes(std::span(input.data(), input.size())));
  constexpr char hex[] = "0123456789abcdef";
  std::string handle;
  for (auto byte : digest) {
    handle += hex[byte >> 4];
    handle += hex[byte & 15];
  }
  PublishedArtifact artifact;
  artifact.set_store_id(config_.store_id);
  artifact.set_artifact_handle(handle);
  artifact.set_artifact_format_version(kCheckpointFormat);
  ValidateCheckpoint(artifact);
  return artifact;
}

void
S3StorageBackend::ValidateCheckpoint(const PublishedArtifact& storage) const
{
  if (config_.bucket.empty() || config_.store_id.empty())
    throw std::invalid_argument("S3 checkpoint storage is not configured");
  if (storage.store_id().empty() || storage.artifact_format_version().empty())
    throw std::invalid_argument("artifact requires store ID, handle, and format version");
  if (storage.store_id() != config_.store_id)
    throw CheckpointError(Failure::STORE_MISMATCH, "artifact belongs to another store");
  if (storage.artifact_format_version() != kCheckpointFormat)
    throw CheckpointError(Failure::UNSUPPORTED_ARTIFACT, "unsupported artifact format");
  ValidateCheckpointID(storage.artifact_handle());
}

S3ObjectLocation
S3StorageBackend::Object(const PublishedArtifact& storage, const std::string& key) const
{
  ValidateCheckpoint(storage);
  const auto prefix = config_.prefix.empty() ? "" : config_.prefix + "/";
  return {config_.bucket, prefix + storage.artifact_handle() + "/" + key};
}

bool
S3StorageBackend::CheckpointExists(const PublishedArtifact& storage, TransferControl control) const
{
  const auto object = Object(storage, "index.json");
  return Client().Get(object, kMaxIndexBytes, control).has_value();
}

void
S3StorageBackend::PrepareCheckpoint(const PublishedArtifact& storage, TransferControl control) const
{
  if (CheckpointExists(storage, control))
    throw CheckpointError(Failure::TRANSACTION_CONFLICT, "checkpoint is already published; recover using its ID");
}

RestorePlan
S3StorageBackend::InspectCheckpoint(const Path& source, const PublishedArtifact& storage, TransferControl control) const
{
  ValidateCheckpoint(storage);
  auto plan = filesystem_storage::BuildRestorePlan(source, control, kMaxCheckpointEntries);
  (void)ValidateCheckpointPlan(plan, false);
  for (auto& file : plan.files) {
    const auto object = Object(storage, "data/" + file.relative_path.generic_string());
    object.Validate();
    config_.transfer.upload_limits.ValidateFileSize(file.size_bytes);
    file.source_locator = "s3://" + object.bucket + "/" + object.key;
    // Digests have fixed encoded length. Bound the final index before writes
    // without a second checksum pass over every source file.
    file.expected_sha256 = utils::Sha256Digest{};
  }
  (void)SerializeCheckpointIndex(plan);
  for (auto& file : plan.files)
    file.expected_sha256.reset();
  return plan;
}

std::string
S3StorageBackend::UploadCheckpointPayloads(const Path& source, RestorePlan plan, TransferControl control) const
{
  return SerializeCheckpointIndex(UploadFiles(source, std::move(plan), S3UploadOptions{control}));
}

bool
S3StorageBackend::ConfirmIndex(const S3ObjectLocation& object, const std::string& index, TransferControl control) const
{
  const auto existing = Client().Get(object, kMaxIndexBytes, control);
  if (!existing)
    return false;
  if (*existing != index)
    throw CheckpointError(Failure::TRANSACTION_CONFLICT, "checkpoint contains a different index");
  return true;
}

void
S3StorageBackend::PublishIndex(const PublishedArtifact& storage, const std::string& index, TransferControl control) const
{
  try {
    const auto object = Object(storage, "index.json");
    auto& client = Client();
    // The checkpoint publication state saves these exact bytes before publication. Every
    // error here remains uncertain independently of concurrent Abort/expiry.
    if (ConfirmIndex(object, index, control))
      return;
    try {
      client.Put(object, index, kMaxIndexBytes, control);
      return;
    }
    catch (...) {
      if (ConfirmIndex(object, index, control))
        return;
    }
  }
  catch (const CheckpointError&) {
    throw;
  }
  catch (...) {
  }
  throw CheckpointError(Failure::OUTCOME_UNKNOWN, "checkpoint publication could not be confirmed; recover without recapturing");
}

RestorePlan
S3StorageBackend::LoadRestorePlan(const PublishedArtifact& storage, bool metadata_only, TransferControl control) const
{
  const auto object = Object(storage, "index.json");
  auto& client = Client();
  const auto text = client.Get(object, kMaxIndexBytes, control);
  if (!text)
    throw CheckpointError(Failure::ARTIFACT_NOT_FOUND, "checkpoint index not found");
  auto plan = ParseCheckpointIndex(*text);
  if (metadata_only) {
    std::erase_if(plan.files, [](const auto& file) { return file.relative_path != "manifest.yaml"; });
    plan.directories.clear();
    plan.root_permissions = fs::perms::owner_all;
    plan.files.front().permissions = fs::perms::owner_read | fs::perms::owner_write;
  }
  for (auto& file : plan.files) {
    const auto payload = Object(storage, "data/" + file.relative_path.generic_string());
    payload.Validate();
    const auto size = client.Head(payload, control);
    if (!size || *size != file.size_bytes)
      throw CheckpointError(Failure::ARTIFACT_CORRUPT, "checkpoint file is absent or has the wrong size");
    file.source_locator = "s3://" + payload.bucket + "/" + payload.key;
  }
  return plan;
}

}  // namespace snapshot::pagebroker
