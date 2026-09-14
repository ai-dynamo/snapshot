// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "cuda_worker_pool.hpp"

#include <spawn.h>
#include <signal.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <condition_variable>
#include <cstring>
#include <filesystem>
#include <mutex>
#include <thread>
#include <utility>

extern char **environ;

namespace snapshot::pagebroker {
namespace {
namespace protocol = cuda_checkpoint_daemon;
namespace fs = std::filesystem;

constexpr size_t kMaximumWorkers = 128;
constexpr size_t kPersistentHealthFailureThreshold = 3;

bool HealthFailureIsTransportUncertainty(CudaWorkerRpcStatus status) {
  return status == CudaWorkerRpcStatus::kConnectFailed ||
         status == CudaWorkerRpcStatus::kTimeout ||
         status == CudaWorkerRpcStatus::kDisconnected;
}

std::string Incarnation(const std::string &health_output) {
  constexpr std::string_view prefix = "\"incarnation\":\"";
  const size_t start = health_output.find(prefix);
  if (start == std::string::npos)
    return {};
  const size_t value_start = start + prefix.size();
  const size_t end = health_output.find('"', value_start);
  if (end == std::string::npos || end == value_start)
    return {};
  return health_output.substr(value_start, end - value_start);
}

bool AbsoluteNormalized(const std::string &value) {
  const fs::path path(value);
  return path.is_absolute() && path.lexically_normal() == path;
}

} // namespace

struct CudaWorkerPool::State {
  struct Worker {
    size_t index = 0;
    pid_t pid = -1;
    std::string socket_path;
    std::shared_ptr<CudaWorkerClient> client;
    bool alive = true;
    bool healthy = false;
    bool leased = false;
    bool health_checking = false;
    bool unknown_outcome = false;
    size_t consecutive_health_failures = 0;
    size_t retained_context_owners = 0;
    std::string incarnation;
  };

