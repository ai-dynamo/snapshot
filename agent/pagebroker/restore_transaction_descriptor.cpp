// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "restore_transaction_descriptor.hpp"

#include <algorithm>
#include <utility>

namespace snapshot::pagebroker {
RestoreTransactionDescriptor::RestoreTransactionDescriptor(
    Path staging_directory, Kind kind)
    : staging_directory_(std::move(staging_directory)), kind_(kind)
{
}

RestoreTransactionDescriptor::RestoreTransactionDescriptor(
    Path staging_directory,
    FileDescriptor direct_source_directory,
    DirectRestoreProcesses direct_processes,
    uintmax_t retained_carrier_bytes)
    : staging_directory_(std::move(staging_directory)),
      kind_(Kind::DIRECT_CUSTOM_STORAGE),
      direct_source_directory_(std::move(direct_source_directory)),
      direct_processes_(std::move(direct_processes)),
      retained_carrier_bytes_(retained_carrier_bytes)
{
}

RestoreTransactionDescriptor::Kind
RestoreTransactionDescriptor::kind() const
{
  return kind_;
}

bool
RestoreTransactionDescriptor::supports_backend(CudaStorageBackend backend) const
{
  if (kind_ == Kind::DIRECT_CUSTOM_STORAGE)
    return backend == v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE;
  return backend == v1::CUDA_STORAGE_BACKEND_REGULAR;
}

const Path&
RestoreTransactionDescriptor::staging_directory() const
{
  return staging_directory_;
}

bool
RestoreTransactionDescriptor::direct_carriers() const
{
  return direct_source_directory_.has_value();
}

const DirectRestoreProcesses&
RestoreTransactionDescriptor::direct_processes() const
{
  return direct_processes_;
}

bool
RestoreTransactionDescriptor::has_exact_cuda_namespace_pids(
    const CudaRestoreRequest& request) const
{
  if (static_cast<size_t>(request.targets_size()) != direct_processes_.size())
    return false;
  std::vector<uint32_t> requested;
  requested.reserve(request.targets_size());
  for (const auto& target : request.targets())
    requested.push_back(target.namespace_pid());
  std::sort(requested.begin(), requested.end());
  std::vector<uint32_t> admitted;
  admitted.reserve(direct_processes_.size());
  for (const auto& process : direct_processes_)
    admitted.push_back(process.namespace_pid);
  std::sort(admitted.begin(), admitted.end());
  return requested == admitted;
}

uintmax_t
RestoreTransactionDescriptor::retained_carrier_bytes() const
{
  return retained_carrier_bytes_;
}

const std::string&
RestoreTransactionDescriptor::staging_request_id() const
{
  return staging_request_id_;
}

void
RestoreTransactionDescriptor::set_staging_request_id(std::string request_id)
{
  staging_request_id_ = std::move(request_id);
}

const std::optional<RestoreIdentity>&
RestoreTransactionDescriptor::restore_identity() const
{
  return restore_identity_;
}

void
RestoreTransactionDescriptor::set_restore_identity(
    RestoreIdentity identity)
{
  restore_identity_ = std::move(identity);
}

const std::optional<StageReadyHandle>&
RestoreTransactionDescriptor::stage_ready_handle() const
{
  return stage_ready_handle_;
}

void
RestoreTransactionDescriptor::set_stage_ready_handle(StageReadyHandle handle)
{
  stage_ready_handle_ = std::move(handle);
}
}  // namespace snapshot::pagebroker
