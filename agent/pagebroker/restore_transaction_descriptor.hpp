// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <optional>
#include <vector>

#include "file_descriptor.hpp"
#include "direct_restore_source.hpp"
#include "stage_ready_gate.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
class RestoreTransactionDescriptor {
 public:
  enum class Kind {
    STAGED_COPY,
    REGULAR_REFERENCE,
    DIRECT_CUSTOM_STORAGE,
  };

  explicit RestoreTransactionDescriptor(
      Path staging_directory, Kind kind = Kind::STAGED_COPY);
  RestoreTransactionDescriptor(
      Path staging_directory,
      FileDescriptor direct_source_directory,
      DirectRestoreProcesses direct_processes,
      uintmax_t retained_carrier_bytes);

  const Path& staging_directory() const;
  Kind kind() const;
  bool supports_backend(CudaStorageBackend backend) const;
  bool direct_carriers() const;
  const DirectRestoreProcesses& direct_processes() const;
  bool has_exact_cuda_namespace_pids(
      const CudaRestoreRequest& request) const;
  uintmax_t retained_carrier_bytes() const;
  const std::string& staging_request_id() const;
  void set_staging_request_id(std::string request_id);
  const std::optional<RestoreIdentity>& restore_identity() const;
  void set_restore_identity(RestoreIdentity identity);
  const std::optional<StageReadyHandle>& stage_ready_handle() const;
  void set_stage_ready_handle(StageReadyHandle handle);

 private:
  Path staging_directory_;
  Kind kind_;
  std::optional<FileDescriptor> direct_source_directory_;
  DirectRestoreProcesses direct_processes_;
  uintmax_t retained_carrier_bytes_ = 0;
  std::string staging_request_id_;
  std::optional<RestoreIdentity> restore_identity_;
  std::optional<StageReadyHandle> stage_ready_handle_;
};
}  // namespace snapshot::pagebroker
