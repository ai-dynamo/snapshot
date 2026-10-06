// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon_options.hpp"

#include <charconv>
#include <string_view>

namespace {
bool
HasRequiredPaths(const DaemonOptions& options)
{
  if (options.socket_path.empty() || options.staging_directory.empty())
    return false;
  return !options.storage_root.empty();
}

bool
ParsePositiveCount(std::string_view value, std::size_t& result)
{
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), result);
  if (error != std::errc{} || end != value.data() + value.size())
    return false;
  return result > 0;
}
}  // namespace

std::optional<DaemonOptions>
ParseDaemonArguments(int argc, const char* const* argv)
{
  if (argc < 6 || (argc - 4) % 2 != 0)
    return std::nullopt;
  DaemonOptions options{argv[1], argv[2], argv[3], 0, {}, {}};
  if (!HasRequiredPaths(options))
    return std::nullopt;
  for (int i = 4; i < argc; i += 2) {
    const std::string_view name(argv[i]);
    const std::string_view value(argv[i + 1]);
    if (value.empty())
      return std::nullopt;
    if (name == "--max-concurrent-requests" && options.max_concurrent_requests == 0) {
      if (!ParsePositiveCount(value, options.max_concurrent_requests))
        return std::nullopt;
    } else if (name == "--storage-config" && options.storage_config.empty()) {
      options.storage_config = value;
    } else if (name == "--model-streamer-config" && options.model_streamer_config.empty()) {
      options.model_streamer_config = value;
    } else {
      return std::nullopt;
    }
  }
  if (options.max_concurrent_requests == 0)
    return std::nullopt;
  return options;
}
