// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <future>
#include <system_error>
#include <vector>

#include "v1/pagebroker.pb.h"

namespace snapshot::pagebroker {
class Broker;
}

enum class ExitCode { SUCCESS = 0, FAILURE = 1, INVALID_ARGUMENTS = 2 };

const char* RequestCommandName(snapshot::pagebroker::v1::Request::CommandCase command);
const char* ResponseResultName(
    snapshot::pagebroker::v1::Request::CommandCase command,
    const snapshot::pagebroker::v1::Response& response);

// Re-drive identity-safe CUDA shutdown before joining request handlers. A
// blocked restore handler may only unwind after a later identity check proves
// its target absent and the worker receives SIGTERM.
void ShutdownAndWaitForHandlers(
    snapshot::pagebroker::Broker& broker,
    std::vector<std::future<void>>& handlers,
    const std::filesystem::path& readiness_socket_path = {});

class DaemonMaintenanceSchedule {
 public:
  using Clock = std::chrono::steady_clock;

  explicit DaemonMaintenanceSchedule(Clock::time_point now)
      : next_transaction_reap_(now), next_cuda_reap_(now) {}

  bool TransactionsDue(Clock::time_point now) {
    return Due(now, std::chrono::minutes(2), &next_transaction_reap_);
  }

  bool CudaDue(Clock::time_point now) {
    return Due(now, std::chrono::seconds(1), &next_cuda_reap_);
  }

 private:
  static bool Due(Clock::time_point now, Clock::duration interval,
                  Clock::time_point* next) {
    if (now < *next)
      return false;
    *next = now + interval;
    return true;
  }

  Clock::time_point next_transaction_reap_;
  Clock::time_point next_cuda_reap_;
};

ExitCode RunDaemon(
    const std::filesystem::path& socket_path,
    const std::filesystem::path& staging_directory,
    const std::filesystem::path& storage_root,
    size_t max_concurrent_requests,
    uintmax_t max_staging_bytes,
    bool cuda,
    bool cuda_custom_storage,
    size_t max_concurrent_cuda_restores,
    uint64_t cuda_operation_timeout_seconds,
    size_t cuda_worker_count,
    size_t cuda_custom_storage_worker_count);

// Kubernetes probes use this process-local contract rather than extending the
// public PageBroker transaction protocol. The daemon publishes the marker only
// after CUDA initialization and worker prewarm complete; the probe also opens
// the live Unix listener so a stale marker cannot report Ready after a crash.
std::filesystem::path DaemonReadinessPath(
    const std::filesystem::path& socket_path);
bool PublishDaemonReadiness(const std::filesystem::path& socket_path,
                            std::error_code* error);
void WithdrawDaemonReadiness(const std::filesystem::path& socket_path);
ExitCode ProbeDaemonReady(const std::filesystem::path& socket_path);
