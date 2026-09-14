// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <functional>
#include <mutex>
#include <string>
#include <vector>

#include "file_descriptor.hpp"
#include "direct_restore_source.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
struct DirectRestoreStage {
  FileDescriptor source_directory{-1};
  DirectRestoreProcesses processes;
  uintmax_t copied_bytes = 0;
  uintmax_t retained_bytes = 0;
};

struct DirectRestorePlan {
  FileDescriptor source_directory{-1};
  DirectRestoreProcesses processes;
  Path source_display;
  std::vector<uint32_t> cuda_namespace_pids;
  uintmax_t copied_bytes = 0;
  uintmax_t retained_bytes = 0;
};

class PosixCopyEngine final : public TransferEngine {
 public:
  explicit PosixCopyEngine(Path storage_root);
  PosixCopyEngine(
      Path storage_root,
      std::string reference_owner,
      std::function<int(const std::string&)>
          operation_failure_for_testing = {});
  PosixCopyEngine(Path storage_root,
                  std::function<void()> source_opened_for_testing,
                  std::function<void()> checkpoint_partial_opened_for_testing = {},
                  std::function<int(const std::string&)>
                      operation_failure_for_testing = {});
  TransferEngineType type() const override;
  uintmax_t RestoreSize(const StorageBackend& source) const override;
  uintmax_t StageRestore(
      const StorageBackend& source, const Path& destination,
      uintmax_t max_bytes) const override;
  Path ReferenceRegularRestore(const StorageBackend& source,
                               const std::string& transaction_id) const;
  DirectRestorePlan PrepareDirectRestore(
      const StorageBackend& source,
      const std::vector<uint32_t>& cuda_namespace_pids,
      size_t max_retained_descriptors) const;
  size_t DirectRestoreDescriptorCount(
      const StorageBackend& source,
      const std::vector<uint32_t>& cuda_namespace_pids) const;
  DirectRestoreStage StageDirectRestore(
      DirectRestorePlan plan,
      const Path& destination) const;
  void ValidateCheckpointDestination(const StorageBackend& destination) const override;
  bool CheckpointDestinationConflicts(const StorageBackend& destination) const override;
  void PublishCheckpoint(const Path& source, const StorageBackend& destination) const override;
  uintmax_t CopyDirectory(const Path& source, const Path& destination) const override;

 private:
  void RecoverCheckpointDestination(int parent_fd,
                                    const Path& relative) const;
  void MaybeFailOperation(const char* operation) const;
  void RenameChecked(int parent_fd,
                     const std::string& source,
                     const std::string& destination,
                     const Path& source_display,
                     const Path& destination_display,
                     const char* operation) const;
  void RemoveChecked(int parent_fd,
                     const std::string& leaf,
                     const Path& display,
                     const char* operation) const;
  void SyncParentChecked(int parent_fd,
                         const Path& display,
                         const char* operation) const;

  Path storage_root_;
  FileDescriptor storage_root_fd_;
  Path reference_root_;
  FileDescriptor reference_root_fd_{-1};
  FileDescriptor reference_owner_lock_fd_{-1};
  std::function<void()> source_opened_for_testing_;
  std::function<void()> checkpoint_partial_opened_for_testing_;
  std::function<int(const std::string&)> operation_failure_for_testing_;
  mutable std::mutex checkpoint_publication_mutex_;
};
}  // namespace snapshot::pagebroker
