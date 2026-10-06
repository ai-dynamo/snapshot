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
}  // namespace

int
main(int argc, char** argv)
{
  size_t max_concurrent_requests = 16;
  snapshot::pagebroker::gpu::EngineOptions gpu_options;
  bool enable_gpu = true;
  bool valid = argc >= 4;
  for (int index = 4; valid && index < argc; ++index) {
    const std::string_view option(argv[index]);
    if (option == "--disable-gpu") { enable_gpu = false; continue; }
    if (++index >= argc) { valid = false; break; }
    size_t value = 0;
    const std::string_view input(argv[index]);
    const auto [end, error] = std::from_chars(input.data(), input.data() + input.size(), value);
    valid = error == std::errc{} && end == input.data() + input.size();
    if (!valid) break;
    if (option == "--max-concurrent-requests") valid = ParseMaxConcurrentRequests(input, max_concurrent_requests);
    else if (option == "--gpu-buffer-count") { gpu_options.buffer_count = value; valid = value > 0; }
    else if (option == "--gpu-chunk-bytes") { gpu_options.chunk_bytes = value; valid = value > 0; }
    else if (option == "--gpu-max-pinned-bytes") gpu_options.max_pinned_bytes = value;
    else valid = false;
  }
  if (!valid) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root "
                 "[--max-concurrent-requests count] [--gpu-buffer-count count] "
                 "[--gpu-chunk-bytes bytes] [--gpu-max-pinned-bytes bytes] [--disable-gpu]\n";
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  return static_cast<int>(RunDaemon(argv[1], argv[2], argv[3], max_concurrent_requests, gpu_options, enable_gpu));
}
