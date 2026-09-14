/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <chrono>
#include <cstdint>
#include <functional>
#include <stop_token>
#include <string>
#include <vector>

#include "daemon_protocol.h"

namespace cuda_checkpoint_operation {
class Service;
}

namespace cuda_checkpoint_server {

inline constexpr uint64_t kMaximumOperationSeconds = 24 * 60 * 60;

struct DaemonOptions {
  std::string socket_path;
  std::string private_socket_directory = "/run/cuda-checkpoint-helper";
  std::string process_root = "/host/proc";
  std::string storage_root = "/checkpoints";
  uint64_t max_operation_seconds = 60 * 60;
};

bool ValidateDaemonOptions(const DaemonOptions &options, std::string *error);
// Receive one private worker request plus its optional SCM_RIGHTS carrier set.
// The caller owns every returned descriptor and must keep them open through
// request execution, then close them on every path.
bool ReceiveWorkerRequest(int socket_fd, std::vector<unsigned char> *packet,
                          cuda_checkpoint_daemon::Request *request,
                          std::vector<int> *descriptors,
                          std::string *error,
                          const std::string &storage_root);
// A worker's signal-owner thread can observe SIGTERM while its operation
// thread remains blocked in the CUDA driver. Cancel and identity-terminate
// restored targets immediately, then exit the isolated worker after a bounded
// grace period only when active and retained target termination is conclusive.
void RunShutdownWatchdog(
    std::stop_token stop,
    cuda_checkpoint_daemon::ShutdownSignalOwner *signal_owner,
    cuda_checkpoint_operation::Service *operation_service,
    const std::string &process_root, std::chrono::milliseconds grace_period,
    const std::function<void()> &safe_exit);
int RunDaemon(const DaemonOptions &options);
int RunDaemon(const std::string &socket_path, uint64_t max_operation_seconds);
int RunHealthClient(const std::string &socket_path);

} // namespace cuda_checkpoint_server
