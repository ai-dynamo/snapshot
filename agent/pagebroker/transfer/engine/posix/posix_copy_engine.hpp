// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
class PosixCopyEngine final : public TransferEngine {
 public:
  // Retains the roots used to validate filesystem locations. storage_root is the
  // snapshot store; empty when the store is not a filesystem, which refuses every
  // checkpoint. node_cache_root, when set, is a node-local directory whose
  // artifacts/ subtree holds checkpoint copies that a restore may read in
  // place of the store. Nothing is ever written there.
  explicit PosixCopyEngine(Path storage_root, Path node_cache_root = {});

  // Identifies the POSIX transfer implementation.
  IoEngine type() const override;

  // Validates the source and prepares direct access or a CPU staging inventory.
  PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation cancellation = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false,
      TransferMode mode = TransferMode::Staged) const override;

  // Returns direct access or copies CPU files into owned staging.
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation cancellation = {}) const override;

  // Selects a local capture directory appropriate for the requested mode.
  Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode = TransferMode::Staged,
      const PublishedArtifact* artifact = nullptr, Cancellation cancellation = {}) const override;

  // Rejects unsupported artifacts and destinations outside the storage root.
  void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, Cancellation cancellation = {}) const override;

  // Scans local checkpoint output before publication admission.
  CheckpointInventory InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      Cancellation cancellation = {}) const override;

  // Publishes checkpoint output using the filesystem strategy for its mode.
  void PublishCheckpoint(const Path& source, const StorageBackend& destination, CheckpointInventory inventory,
      CheckpointPublication* publication = nullptr, Cancellation cancellation = {},
      TransferMode mode = TransferMode::Staged) const override;

 private:
  // Selects the root a restore source must lie beneath.
  const Path& RestoreRoot(const StorageBackend& source) const;

  Path storage_root_;
  Path node_cache_artifacts_;
};
}  // namespace snapshot::pagebroker
