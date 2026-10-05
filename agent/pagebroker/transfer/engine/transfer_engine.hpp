// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <stdexcept>
#include <string>

#include "pagebroker_types.hpp"
#include "transfer/restore_plan.hpp"
#include "transfer/transfer_control.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class TransferEngineType { POSIX_COPY, MODEL_STREAMER };

class TransferError : public std::runtime_error {
 public:
  TransferError(Failure::Code code, const char* message) : std::runtime_error(message), code(code) {}
  const Failure::Code code;
};

// Retained across Commit retries, including after staging has been released.
struct CheckpointPublication {
  PublishedArtifact artifact;
  std::string pending_index;
  bool published = false;
};

class TransferEngine {
 public:
  virtual ~TransferEngine();
  virtual TransferEngineType type() const = 0;
  virtual RestorePlan PrepareRestore(const StorageBackend& source, TransferControl control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false) const = 0;
  virtual void StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control = {}) const = 0;
  virtual void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, TransferControl control = {}) const = 0;
  // Inspect before admission; publish only after the caller reserves staging capacity.
  virtual RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      TransferControl control = {}) const;
  // Reject destination conflicts before writing, preserving existing publication state.
  virtual void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, TransferControl control = {}) const = 0;

  virtual PublishedArtifact ResolveArtifactTarget(const ArtifactTarget& target) const;
  virtual void ValidateArtifact(const PublishedArtifact& artifact) const;
};
}  // namespace snapshot::pagebroker
