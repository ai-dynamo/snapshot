// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_storage_backend.hpp"

#include <algorithm>
#include <filesystem>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"
#include "transfer/engine/transfer_engine.hpp"
#include "utils/sha256.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
constexpr char kS3UriPrefix[] = "s3://";
constexpr char kIndexObjectName[] = "index.json";
constexpr char kPayloadDirectory[] = "data";
const std::string kPayloadPrefix = std::string(kPayloadDirectory) + "/";

bool
HasCompleteArtifactIdentity(const ArtifactTarget& target)
{
  if (target.store_id().empty())
    return false;
  const auto& identity = target.artifact();
  return !identity.artifact_uid().empty() && !identity.container_name().empty();
}

}  // namespace

S3StorageBackend::S3StorageBackend(S3Config config) : config_(std::move(config))
{
  if (config_.bucket.empty() || config_.store_id.empty())
    throw std::invalid_argument("S3 checkpoint storage is not configured");
  config_.transfer.Validate();
  // Every checkpoint needs this payload key, which is longer than the index key.
  // Check longer capture-specific paths when building the inventory.
  PublishedArtifact artifact;
  artifact.set_artifact_handle(std::string(kCheckpointHandleLength, kCheckpointHandleHexDigits[0]));
  Object(artifact, std::string(kPayloadPrefix) + kCheckpointManifestName).Validate();
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

void
S3StorageBackend::UploadFiles(const Path& source, const CheckpointInventory& inventory,
    const PublishedArtifact& storage, Cancellation cancellation) const
{
  std::vector<S3UploadFile> files;
  files.reserve(inventory.files.size());
  for (const auto& file : inventory.files) {
    cancellation.ThrowIfCancelled();
    files.push_back({file.relative_path, file.size_bytes});
  }
  // Source files and this destination prefix must remain exclusive to the caller.
  Client().UploadFiles(source, Object(storage, kPayloadDirectory), files, cancellation);
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
  std::string handle;
  for (auto byte : digest) {
    handle += kCheckpointHandleHexDigits[byte >> 4];
    handle += kCheckpointHandleHexDigits[byte & 15];
  }
  PublishedArtifact artifact;
  artifact.set_store_id(config_.store_id);
  artifact.set_artifact_handle(handle);
  return artifact;
}

void
S3StorageBackend::ValidateArtifactDescriptor(const PublishedArtifact& storage) const
{
  if (storage.store_id().empty())
    throw std::invalid_argument("artifact requires store ID and handle");
  if (storage.store_id() != config_.store_id)
    throw CheckpointError(Failure::STORE_MISMATCH, "artifact belongs to another store");
  ValidateCheckpointID(storage.artifact_handle());
}

S3ObjectLocation
S3StorageBackend::Object(const PublishedArtifact& storage, const std::string& key) const
{
  const auto prefix = config_.prefix.empty() ? "" : config_.prefix + "/";
  return {config_.bucket, prefix + storage.artifact_handle() + "/" + key};
}

void
S3StorageBackend::CheckCheckpointDestination(const PublishedArtifact& storage, Cancellation cancellation) const
{
  const auto object = Object(storage, kIndexObjectName);
  if (Client().Head(object, cancellation).has_value())
    throw CheckpointError(Failure::TRANSACTION_CONFLICT, "checkpoint is already published; recover using its ID");
}

CheckpointInventory
S3StorageBackend::BuildCheckpointInventory(const Path& source, const PublishedArtifact& storage, Cancellation cancellation) const
{
  auto inventory = filesystem_storage::BuildCheckpointInventory(source, cancellation, kMaxCheckpointEntries);
  (void)SerializeCheckpointIndex(inventory);
  for (const auto& file : inventory.files) {
    const auto object = Object(storage, kPayloadPrefix + file.relative_path.generic_string());
    object.Validate();
    config_.transfer.upload_limits.ValidateFileSize(file.size_bytes);
  }
  return inventory;
}

std::string
S3StorageBackend::UploadCheckpointPayloads(const Path& source, const CheckpointInventory& inventory,
    const PublishedArtifact& storage, Cancellation cancellation) const
{
  UploadFiles(source, inventory, storage, cancellation);
  return SerializeCheckpointIndex(inventory);
}

bool
S3StorageBackend::ConfirmIndex(const S3ObjectLocation& object, const std::string& index, Cancellation cancellation) const
{
  const auto existing = Client().Get(object, kMaxIndexBytes, cancellation);
  if (!existing)
    return false;
  if (*existing != index)
    throw CheckpointError(Failure::TRANSACTION_CONFLICT, "checkpoint contains a different index");
  return true;
}

void
S3StorageBackend::PublishIndex(const PublishedArtifact& storage, const std::string& index, Cancellation cancellation) const
{
  try {
    const auto object = Object(storage, kIndexObjectName);
    auto& client = Client();
    // The checkpoint publication state saves these exact bytes before publication. Every
    // error here remains uncertain independently of concurrent Abort/expiry.
    if (ConfirmIndex(object, index, cancellation))
      return;
    try {
      client.Put(object, index, kMaxIndexBytes, cancellation);
      return;
    }
    catch (...) {
      if (ConfirmIndex(object, index, cancellation))
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

CheckpointInventory
S3StorageBackend::LoadCheckpointInventory(const PublishedArtifact& storage, bool metadata_only, Cancellation cancellation) const
{
  const auto object = Object(storage, kIndexObjectName);
  auto& client = Client();
  const auto text = client.Get(object, kMaxIndexBytes, cancellation);
  if (!text)
    throw CheckpointError(Failure::ARTIFACT_NOT_FOUND, "checkpoint index not found");
  auto inventory = ParseCheckpointIndex(*text);
  if (metadata_only) {
    std::erase_if(inventory.files, [](const auto& file) { return file.relative_path != kCheckpointManifestName; });
    inventory.directories.clear();
    inventory.root_permissions = fs::perms::owner_all;
    inventory.files.front().permissions = fs::perms::owner_read | fs::perms::owner_write;
  }
  for (auto& file : inventory.files) {
    const auto payload = Object(storage, kPayloadPrefix + file.relative_path.generic_string());
    const auto size = client.Head(payload, cancellation);
    if (!size || *size != file.size_bytes)
      throw CheckpointError(Failure::ARTIFACT_CORRUPT, "checkpoint file is absent or has the wrong size");
    file.source_locator = kS3UriPrefix + payload.bucket + "/" + payload.key;
  }
  return inventory;
}

PreparedRestore
S3StorageBackend::PrepareRestore(const PublishedArtifact& storage, bool metadata_only,
    TransferMode mode, Cancellation cancellation) const
{
  if (mode == TransferMode::Direct)
    throw std::invalid_argument("S3 restores require staging");
  PreparedRestore prepared;
  prepared.inventory = LoadCheckpointInventory(storage, metadata_only, cancellation);
  return prepared;
}

void
S3StorageBackend::PublishCheckpoint(const Path& source, CheckpointInventory inventory, CheckpointPublication& publication,
    Cancellation cancellation) const
{
  if (publication.pending_index.empty()) {
    CheckCheckpointDestination(publication.artifact, cancellation);
    publication.pending_index = UploadCheckpointPayloads(source, inventory, publication.artifact, cancellation);
  }
  // Retry the saved index verbatim when the preceding publication outcome was uncertain.
  PublishIndex(publication.artifact, publication.pending_index, cancellation);
  publication.published = true;
}

}  // namespace snapshot::pagebroker
