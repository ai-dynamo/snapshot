// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <string_view>

#include "transfer/s3/s3_client.hpp"

namespace snapshot::pagebroker {
// Shares connection settings between SDK uploads and native Model Streamer
// restores while keeping their resource limits and timeouts separate.
struct S3TransferOptions {
  S3Connection connection;
  S3Limits upload_limits;
  // Bounds each native restore submission, including its response wait.
  std::chrono::milliseconds restore_timeout = std::chrono::hours(2);
};

// Resolved startup settings for one store. Credentials are supplied externally
// through standard AWS providers and snapshotted once by the daemon.
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

// Parses bounded startup JSON and validates fields, locations and limits.
// Credential fields are rejected; external credential resolution is separate.
S3Config ParseS3Config(std::string_view text);
// Reads at most the configuration size limit plus one byte, then parses it so
// oversized files are rejected without loading their full contents.
S3Config ReadS3Config(const std::filesystem::path& path);
}  // namespace snapshot::pagebroker
