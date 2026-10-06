// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <iostream>

#include "daemon.hpp"
#include "daemon_options.hpp"

int
main(int argc, char** argv)
{
  const auto options = ParseDaemonArguments(argc, argv);
  if (!options) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root --max-concurrent-requests max_concurrent_requests [--storage-config path] [--model-streamer-config path]\n";
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
  try {
    return static_cast<int>(RunDaemon(options->socket_path, options->staging_directory, options->storage_root,
        options->max_concurrent_requests, options->storage_config, options->model_streamer_config));
  }
  catch (const std::exception&) {
    std::cerr << "PageBroker startup failed: invalid configuration or unavailable local resources\n";
    return static_cast<int>(ExitCode::FAILURE);
  }
}
