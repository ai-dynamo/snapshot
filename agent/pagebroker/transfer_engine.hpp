// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <cstddef>

#include "pagebroker_types.hpp"

namespace snapshot::pagebroker {
using Path = std::filesystem::path;

enum class IoEngine { POSIX_COPY, NIXL };

namespace io {
enum class Operation { Read, Write };
}

class TransferEngine {
 public:
  virtual ~TransferEngine();
  virtual IoEngine type() const = 0;
  // POSIX_COPY supports directory staging. NIXL supports registered-buffer I/O.
  // Unsupported operations fail explicitly until directory staging uses NIXL.
  virtual void Open(int descriptor, size_t size);
  virtual void Submit(size_t slot, io::Operation operation, size_t offset, size_t size);
  virtual void Wait(size_t slot);
  virtual void Close();
  virtual Path DestinationDirectory(const StorageBackend& destination) const;
  virtual Path SourceDirectory(const StorageBackend& source) const;
  // CPU restore staging excludes the separate GPU payload directory.
  virtual uintmax_t RestoreSize(const StorageBackend& source) const;
  virtual void StageRestore(const StorageBackend& source, const Path& destination) const;
  virtual void ValidateCheckpointDestination(const StorageBackend& destination) const;
  virtual bool CheckpointDestinationConflicts(const StorageBackend& destination) const;
  virtual void PublishCheckpoint(const Path& source, const StorageBackend& destination) const;
  virtual void PromoteCheckpoint(const Path& output, const StorageBackend& destination) const;
  virtual void CopyDirectory(const Path& source, const Path& destination) const;
};
}  // namespace snapshot::pagebroker
