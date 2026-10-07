// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <optional>

#include "pagebroker_types.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
enum class CheckpointOutput { Staged, Direct };
class CheckpointTransactionDescriptor {
 public:
  // Legacy filesystem-addressed checkpoint.
  CheckpointTransactionDescriptor(
      Path staging_directory, StorageBackend destination_storage, IoEngine engine_type,
      CheckpointOutput output);
  // Artifact-addressed checkpoint: the configured PVCArtifactStore publishes
  // staging_directory into target on Commit instead of a TransferEngine.
  CheckpointTransactionDescriptor(Path staging_directory, ArtifactTarget target);

  const Path& staging_directory() const;
  bool is_artifact_addressed() const;
  // Only valid when !is_artifact_addressed().
  const StorageBackend& destination_storage() const;
  IoEngine engine_type() const;
  CheckpointOutput output() const { return output_; }
  // Only valid when is_artifact_addressed().
  const ArtifactTarget& target() const;

 private:
  Path staging_directory_;
  StorageBackend destination_storage_;
  IoEngine engine_type_ = IoEngine::POSIX_COPY;
  CheckpointOutput output_ = CheckpointOutput::Staged;
  std::optional<ArtifactTarget> target_;
};
}  // namespace snapshot::pagebroker
