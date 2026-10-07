// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint_transaction_descriptor.hpp"

#include <stdexcept>
#include <utility>

namespace snapshot::pagebroker {
CheckpointTransactionDescriptor::CheckpointTransactionDescriptor(
    Path staging_directory, StorageBackend destination_storage, IoEngine engine_type, CheckpointOutput output)
    : staging_directory_(std::move(staging_directory)), destination_storage_(std::move(destination_storage)),
      engine_type_(engine_type), output_(output)
{
}

CheckpointTransactionDescriptor::CheckpointTransactionDescriptor(Path staging_directory, ArtifactTarget target)
    : staging_directory_(std::move(staging_directory)), target_(std::move(target))
{
}

const Path&
CheckpointTransactionDescriptor::staging_directory() const
{
  return staging_directory_;
}

bool
CheckpointTransactionDescriptor::is_artifact_addressed() const
{
  return target_.has_value();
}

const StorageBackend&
CheckpointTransactionDescriptor::destination_storage() const
{
  if (target_)
    throw std::logic_error("destination_storage() is not valid for an artifact-addressed checkpoint");
  return destination_storage_;
}

IoEngine
CheckpointTransactionDescriptor::engine_type() const
{
  if (target_)
    throw std::logic_error("engine_type() is not valid for an artifact-addressed checkpoint");
  return engine_type_;
}

const ArtifactTarget&
CheckpointTransactionDescriptor::target() const
{
  if (!target_)
    throw std::logic_error("target() is only valid for an artifact-addressed checkpoint");
  return *target_;
}
}  // namespace snapshot::pagebroker
