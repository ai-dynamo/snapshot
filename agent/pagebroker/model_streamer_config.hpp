// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <optional>
#include <string>
#include <string_view>

#include "s3_config.hpp"

namespace snapshot::pagebroker::model_streamer::settings {
// Optional values distinguish omission (inherit the startup environment or
// defaults) from an explicit zero. Native tuning is fixed for the process.
struct Options {
  std::optional<std::uint64_t> read_chunk_bytes;
  std::optional<std::uint64_t> process_group_size;
  std::optional<std::uint64_t> s3_concurrency;
  std::optional<std::uint64_t> s3_target_gbps;
  std::optional<std::uint64_t> s3_max_connections;
  std::optional<std::uint64_t> s3_max_inflight_mib;
  std::optional<std::uint64_t> s3_max_retries;
  std::optional<std::uint64_t> s3_retry_window_seconds;
  std::optional<std::uint64_t> s3_low_speed_timeout_ms;
  std::optional<std::uint64_t> s3_low_speed_bytes_per_second;
  std::optional<std::string> log_level;
  std::optional<bool> log_to_stderr;

  void Validate() const;
  // Only explicitly resolved settings; never synthesizes library defaults.
  std::map<std::string, std::string> NativeEnvironment() const;
  // Direct C++ callers must apply process settings before constructing engines.
  void ValidateEnvironment() const;
};

Options ParseConfig(std::string_view text);
Options ReadConfig(const std::filesystem::path& path);
// Pure with respect to the environment: resolve JSON > native environment >
// legacy S3 daemon defaults, then validate the complete result before mutation.
Options ResolveOptions(Options options, const S3Config* storage = nullptr);
// Startup only, before constructing Broker or any native workers. Shared S3
// connection settings remain authoritative in the existing storage config.
void ConfigureEnvironment(const Options& options, const S3Config* storage = nullptr);
}  // namespace snapshot::pagebroker::model_streamer::settings
