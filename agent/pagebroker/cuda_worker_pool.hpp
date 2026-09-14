// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <optional>
#include <string>
#include <stop_token>
#include <vector>

#include <sys/types.h>

#include "cuda_worker_client.hpp"

namespace snapshot::pagebroker {

struct CudaWorkerPoolConfig {
  size_t worker_count = 8;
  std::string worker_binary = "/usr/local/bin/pagebroker-cuda-worker";
  std::string private_socket_directory = "/run/pagebroker/cuda-workers";
  std::string process_root = "/host/proc";
  std::string storage_root = "/checkpoints";
  uint64_t max_operation_seconds = 30 * 60;
  std::chrono::milliseconds rpc_timeout = std::chrono::minutes(30);
  std::chrono::milliseconds health_rpc_timeout = std::chrono::seconds(1);
  std::chrono::milliseconds startup_timeout = std::chrono::seconds(30);
  std::chrono::milliseconds health_interval = std::chrono::seconds(5);
  bool require_custom_storage = false;
};

struct CudaWorkerHandle {
  size_t index = 0;
  pid_t pid = -1;
  std::shared_ptr<CudaWorkerClient> client;
};

struct CudaWorkerSnapshot {
  size_t index = 0;
  pid_t pid = -1;
  bool alive = false;
  bool healthy = false;
  bool leased = false;
  bool unknown_outcome = false;
  bool health_checking = false;
  std::string incarnation;
};

class CudaWorkerPool {
  struct State;

public:
  class Lease {
  public:
    Lease() = default;
    Lease(const Lease &) = delete;
    Lease &operator=(const Lease &) = delete;
    Lease(Lease &&other) noexcept;
    Lease &operator=(Lease &&other) noexcept;
    ~Lease();

    const std::vector<CudaWorkerHandle> &workers() const { return workers_; }
    explicit operator bool() const { return state_ != nullptr; }
    CudaWorkerRpcResult
    Call(size_t worker_index,
         const cuda_checkpoint_daemon::Request &request) const;
    void Poison(size_t worker_index);
    std::shared_ptr<void> Retain(size_t worker_index);
    void Release();

  private:
    friend class CudaWorkerPool;
    Lease(std::shared_ptr<State> state,
          std::vector<CudaWorkerHandle> workers);

    std::shared_ptr<State> state_;
    std::vector<CudaWorkerHandle> workers_;
  };

  explicit CudaWorkerPool(CudaWorkerPoolConfig config);
  CudaWorkerPool(const CudaWorkerPool &) = delete;
  CudaWorkerPool &operator=(const CudaWorkerPool &) = delete;
  ~CudaWorkerPool();

  // Must be called during single-threaded PageBroker startup, before any
  // worker is spawned. The monitor consumes SIGCHLD and reaps only PIDs owned
  // by this pool.
  static bool BlockChildExitSignal(std::string *error);

  bool Start(std::string *error);
  std::optional<Lease> TryAcquire(size_t weight);
  bool Shutdown(std::chrono::milliseconds timeout, std::string *error);

  bool ready() const;
  bool fail_stop_required() const;
  std::vector<CudaWorkerSnapshot> Snapshot() const;

private:
  void MonitorChildren(std::stop_token stop);
  void ReapExitedChildren();
  void CheckIdleWorkerHealth();

  CudaWorkerPoolConfig config_;
  std::shared_ptr<State> state_;
  class MonitorThread;
  std::unique_ptr<MonitorThread> monitor_;
};

} // namespace snapshot::pagebroker
