// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <memory>
#include <mutex>

#include "model_streamer_restore.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker {
// Stages prepared S3 read inventories. Artifact preparation and publication are supplied
// by the S3 storage integration; unsupported operations use the base contract.
class ModelStreamerTransferEngine final : public TransferEngine {
 public:
  explicit ModelStreamerTransferEngine(ModelStreamerSessionOptions options = {},
      std::chrono::milliseconds restore_timeout = kDefaultModelStreamerSubmissionTimeout);

  IoEngine type() const override;

  // Streams S3 objects into transaction-owned local staging.
  RestoreAccess StageRestore(PreparedRestore preparation, const Path& destination,
      Cancellation cancellation = {}) const override;

 private:
  std::shared_ptr<ModelStreamerRestore> CreateRestore() const;
  std::shared_ptr<ModelStreamerRestore> AcquireRestore() const;
  const ModelStreamerSessionOptions options_;
  const std::chrono::milliseconds restore_timeout_;
  mutable std::mutex restore_mutex_;
  mutable std::shared_ptr<ModelStreamerRestore> restore_;
};
}  // namespace snapshot::pagebroker
