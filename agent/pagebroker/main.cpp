// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <iostream>
#include <stdexcept>
#include <string_view>
#include <vector>

#include "daemon.hpp"

int
main(int argc, char** argv)
{
  DaemonOptions options;
  try {
    const std::vector<std::string_view> arguments(argv + 1, argv + argc);
    options = ParseDaemonOptions(arguments);
  } catch (const std::invalid_argument& error) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root "
                 "[--max-concurrent-requests count] [--custom-storage-engine on|off] "
                 "[--custom-storage-buffer-count count] [--custom-storage-chunk-bytes bytes] "
                 "[--custom-storage-max-pinned-bytes bytes] "
                 "[--storage-config path] [--model-streamer-config path]\n"
              << "pagebroker: " << error.what() << '\n';
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  try {
    return static_cast<int>(RunDaemon(options));
  } catch (const std::exception&) {
    std::cerr << "PageBroker startup failed: invalid configuration or unavailable local resources\n";
    return static_cast<int>(ExitCode::FAILURE);
  }
}
