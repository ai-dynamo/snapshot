// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <fstream>
#include <iostream>
#include <iterator>
#include <stdexcept>
#include <string>
#include <string_view>
#include <vector>

#include "daemon.hpp"
#include "identity.hpp"
#include "storage_config.hpp"

namespace {
// ResolveStoreID reads and parses storage_config_path, then computes this
// installation's store ID. Only type: pvc is supported; other types resolve
// to an unconfigured store (empty), same as omitting --storage-config,
// since no artifact-addressed backend exists for them yet.
std::string
ResolveStoreID(const std::string& storage_config_path)
{
  std::ifstream file(storage_config_path);
  if (!file.is_open()) {
    std::cerr << "open storage config " << storage_config_path << " failed\n";
    return "";
  }
  const std::string yaml((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());

  snapshot::pagebroker::StorageConfig config;
  try {
    config = snapshot::pagebroker::ParseStorageConfig(yaml);
  }
  catch (const std::exception& error) {
    std::cerr << "parse storage config " << storage_config_path << ": " << error.what() << '\n';
    return "";
  }
  if (config.type != "pvc") {
    std::cerr << "storage config type " << config.type << " has no artifact-addressed backend yet\n";
    return "";
  }
  try {
    return snapshot::pagebroker::ComputeStoreID(config.pvc_namespace, config.pvc_claim_name, config.pvc_base_path);
  }
  catch (const std::exception& error) {
    std::cerr << "compute store ID from " << storage_config_path << ": " << error.what() << '\n';
    return "";
  }
}
}  // namespace

int
main(int argc, char** argv)
{
  try {
    const std::vector<std::string_view> arguments(argv + 1, argv + argc);
    auto options = ParseDaemonOptions(arguments);
    if (!options.storage_config_path.empty()) {
      options.store_id = ResolveStoreID(options.storage_config_path.string());
    }
    return static_cast<int>(RunDaemon(options));
  } catch (const std::invalid_argument& error) {
    std::cerr << "usage: pagebroker socket_path staging_directory storage_root "
                 "[--max-concurrent-requests count] [--custom-storage-engine on|off] "
                 "[--custom-storage-buffer-count count] [--custom-storage-chunk-bytes bytes] "
                 "[--custom-storage-max-pinned-bytes bytes] [--storage-config path]\n"
              << "pagebroker: " << error.what() << '\n';
    return static_cast<int>(ExitCode::INVALID_ARGUMENTS);
  }
}
