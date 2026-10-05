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

}  // namespace snapshot::pagebroker
