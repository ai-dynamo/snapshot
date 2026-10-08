// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "pagebroker_types.hpp"
#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
enum class CheckpointOutput { Staged, Direct };
class CheckpointTransactionDescriptor {
 public:
  CheckpointTransactionDescriptor(
      Path staging_directory, StorageBackend destination_storage, IoEngine engine_type,
      CheckpointOutput output);

  const Path& staging_directory() const;
  const StorageBackend& destination_storage() const;
  IoEngine engine_type() const;
  CheckpointOutput output() const { return output_; }

 private:
  Path staging_directory_;
  StorageBackend destination_storage_;
  IoEngine engine_type_;
  CheckpointOutput output_;
};
}  // namespace snapshot::pagebroker
