// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"
#include "transfer/s3/s3_client.hpp"

namespace snapshot::pagebroker {
// Process environment contract shared by native setup and engine validation.
inline constexpr char kS3VirtualAddressingEnv[] = "RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING";
inline constexpr char kS3VirtualAddressingEnabled[] = "1";
inline constexpr char kS3VirtualAddressingDisabled[] = "0";

// Shares connection settings between SDK uploads and native Model Streamer
// restores while keeping their resource limits and timeouts separate.
struct S3TransferOptions {
  void Validate() const;

  S3Connection connection;
  S3Limits upload_limits;
  // Bounds each native restore submission, including its response wait.
  std::chrono::milliseconds restore_timeout = kDefaultModelStreamerSubmissionTimeout;
};

// Checks upload/restore agreement with the Model Streamer process environment.
void ValidateModelStreamerEnvironment(const S3TransferOptions& options);

// Typed settings for one store, supplied by the caller before constructing
// the broker or transfer engine.
struct S3Config {
  S3TransferOptions transfer;
  // Logical store identity and the bucket/key prefix used for its checkpoints.
  std::string store_id;
  std::string bucket;
  std::string prefix;
  // Broker admission and lifetime policy, separate from upload buffer limits.
  std::chrono::seconds transaction_lifetime = std::chrono::hours(2);
  std::uint64_t staging_bytes = 1ULL << 40;
  unsigned active_transactions = 8;
};

}  // namespace snapshot::pagebroker
