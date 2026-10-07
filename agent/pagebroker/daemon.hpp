// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstddef>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include "gpu/engine.hpp"

enum class ExitCode { SUCCESS = 0, FAILURE = 1, INVALID_ARGUMENTS = 2 };

inline constexpr size_t kDefaultMaxConcurrentRequests = 16;

struct DaemonOptions {
  std::filesystem::path socket_path;
  std::filesystem::path staging_directory;
  std::filesystem::path storage_root;
  std::filesystem::path storage_config_path;
  // Empty means artifact-addressed requests must fail INVALID_REQUEST.
  std::string store_id;
  size_t max_concurrent_requests = kDefaultMaxConcurrentRequests;
  snapshot::pagebroker::gpu::EngineOptions gpu;
  bool enable_gpu = true;
};

DaemonOptions ParseDaemonOptions(std::span<const std::string_view> arguments);
ExitCode RunDaemon(const DaemonOptions& options);
