// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <charconv>
#include <iostream>
#include <string_view>

#include "daemon.hpp"

namespace {
bool
ParseMaxConcurrentRequests(std::string_view value, size_t& max_concurrent_requests)
{
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), max_concurrent_requests);
  return error == std::errc{} && end == value.data() + value.size() && max_concurrent_requests > 0;
}

bool
ParseArguments(int argc, char** argv, size_t& max_concurrent_requests)
{
  if (argc != 6 && argc != 8)
    return false;
  if (argc == 8 && std::string_view(argv[6]) != "--storage-config")
    return false;
  if (std::string_view(argv[4]) != "--max-concurrent-requests")
    return false;
  return ParseMaxConcurrentRequests(argv[5], max_concurrent_requests);
}
}  // namespace

int
main(int argc, char** argv)
{
  size_t max_concurrent_requests;
  if (!ParseArguments(argc, argv, max_concurrent_requests)) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests [--storage-config path]\n";
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  try {
    return static_cast<int>(RunDaemon(argv[1], argv[2], argv[3], max_concurrent_requests, argc == 8 ? argv[7] : ""));
  }
  catch (const std::exception&) {
    std::cerr << "PageBroker startup failed: invalid configuration or unavailable local resources\n";
    return static_cast<int>(ExitCode::FAILURE);
  }
}
