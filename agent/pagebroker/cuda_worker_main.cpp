// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "daemon_server.h"

#include <charconv>
#include <cstdint>
#include <iostream>
#include <string_view>

namespace {
constexpr std::string_view kUsage =
    "usage: pagebroker-cuda-worker --socket PATH --socket-directory DIR "
    "--proc-root DIR --storage-root DIR --max-operation-seconds SECONDS";

bool ParseSeconds(std::string_view value, uint64_t *seconds) {
  const auto [end, error] =
      std::from_chars(value.data(), value.data() + value.size(), *seconds);
  return error == std::errc{} && end == value.data() + value.size() &&
         *seconds > 0 &&
         *seconds <= cuda_checkpoint_server::kMaximumOperationSeconds;
}
} // namespace

int main(int argc, char **argv) {
  cuda_checkpoint_server::DaemonOptions options;
  bool have_socket = false;
  bool have_socket_directory = false;
  bool have_proc_root = false;
  bool have_storage_root = false;
  bool have_timeout = false;
  bool invalid = false;
  for (int index = 1; !invalid && index < argc; ++index) {
    const std::string_view flag(argv[index]);
    if (flag == "--socket" && !have_socket && ++index < argc) {
      options.socket_path = argv[index];
      have_socket = true;
    } else if (flag == "--socket-directory" && !have_socket_directory &&
               ++index < argc) {
      options.private_socket_directory = argv[index];
      have_socket_directory = true;
    } else if (flag == "--proc-root" && !have_proc_root && ++index < argc) {
      options.process_root = argv[index];
      have_proc_root = true;
    } else if (flag == "--storage-root" && !have_storage_root &&
               ++index < argc) {
      options.storage_root = argv[index];
      have_storage_root = true;
    } else if (flag == "--max-operation-seconds" && !have_timeout &&
               ++index < argc &&
               ParseSeconds(argv[index], &options.max_operation_seconds)) {
      have_timeout = true;
    } else {
      invalid = true;
    }
  }
  if (invalid || !have_socket || !have_socket_directory || !have_proc_root ||
      !have_storage_root || !have_timeout) {
    std::cerr << kUsage << '\n';
    return 2;
  }
  return cuda_checkpoint_server::RunDaemon(options);
}
