/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <pthread.h>
#include <signal.h>
#include <string>
#include <thread>
#include <utility>
#include <vector>

namespace cuda_checkpoint_daemon {

constexpr uint32_t kMagic = 0x50484344; // "DCHP" in little-endian.
constexpr uint16_t kVersion = 8;
constexpr uint16_t kLegacyVersion = 7;
constexpr size_t kRequestHeaderSize = 76;
constexpr size_t kLegacyRequestHeaderSize = 60;
constexpr size_t kResponseHeaderSize = 24;
constexpr size_t kMaxRequestSize = 64 * 1024;
constexpr size_t kMaxResponseSize = 128 * 1024;
constexpr size_t kMaxRestoreBatchTargets = 64;
constexpr size_t kMaxPinnedStorageFilesPerRequest = 64;
constexpr size_t kMaxCgroupSize = 4096;
constexpr size_t kMaxJobFileSize = 4096;

constexpr uint32_t kResponseFatal = 1U << 0;
constexpr uint32_t kResponseCapabilityDeferredCUDA = 1U << 1;
constexpr uint32_t kResponseCapabilityCustomStorage = 1U << 2;
constexpr uint32_t kResponseLockNotAcquired = 1U << 3;

enum class Action : uint16_t {
  kHealth = 0,
  kCheckpoint = 1,
  kRestore = 2,
  kLock = 3,
  kUnlock = 4,
  kRestoreBatch = 5,
};

enum class Backend : uint16_t {
  kUnspecified = 0,
  kRegular = 1,
  kPosix = 2,
};

// Cleanup must distinguish a confirmed exit/PID reuse from a transient
// observation failure. Releasing retained CUDA state on an inconclusive
// /proc read can break a live restored process.
enum class ProcessIdentityState : uint8_t {
  kMatches,
  kExitedOrReused,
  kIndeterminate,
};

enum class ProcessExistenceState : uint8_t {
  kExists,
  kMissing,
  kIndeterminate,
};

struct Request {
  Action action = Action::kHealth;
  Backend backend = Backend::kUnspecified;
  uint32_t pid = 0;
  uint32_t transfer_buffer_count = 0;
  uint64_t transfer_chunk_bytes = 0;
  uint64_t expected_start_time_ticks = 0;
  std::string device_map;
  std::string storage_dir;
  std::string expected_cgroup;
  std::string job_file;
  // PageBroker-observed identity of a nonempty launch-job file. Version 8
  // workers revalidate it under the process-wide job-file dispatch lock so
  // paths initially assigned to distinct workers cannot converge unnoticed.
  // An inode of zero means the legacy unpinned contract.
  uint64_t expected_job_file_device = 0;
  uint64_t expected_job_file_inode = 0;
  std::string selected_devices;
  // PageBroker-internal descriptor-backed POSIX restore source. This field is
  // never serialized on the standalone daemon protocol; it is borrowed only
  // by in-process ExecuteUncaptured calls and duplicated before CUDA dispatch.
  int storage_directory_fd = -1;
  std::string storage_relative_dir;
  struct PinnedStorageFile {
    std::string filename;
    int descriptor_fd = -1;
    uint64_t size = 0;
    uint64_t device = 0;
    uint64_t inode = 0;
  };
  // Exact manifest-declared carrier descriptors. Numeric descriptor values are
  // never encoded; PageBroker transfers the open files to its private worker
  // generation with SCM_RIGHTS and the receiver rebuilds this manifest-ordered
  // borrowed view for the duration of one request.
  std::vector<PinnedStorageFile> pinned_storage_files;
  std::vector<Request> targets;
};

// Exact process-lifetime handle established while the request identity still
// matches. Unlike a numeric PID or a later /proc lookup, this remains bound to
// the same process across PID reuse and transient procfs failures.
class PinnedProcess {
public:
  PinnedProcess(const PinnedProcess &) = delete;
  PinnedProcess &operator=(const PinnedProcess &) = delete;
  ~PinnedProcess() noexcept;

  const Request &request() const { return request_; }
  int descriptor() const { return pidfd_; }

private:
  friend bool PinMatchingProcesses(
      const std::vector<Request> &, const std::string &,
      std::vector<std::shared_ptr<PinnedProcess>> *, std::string *);
  PinnedProcess(Request request, int pidfd)
      : request_(std::move(request)), pidfd_(pidfd) {}

  Request request_;
  int pidfd_ = -1;
};

struct Response {
  int32_t cuda_status = 0;
  uint32_t flags = 0;
  std::string output;
  std::string error;
};

struct OperationState {
  bool handle_returned = false;
  bool completion_succeeded = false;

  bool fatal() const { return handle_returned && !completion_succeeded; }
};

struct HealthSnapshot {
  bool ready = false;
  bool busy = false;
  bool healthy = false;
  Action action = Action::kHealth;
  uint32_t pid = 0;
  uint64_t elapsed_seconds = 0;
  uint64_t deadline_seconds = 0;
  bool custom_storage_available = false;
  std::string incarnation;
};

class OperationHealth {
public:
  OperationHealth(std::chrono::seconds max_operation_duration,
                  std::string incarnation);

  void MarkReady(bool custom_storage_available);
  void Begin(Action action, uint32_t pid);
  void End();
  HealthSnapshot Snapshot() const;

private:
  using Clock = std::chrono::steady_clock;

