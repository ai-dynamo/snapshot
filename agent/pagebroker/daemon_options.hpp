// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstddef>
#include <filesystem>
#include <optional>

struct DaemonOptions {
  std::filesystem::path socket_path;
  std::filesystem::path staging_directory;
  std::filesystem::path storage_root;
  std::size_t max_concurrent_requests = 0;
  std::filesystem::path storage_config;
  std::filesystem::path model_streamer_config;
};

// Three positional paths followed by unique option/value pairs, in any order.
std::optional<DaemonOptions> ParseDaemonArguments(int argc, const char* const* argv);