  mutable std::mutex mutex;
  std::condition_variable changed;
  std::vector<Worker> workers;
  bool started = false;
  bool shutting_down = false;
  bool fail_stop_required = false;
};

class CudaWorkerPool::MonitorThread {
public:
  template <typename Callback>
  explicit MonitorThread(Callback callback) : thread_(std::move(callback)) {}
  ~MonitorThread() = default;
  void RequestStop() { thread_.request_stop(); }
  bool StopRequested() const { return thread_.get_stop_token().stop_requested(); }
  std::thread::native_handle_type NativeHandle() { return thread_.native_handle(); }
  void Join() {
    if (thread_.joinable())
      thread_.join();
  }

private:
  std::jthread thread_;
};

CudaWorkerPool::Lease::Lease(std::shared_ptr<State> state,
                             std::vector<CudaWorkerHandle> workers)
    : state_(std::move(state)), workers_(std::move(workers)) {}

CudaWorkerPool::Lease::Lease(Lease &&other) noexcept
    : state_(std::move(other.state_)), workers_(std::move(other.workers_)) {}

CudaWorkerPool::Lease &CudaWorkerPool::Lease::operator=(Lease &&other) noexcept {
  if (this != &other) {
    Release();
    state_ = std::move(other.state_);
    workers_ = std::move(other.workers_);
  }
  return *this;
}

CudaWorkerPool::Lease::~Lease() { Release(); }

CudaWorkerRpcResult CudaWorkerPool::Lease::Call(
    size_t worker_index, const protocol::Request &request) const {
  if (state_ == nullptr)
    return {.status = CudaWorkerRpcStatus::kInvalidRequest,
            .error = "CUDA worker lease is no longer active"};
  std::shared_ptr<CudaWorkerClient> client;
  {
    std::lock_guard lock(state_->mutex);
    if (worker_index >= workers_.size())
      return {.status = CudaWorkerRpcStatus::kInvalidRequest,
              .error = "CUDA worker lease index is out of range"};
    const auto &handle = workers_[worker_index];
    if (handle.index >= state_->workers.size() ||
        !state_->workers[handle.index].leased)
      return {.status = CudaWorkerRpcStatus::kInvalidRequest,
              .error = "CUDA worker lease is no longer active"};
    const auto &worker = state_->workers[handle.index];
    if (!state_->started || state_->shutting_down ||
        state_->fail_stop_required || !worker.alive || !worker.healthy) {
      return {.status = CudaWorkerRpcStatus::kPoolFailStopped,
              .error = "CUDA worker pool fail-stopped before dispatch"};
    }
    // This state-locked check is the dispatch linearization point: a monitor
    // that publishes fail-stop first prevents the send; a call that wins is an
    // in-flight operation even though its per-worker socket I/O occurs after
    // releasing the shared pool lock. The long rank RPCs therefore remain
    // parallel and never serialize on pool state.
    client = handle.client;
  }
  return client->Call(request);
}

void CudaWorkerPool::Lease::Poison(size_t worker_index) {
  if (state_ == nullptr || worker_index >= workers_.size())
    return;
  std::lock_guard lock(state_->mutex);
  auto &worker = state_->workers[workers_[worker_index].index];
  worker.unknown_outcome = true;
  worker.healthy = false;
  state_->fail_stop_required = true;
  state_->changed.notify_all();
}

std::shared_ptr<void> CudaWorkerPool::Lease::Retain(size_t worker_index) {
  if (state_ == nullptr || worker_index >= workers_.size())
    return nullptr;
  const size_t index = workers_[worker_index].index;
  {
    std::lock_guard lock(state_->mutex);
    ++state_->workers[index].retained_context_owners;
  }
  return std::shared_ptr<void>(
      state_.get(), [state = state_, index](void *) {
        std::lock_guard lock(state->mutex);
        auto &worker = state->workers[index];
        if (worker.retained_context_owners != 0)
          --worker.retained_context_owners;
        state->changed.notify_all();
      });
}

void CudaWorkerPool::Lease::Release() {
  if (state_ == nullptr)
    return;
  std::lock_guard lock(state_->mutex);
  for (const auto &handle : workers_) {
    if (handle.index < state_->workers.size())
      state_->workers[handle.index].leased = false;
  }
  state_->changed.notify_all();
  workers_.clear();
  state_.reset();
}

CudaWorkerPool::CudaWorkerPool(CudaWorkerPoolConfig config)
    : config_(std::move(config)), state_(std::make_shared<State>()) {}

CudaWorkerPool::~CudaWorkerPool() {
  std::string ignored;
  while (!Shutdown(std::chrono::seconds(1), &ignored)) {
    // A worker may intentionally remain alive while identity-safe retained
    // target termination is inconclusive. Never force-kill it and release its
    // CUDA contexts underneath a live restored workload.
  }
}

bool CudaWorkerPool::BlockChildExitSignal(std::string *error) {
  if (error == nullptr)
    return false;
  sigset_t child_signal;
  if (sigemptyset(&child_signal) != 0 ||
      sigaddset(&child_signal, SIGCHLD) != 0) {
    *error = std::string("prepare SIGCHLD mask: ") + std::strerror(errno);
    return false;
  }
  const int mask_error = pthread_sigmask(SIG_BLOCK, &child_signal, nullptr);
  if (mask_error != 0) {
    *error = std::string("block SIGCHLD: ") + std::strerror(mask_error);
    return false;
  }
  return true;
}

bool CudaWorkerPool::Start(std::string *error) {
  if (error == nullptr)
    return false;
  {
    std::lock_guard lock(state_->mutex);
    if (state_->started || state_->shutting_down) {
      *error = "CUDA worker pool is already started or shutting down";
      return false;
    }
  }
  if (config_.worker_count == 0 || config_.worker_count > kMaximumWorkers ||
      config_.max_operation_seconds == 0 ||
      config_.max_operation_seconds > 24 * 60 * 60 ||
      config_.rpc_timeout <= std::chrono::milliseconds::zero() ||
      config_.health_rpc_timeout <= std::chrono::milliseconds::zero() ||
      config_.startup_timeout <= std::chrono::milliseconds::zero() ||
      config_.health_interval <= std::chrono::milliseconds::zero() ||
      !AbsoluteNormalized(config_.worker_binary) ||
      !AbsoluteNormalized(config_.private_socket_directory) ||
      !AbsoluteNormalized(config_.process_root) ||
      !AbsoluteNormalized(config_.storage_root)) {
    *error = "invalid CUDA worker pool configuration";
    return false;
  }
  if (!BlockChildExitSignal(error))
    return false;

  std::error_code filesystem_error;
  fs::create_directories(config_.private_socket_directory, filesystem_error);
  if (filesystem_error ||
      chmod(config_.private_socket_directory.c_str(), 0700) != 0) {
    *error = "create private CUDA worker socket directory failed: " +
             std::string(filesystem_error ? filesystem_error.message()
                                          : std::strerror(errno));
    return false;
  }

  for (size_t index = 0; index < config_.worker_count; ++index) {
    const std::string socket_path =
        (fs::path(config_.private_socket_directory) /
         ("worker-" + std::to_string(index) + ".sock"))
            .string();
    const std::string timeout = std::to_string(config_.max_operation_seconds);
    std::vector<std::string> arguments{
        config_.worker_binary,
        "--socket",
        socket_path,
        "--socket-directory",
        config_.private_socket_directory,
        "--proc-root",
        config_.process_root,
        "--storage-root",
        config_.storage_root,
        "--max-operation-seconds",
        timeout,
    };
    std::vector<char *> argv;
    argv.reserve(arguments.size() + 1);
    for (auto &argument : arguments)
      argv.push_back(argument.data());
    argv.push_back(nullptr);

    posix_spawnattr_t attributes;
    if (posix_spawnattr_init(&attributes) != 0) {
      *error = "initialize CUDA worker spawn attributes failed";
      (void)Shutdown(std::chrono::seconds(5), error);
      return false;
    }
    sigset_t empty_mask;
    sigemptyset(&empty_mask);
    int spawn_error = posix_spawnattr_setsigmask(&attributes, &empty_mask);
    short spawn_flags = POSIX_SPAWN_SETSIGMASK;
    if (spawn_error == 0)
      spawn_error = posix_spawnattr_setflags(&attributes, spawn_flags);
    pid_t pid = -1;
    if (spawn_error == 0)
      spawn_error = posix_spawn(&pid, config_.worker_binary.c_str(), nullptr,
                                &attributes, argv.data(), environ);
    posix_spawnattr_destroy(&attributes);
    if (spawn_error != 0) {
      *error = std::string("spawn CUDA worker: ") +
               std::strerror(spawn_error);
      (void)Shutdown(std::chrono::seconds(5), error);
      return false;
    }
    std::lock_guard lock(state_->mutex);
    state_->workers.push_back(State::Worker{
        .index = index,
        .pid = pid,
        .socket_path = socket_path,
        .client = std::make_shared<CudaWorkerClient>(socket_path,
                                                     config_.rpc_timeout,
                                                     config_.health_rpc_timeout),
    });
  }

  const auto startup_deadline =
      std::chrono::steady_clock::now() + config_.startup_timeout;
  for (;;) {
    ReapExitedChildren();
    bool all_ready = true;
    bool child_exited = false;
    std::vector<size_t> unchecked;
    {
      std::lock_guard lock(state_->mutex);
      for (const auto &worker : state_->workers) {
        if (!worker.alive) {
          child_exited = true;
          break;
        }
        if (!worker.healthy) {
          all_ready = false;
          unchecked.push_back(worker.index);
        }
      }
    }
    if (child_exited) {
      *error = "CUDA worker exited during prewarm";
      std::string shutdown_error;
      (void)Shutdown(std::chrono::seconds(5), &shutdown_error);
      return false;
    }
    if (all_ready)
      break;
    if (std::chrono::steady_clock::now() >= startup_deadline) {
      *error = "CUDA worker pool prewarm timed out";
      (void)Shutdown(std::chrono::seconds(5), error);
      return false;
    }
    for (size_t index : unchecked) {
      std::shared_ptr<CudaWorkerClient> client;
      {
        std::lock_guard lock(state_->mutex);
        client = state_->workers[index].client;
      }
      const CudaWorkerRpcResult health = client->Health();
      if (!health || health.response.cuda_status != 0 ||
          (health.response.flags & protocol::kResponseCapabilityDeferredCUDA) ==
              0 ||
          (config_.require_custom_storage &&
           (health.response.flags &
            protocol::kResponseCapabilityCustomStorage) == 0))
        continue;
      const std::string incarnation = Incarnation(health.response.output);
      if (incarnation.empty())
        continue;
      std::lock_guard lock(state_->mutex);
      if (state_->workers[index].alive) {
        state_->workers[index].healthy = true;
        state_->workers[index].incarnation = incarnation;
      }
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }

  {
    std::lock_guard lock(state_->mutex);
    state_->started = true;
  }
  monitor_ = std::make_unique<MonitorThread>(
      [this](std::stop_token stop) { MonitorChildren(stop); });
  return true;
}

std::optional<CudaWorkerPool::Lease>
CudaWorkerPool::TryAcquire(size_t weight) {
  if (weight == 0)
    return std::nullopt;
  std::lock_guard lock(state_->mutex);
  if (!state_->started || state_->shutting_down ||
      state_->fail_stop_required)
    return std::nullopt;
  // One uncertain idle-worker probe quarantines the complete fixed-size
  // generation. Letting a smaller request consume the remaining workers would
  // make admission depend on a worker whose identity/liveness is not currently
  // established. Existing leases remain usable and long rank RPCs stay
  // independent; only new admission is suspended until the probe recovers.
  if (!std::all_of(state_->workers.begin(), state_->workers.end(),
                   [](const State::Worker &worker) {
                     return worker.alive && worker.healthy;
                   }))
    return std::nullopt;
  std::vector<CudaWorkerHandle> selected;
  selected.reserve(weight);
  for (const auto &worker : state_->workers) {
    if (worker.alive && worker.healthy && !worker.leased &&
        !worker.health_checking) {
      selected.push_back(
          {.index = worker.index, .pid = worker.pid, .client = worker.client});
      if (selected.size() == weight)
        break;
    }
  }
  if (selected.size() != weight)
    return std::nullopt;
  for (const auto &worker : selected)
    state_->workers[worker.index].leased = true;
  return Lease(state_, std::move(selected));
}

bool CudaWorkerPool::Shutdown(std::chrono::milliseconds timeout,
                              std::string *error) {
  if (error == nullptr)
    return false;
  std::vector<pid_t> children;
  {
    std::lock_guard lock(state_->mutex);
    state_->shutting_down = true;
    for (const auto &worker : state_->workers) {
      if (worker.alive)
        children.push_back(worker.pid);
    }
  }
  for (pid_t child : children) {
    if (kill(child, SIGTERM) != 0 && errno != ESRCH) {
      *error = std::string("signal CUDA worker shutdown: ") +
               std::strerror(errno);
      return false;
    }
  }

  const auto deadline = std::chrono::steady_clock::now() + timeout;
  std::unique_lock lock(state_->mutex);
  while (std::any_of(state_->workers.begin(), state_->workers.end(),
                     [](const State::Worker &worker) { return worker.alive; })) {
    lock.unlock();
    ReapExitedChildren();
    lock.lock();
    const auto next_poll =
        std::min(deadline, std::chrono::steady_clock::now() +
                              std::chrono::milliseconds(10));
    if (state_->changed.wait_until(lock, next_poll) == std::cv_status::timeout &&
        std::chrono::steady_clock::now() >= deadline &&
        std::any_of(state_->workers.begin(), state_->workers.end(),
                    [](const State::Worker &worker) { return worker.alive; })) {
      *error = "timed out waiting for CUDA workers to terminate safely";
      return false;
    }
  }
  lock.unlock();

  if (monitor_ != nullptr) {
    monitor_->RequestStop();
    (void)pthread_kill(monitor_->NativeHandle(), SIGCHLD);
    monitor_->Join();
    monitor_.reset();
  }
  for (const auto &worker : state_->workers) {
    std::error_code ignored;
    fs::remove(worker.socket_path, ignored);
    fs::remove(worker.socket_path + ".health", ignored);
  }
  return true;
}

bool CudaWorkerPool::ready() const {
  std::lock_guard lock(state_->mutex);
  return state_->started && !state_->shutting_down &&
         !state_->fail_stop_required &&
         std::all_of(state_->workers.begin(), state_->workers.end(),
                     [](const State::Worker &worker) {
                       return worker.alive && worker.healthy;
                     });
}

bool CudaWorkerPool::fail_stop_required() const {
  std::lock_guard lock(state_->mutex);
  return state_->fail_stop_required;
}

std::vector<CudaWorkerSnapshot> CudaWorkerPool::Snapshot() const {
  std::lock_guard lock(state_->mutex);
  std::vector<CudaWorkerSnapshot> result;
  result.reserve(state_->workers.size());
  for (const auto &worker : state_->workers) {
    result.push_back({.index = worker.index,
                      .pid = worker.pid,
                      .alive = worker.alive,
                      .healthy = worker.healthy,
                      .leased = worker.leased,
                      .unknown_outcome = worker.unknown_outcome,
                      .health_checking = worker.health_checking,
                      .incarnation = worker.incarnation});
  }
  return result;
}

void CudaWorkerPool::MonitorChildren(std::stop_token stop) {
  sigset_t child_signal;
  sigemptyset(&child_signal);
  sigaddset(&child_signal, SIGCHLD);
  auto next_health = std::chrono::steady_clock::now() + config_.health_interval;
  while (!stop.stop_requested()) {
    const timespec wait{.tv_sec = 0, .tv_nsec = 100 * 1000 * 1000};
    const int signal = sigtimedwait(&child_signal, nullptr, &wait);
    if (signal == SIGCHLD || (signal < 0 && errno == EAGAIN))
      ReapExitedChildren();
    if (std::chrono::steady_clock::now() >= next_health) {
      CheckIdleWorkerHealth();
      next_health = std::chrono::steady_clock::now() + config_.health_interval;
    }
  }
  ReapExitedChildren();
}

void CudaWorkerPool::ReapExitedChildren() {
  std::vector<std::pair<size_t, pid_t>> children;
  {
    std::lock_guard lock(state_->mutex);
    for (const auto &worker : state_->workers) {
      if (worker.alive)
        children.emplace_back(worker.index, worker.pid);
    }
  }
  for (const auto &[index, pid] : children) {
    int status = 0;
    const pid_t result = waitpid(pid, &status, WNOHANG);
    if (result != pid && !(result < 0 && errno == ECHILD))
      continue;
    std::lock_guard lock(state_->mutex);
    auto &worker = state_->workers[index];
    if (!worker.alive || worker.pid != pid)
      continue;
    worker.alive = false;
    worker.healthy = false;
    if (!state_->shutting_down) {
      // The pool is deliberately fixed-size and never respawns in-process.
      // Even a strictly idle loss would otherwise shrink capacity forever and
      // make larger restores return BUSY indefinitely. Let PageBroker restart
      // the complete prewarmed generation instead.
      state_->fail_stop_required = true;
    }
    if (worker.leased || worker.retained_context_owners != 0)
      worker.unknown_outcome = true;
    state_->changed.notify_all();
  }
}

void CudaWorkerPool::CheckIdleWorkerHealth() {
  std::vector<size_t> workers;
  {
    std::lock_guard lock(state_->mutex);
    if (state_->shutting_down)
      return;
    for (const auto &worker : state_->workers)
      workers.push_back(worker.index);
  }
  for (size_t index : workers) {
    std::shared_ptr<CudaWorkerClient> client;
    {
      std::lock_guard lock(state_->mutex);
      auto &worker = state_->workers[index];
      if (state_->shutting_down || !worker.alive || worker.leased ||
          worker.health_checking)
        continue;
      worker.health_checking = true;
      client = worker.client;
    }
    const CudaWorkerRpcResult health = client->Health();
    const std::string incarnation = health ? Incarnation(health.response.output)
                                           : std::string{};
    std::lock_guard lock(state_->mutex);
    auto &worker = state_->workers[index];
    worker.health_checking = false;
    if (worker.alive) {
      const bool healthy = health && health.response.cuda_status == 0 &&
                           (health.response.flags &
                            protocol::kResponseCapabilityDeferredCUDA) != 0 &&
                           (!config_.require_custom_storage ||
                            (health.response.flags &
                             protocol::kResponseCapabilityCustomStorage) != 0) &&
                           !incarnation.empty() &&
                           incarnation == worker.incarnation;
      if (healthy) {
        worker.healthy = true;
        worker.consecutive_health_failures = 0;
      } else if (!health &&
                 HealthFailureIsTransportUncertainty(health.status)) {
        // A bounded health timeout cannot prove that the worker or a retained
        // CUDA context was lost. Quarantine admission and retry on the next
        // monitor cadence. Persistent uncertainty eventually converges via the
        // normal fail-closed shutdown path without making one transient probe
        // kill every retained workload in the generation.
        worker.healthy = false;
        ++worker.consecutive_health_failures;
        if (worker.consecutive_health_failures >=
            kPersistentHealthFailureThreshold) {
          state_->fail_stop_required = true;
          if (worker.retained_context_owners != 0)
            worker.unknown_outcome = true;
        }
      } else {
        // A response with a different incarnation, an explicit fatal health
        // result, or a malformed health protocol is affirmative evidence that
        // this fixed generation cannot be reused safely.
        worker.healthy = false;
        state_->fail_stop_required = true;
        if (worker.retained_context_owners != 0)
          worker.unknown_outcome = true;
      }
    }
    state_->changed.notify_all();
  }
}

} // namespace snapshot::pagebroker