  const std::chrono::seconds max_operation_duration_;
  mutable std::mutex mutex_;
  bool ready_ = false;
  bool custom_storage_available_ = false;
  bool busy_ = false;
  Action action_ = Action::kHealth;
  uint32_t pid_ = 0;
  Clock::time_point started_{};
  const std::string incarnation_;
};

class OwnedUnixSocket {
public:
  OwnedUnixSocket() = default;
  OwnedUnixSocket(const OwnedUnixSocket &) = delete;
  OwnedUnixSocket &operator=(const OwnedUnixSocket &) = delete;
  ~OwnedUnixSocket();

  bool Bind(const std::string &path, int backlog, std::string *error);
  void Close();
  int fd() const { return fd_; }

private:
  void UnlinkIfOwned();

  int fd_ = -1;
  int lock_fd_ = -1;
  std::string path_;
  uint64_t device_ = 0;
  uint64_t inode_ = 0;
  bool bound_ = false;
};

class ShutdownSignalOwner {
public:
  struct ShutdownResult {
    const char *operation = nullptr;
    int error_code = 0;

    bool ok() const { return error_code == 0; }
  };

  ShutdownSignalOwner() = default;
  ShutdownSignalOwner(const ShutdownSignalOwner &) = delete;
  ShutdownSignalOwner &operator=(const ShutdownSignalOwner &) = delete;
  ~ShutdownSignalOwner() noexcept;

  bool Start(std::string *error);
  ShutdownResult RequestShutdownNoThrow() noexcept;
  ShutdownResult StopAndJoinNoThrow() noexcept;
  bool StopAndJoin(std::string *error);
  void Close() noexcept;
  bool ShutdownRequested() const {
    return shutdown_requested_.load(std::memory_order_acquire);
  }
  int operation_stop_fd() const { return operation_stop_pipe_[0]; }
  int health_stop_fd() const { return health_stop_pipe_[0]; }

private:
  static void *ThreadEntry(void *owner) noexcept;
  void Run() noexcept;

  std::atomic<bool> shutdown_requested_{false};
  sigset_t signals_{};
  pthread_t thread_{};
  std::atomic<bool> thread_started_{false};
  int signal_fd_ = -1;
  int operation_stop_pipe_[2]{-1, -1};
  int health_stop_pipe_[2]{-1, -1};
  int control_pipe_[2]{-1, -1};
  int thread_error_ = 0;
};

// BoundedOutputCapture drains a pipe continuously while retaining at most
// limit bytes. This prevents verbose CUDA diagnostics from filling ephemeral
// storage or blocking the daemon on a full pipe.
class BoundedOutputCapture {
public:
  explicit BoundedOutputCapture(size_t limit) : limit_(limit) {}
  BoundedOutputCapture(const BoundedOutputCapture &) = delete;
  BoundedOutputCapture &operator=(const BoundedOutputCapture &) = delete;
  ~BoundedOutputCapture() noexcept;

  bool Start(std::string *error);
  int write_fd() const { return write_fd_; }
  bool Finish(std::string *output, bool *truncated, std::string *error);

private:
  void Drain() noexcept;

  size_t limit_;
  int read_fd_ = -1;
  int write_fd_ = -1;
  std::thread reader_;
  std::string output_;
  std::string read_error_;
  bool truncated_ = false;
};

using OperationExecutor = std::function<Response(const Request &)>;
using CompletionExecutor = std::function<int32_t()>;

bool ParseRequest(const unsigned char *data, size_t size, Request *request,
                  std::string *error,
                  const std::string &storage_root = "/checkpoints");
bool EncodeRequest(const Request &request, std::vector<unsigned char> *data,
                   std::string *error);
bool ParseResponse(const unsigned char *data, size_t size, Response *response,
                   std::string *error);
bool EncodeResponse(const Response &response, std::vector<unsigned char> *data,
                    std::string *error);
bool ValidateProcessIdentity(const Request &request,
                             const std::string &proc_root, std::string *error);
ProcessExistenceState InspectProcessExistence(uint32_t pid,
                                              const std::string &proc_root,
                                              std::string *error);
ProcessIdentityState InspectProcessIdentity(const Request &request,
                                            const std::string &proc_root,
                                            std::string *error);
bool PinMatchingProcesses(
    const std::vector<Request> &requests, const std::string &proc_root,
    std::vector<std::shared_ptr<PinnedProcess>> *targets, std::string *error);
bool InspectPinnedProcess(const PinnedProcess &target, bool *exited,
                          std::string *error);
bool TerminatePinnedProcesses(
    const std::vector<std::shared_ptr<PinnedProcess>> &targets,
    std::chrono::milliseconds timeout, std::string *error);
bool TerminateMatchingProcesses(const std::vector<Request> &requests,
                                const std::string &proc_root,
                                std::chrono::milliseconds timeout,
                                std::string *error);
bool ExecuteValidated(const Request &request, const std::string &proc_root,
                      const OperationExecutor &executor, Response *response);
bool ResponseAllowsServerContinue(const Response &response);
int32_t FinishHandledOperation(bool post_handle_succeeded,
                               int32_t failure_status,
                               const CompletionExecutor &completion,
                               OperationState *state);
// Returns 1 for input, 0 for stop, -1 for timeout, and -2 for poll failure
// with errno preserved for the caller.
int PollForInputOrStop(int input_fd, int stop_fd,
                       const std::function<void()> &before_poll = {},
                       int timeout_milliseconds = -1);
const char *ActionName(Action action);
Response HealthResponse(const OperationHealth &health);
Response HealthResponseAfterReap(const OperationHealth &health,
                                 int32_t release_status,
                                 const std::string &reap_warning);
bool OperationTimeoutMilliseconds(std::chrono::seconds duration,
                                  unsigned int *timeout_ms,
                                  std::string *error);
bool GenerateIncarnation(std::string *value, std::string *error);

} // namespace cuda_checkpoint_daemon
