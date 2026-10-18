// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <stdexcept>
#include <string>

#include "pagebroker_types.hpp"
#include "transfer/restore_plan.hpp"
#include "cancellation.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class IoEngine { POSIX_COPY, NIXL };

namespace io {
enum class Operation { Read, Write };
}

class TransferError : public std::runtime_error {
 public:
  // Creates a transfer error with a wire failure code and diagnostic message.
  TransferError(Failure::Code code, const char* message) : std::runtime_error(message), code(code) {}
  const Failure::Code code;
};

// Preserves explicit transfer outcomes and distinguishes interruption from I/O failure.
Failure::Code TransferFailureCode(const std::exception& error);

// Retained across Commit retries, including after staging has been released.
struct CheckpointPublication {
  PublishedArtifact artifact;
  std::string pending_index;
  bool published = false;
};

// Engines reject operations they do not implement.
class TransferEngine {
 public:
  // Destroys the engine through its base interface.
  virtual ~TransferEngine();

  // Returns the engine kind used for broker selection.
  virtual IoEngine type() const = 0;

  // Registers the file descriptor and size for buffered I/O.
  virtual void Open(int descriptor, size_t size);

  // Starts a read or write through the selected buffer slot.
  virtual void Submit(size_t slot, io::Operation operation, size_t offset, size_t size);

  // Waits for a slot's transfer to finish and releases its request.
  virtual void Wait(size_t slot);

  // Drains outstanding transfers and unregisters the file.
  virtual void Close();

  // Resolves the source and retains local access before staging begins.
  virtual PreparedRestore PrepareRestore(const StorageBackend& source, Cancellation control = {},
      const PublishedArtifact* artifact = nullptr, bool metadata_only = false,
      TransferMode mode = TransferMode::Staged) const;

  // Makes the prepared restore accessible and identifies transaction-owned staging.
  virtual RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation control = {}) const;

  // Resolves the local capture directory without creating or claiming it.
  virtual Path PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
      const std::string& transaction_id, TransferMode mode = TransferMode::Staged,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const;

  // Rejects invalid checkpoint destinations before capture begins.
  virtual void ValidateCheckpointDestination(const StorageBackend& destination,
      const PublishedArtifact* artifact = nullptr, Cancellation control = {}) const;

  // Builds the checkpoint plan before the caller reserves staging capacity.
  virtual RestorePlan InspectCheckpoint(const Path& source, const PublishedArtifact* artifact = nullptr,
      Cancellation control = {}) const;

  // Publishes a checkpoint after admission, rejecting destination conflicts.
  virtual void PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
      CheckpointPublication* publication = nullptr, Cancellation control = {},
      TransferMode mode = TransferMode::Staged) const;

  // Resolves a logical target into a publication descriptor, if artifacts are supported.
  virtual PublishedArtifact ResolveArtifactTarget(const ArtifactTarget& target) const;

  // Validates a publication descriptor, or rejects unsupported artifacts.
  virtual void ValidateArtifact(const PublishedArtifact& artifact) const;
};
}  // namespace snapshot::pagebroker
