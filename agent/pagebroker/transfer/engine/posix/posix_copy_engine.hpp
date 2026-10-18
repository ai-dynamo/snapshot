// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class PosixCopyEngine final : public TransferEngine {
 public:
  // Retains the storage root used to validate filesystem locations.
  explicit PosixCopyEngine(Path storage_root);

  // Identifies the POSIX transfer implementation.
  IoEngine type() const override;

  // Validates the source and prepares direct access or a CPU staging plan.
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false,
      TransferMode mode = TransferMode::Staged) const override;

  // Returns direct access or copies CPU files into owned staging.
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation control = {}) const override;

  // Selects a local capture directory appropriate for the requested mode.
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode = TransferMode::Staged,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const override;

  // Rejects unsupported artifacts and destinations outside the storage root.
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const override;

  // Scans local checkpoint output before publication admission.
  RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      Cancellation control = {}) const override;

  // Publishes checkpoint output using the filesystem strategy for its mode.
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, Cancellation control = {},
      TransferMode mode = TransferMode::Staged) const override;

 private:
  Path storage_root_;
};
}  // namespace snapshot::pagebroker
