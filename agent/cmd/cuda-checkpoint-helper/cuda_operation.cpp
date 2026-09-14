/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "cuda_operation.h"

#include <cuda.h>
#include <fcntl.h>
#include <signal.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <exception>
#include <filesystem>
#include <limits>
#include <memory>
#include <mutex>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <unordered_set>
#include <utility>
#include <vector>

#include "restore_batch_order.h"
#include "storage_manifest.h"
#include "transfer_config.h"
#include "transfer_engine.h"
#include "transfer_scheduler.h"

#if !defined(CUDA_VERSION) || CUDA_VERSION < 13040
#error "cuda-checkpoint-helper requires CUDA 13.4 or newer headers"
#endif

namespace cuda_checkpoint_operation {

bool ResolveRestoreBatchJobFileIdentity(
    const std::string &path, RestoreBatchJobFileIdentity *identity,
    std::string *error) {
  const int fd = open(path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (fd < 0) {
    *error = "open CUDA job file: " + std::string(std::strerror(errno));
    return false;
  }
  struct stat status {};
  const int stat_result = fstat(fd, &status);
  const int stat_error = errno;
  (void)close(fd);
  if (stat_result != 0) {
    *error = "stat CUDA job file: " + std::string(std::strerror(stat_error));
    return false;
  }
  if (!S_ISREG(status.st_mode)) {
    *error = "CUDA job file is not a regular file";
    return false;
  }
  *identity = {.present = true,
               .device = static_cast<std::uint64_t>(status.st_dev),
               .inode = static_cast<std::uint64_t>(status.st_ino)};
  return true;
}

namespace {

void PrintCudaError(CUresult status) {
  const char *name = nullptr;
  const char *message = nullptr;
  (void)cuGetErrorName(status, &name);
  (void)cuGetErrorString(status, &message);
  std::fprintf(stderr, "%s: %s\n",
               name == nullptr ? "CUDA_ERROR_UNKNOWN" : name,
               message == nullptr ? "unknown CUDA error" : message);
}

void PrintCudaStageError(const char *stage, int pid, CUresult status) {
  const char *name = nullptr;
  const char *message = nullptr;
  (void)cuGetErrorName(status, &name);
  (void)cuGetErrorString(status, &message);
  std::fprintf(stderr, "%s failed for pid %d: %s (%d): %s\n", stage, pid,
               name == nullptr ? "CUDA_ERROR_UNKNOWN" : name,
               static_cast<int>(status),
               message == nullptr ? "unknown CUDA error" : message);
}

void PrintRestoreFailureState(int pid, CUresult restore_status) {
  CUprocessState process_state{};
  const CUresult state_status =
      cuCheckpointProcessGetState(pid, &process_state);
  PrintCudaStageError("cuCheckpointProcessRestore", pid, restore_status);
  if (state_status == CUDA_SUCCESS) {
    std::fprintf(stderr,
                 "CUDA process state after restore failure for pid %d: %d\n",
                 pid, static_cast<int>(process_state));
    return;
  }
  PrintCudaStageError("cuCheckpointProcessGetState after restore failure", pid,
                      state_status);
}

namespace storage = cuda_checkpoint_storage;
namespace transfer = cuda_checkpoint_transfer;
namespace daemon_protocol = cuda_checkpoint_daemon;
using Clock = std::chrono::steady_clock;
using OperationCompleteFn = decltype(&cuCheckpointOperationComplete);
static_assert(kMaxRestoreBatchTargets ==
              daemon_protocol::kMaxRestoreBatchTargets);

class OwnedDirectoryDescriptor {
public:
  OwnedDirectoryDescriptor() = default;
  explicit OwnedDirectoryDescriptor(int fd) : fd_(fd) {}
  OwnedDirectoryDescriptor(const OwnedDirectoryDescriptor &) = delete;
  OwnedDirectoryDescriptor &operator=(const OwnedDirectoryDescriptor &) = delete;
  OwnedDirectoryDescriptor(OwnedDirectoryDescriptor &&other) noexcept
      : fd_(std::exchange(other.fd_, -1)) {}
  OwnedDirectoryDescriptor &operator=(OwnedDirectoryDescriptor &&other) noexcept {
    if (this != &other) {
      if (fd_ >= 0)
        (void)close(fd_);
      fd_ = std::exchange(other.fd_, -1);
    }
    return *this;
  }
  ~OwnedDirectoryDescriptor() {
    if (fd_ >= 0)
      (void)close(fd_);
  }
  int get() const { return fd_; }

private:
  int fd_ = -1;
};

bool StatMatchesJobFileIdentity(
    const struct stat &status,
    const RestoreBatchJobFileIdentity &expected_identity) {
  return expected_identity.present && S_ISREG(status.st_mode) &&
         static_cast<std::uint64_t>(status.st_dev) ==
             expected_identity.device &&
         static_cast<std::uint64_t>(status.st_ino) == expected_identity.inode;
}

bool MatchesExpectedJobFileIdentity(
    const daemon_protocol::Request &request,
    const RestoreBatchJobFileIdentity &actual) {
  return request.expected_job_file_inode == 0 ||
         (actual.present &&
          actual.device == request.expected_job_file_device &&
          actual.inode == request.expected_job_file_inode);
}

// Pins a verified launch-job inode for the whole batch and re-opens the
// representative path while holding CudaJobFileMutex immediately before CUDA
// dispatch. A rename/replacement therefore fails closed instead of silently
// selecting a different launch job after aliases were grouped.
class PinnedJobFile {
public:
  PinnedJobFile() = default;
  PinnedJobFile(const PinnedJobFile &) = delete;
  PinnedJobFile &operator=(const PinnedJobFile &) = delete;
  PinnedJobFile(PinnedJobFile &&other) noexcept
      : path_(std::move(other.path_)), identity_(other.identity_),
        fd_(std::exchange(other.fd_, -1)) {}
  PinnedJobFile &operator=(PinnedJobFile &&other) noexcept {
    if (this != &other) {
      if (fd_ >= 0)
        (void)close(fd_);
      path_ = std::move(other.path_);
      identity_ = other.identity_;
      fd_ = std::exchange(other.fd_, -1);
    }
    return *this;
  }
  ~PinnedJobFile() {
    if (fd_ >= 0)
      (void)close(fd_);
  }

  static bool Open(const RestoreBatchJobFileGroup &group, PinnedJobFile *out,
                   std::string *error) {
    PinnedJobFile pinned;
    pinned.path_ = group.representative;
    pinned.identity_ = group.identity;
    if (pinned.path_.empty()) {
      if (pinned.identity_.present) {
        *error = "empty CUDA job file has a file identity";
        return false;
      }
      *out = std::move(pinned);
      return true;
    }
    pinned.fd_ = open(pinned.path_.c_str(),
                      O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (pinned.fd_ < 0) {
      *error = "pin CUDA job file: " + std::string(std::strerror(errno));
      return false;
    }
    struct stat status {};
    if (fstat(pinned.fd_, &status) != 0 ||
        !StatMatchesJobFileIdentity(status, pinned.identity_)) {
      *error = "CUDA job-file identity changed before it could be pinned";
      return false;
    }
    *out = std::move(pinned);
    return true;
  }

  bool Revalidate(std::string *error) const {
    if (path_.empty())
      return fd_ < 0 && !identity_.present;
    struct stat pinned_status {};
    if (fd_ < 0 || fstat(fd_, &pinned_status) != 0 ||
        !StatMatchesJobFileIdentity(pinned_status, identity_)) {
      *error = "pinned CUDA job-file identity changed before dispatch";
      return false;
    }
    const int current_fd =
        open(path_.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (current_fd < 0) {
      *error = "reopen CUDA job file before dispatch: " +
               std::string(std::strerror(errno));
      return false;
    }
    struct stat current_status {};
    const bool matches = fstat(current_fd, &current_status) == 0 &&
                         StatMatchesJobFileIdentity(current_status, identity_);
    (void)close(current_fd);
    if (!matches) {
      *error = "CUDA job-file path changed identity before dispatch";
      return false;
    }
    return true;
  }

private:
  std::string path_;
  RestoreBatchJobFileIdentity identity_;
  int fd_ = -1;
};

OwnedDirectoryDescriptor OpenDirectStorageDirectory(
    int root_fd, const std::string &relative, std::string *error) {
  if (root_fd < 0 || relative.empty() ||
      std::filesystem::path(relative).is_absolute()) {
    *error = "invalid descriptor-backed storage directory";
    return {};
  }
  int duplicate = fcntl(root_fd, F_DUPFD_CLOEXEC, 0);
  if (duplicate < 0) {
    *error = "duplicate descriptor-backed storage root: " +
             std::string(std::strerror(errno));
    return {};
  }
  OwnedDirectoryDescriptor current(duplicate);
  for (const auto &part : std::filesystem::path(relative)) {
    const std::string component = part.string();
    if (component.empty() || component == "." || component == "..") {
      *error = "unsafe descriptor-backed storage path component";
      return {};
    }
    const int next = openat(current.get(), component.c_str(),
                            O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
    if (next < 0) {
      *error = "open descriptor-backed storage directory: " +
               std::string(std::strerror(errno));
      return {};
    }
    current = OwnedDirectoryDescriptor(next);
  }
  return current;
}

std::string ProcessRoot() {
  const char *configured = std::getenv("CUDA_CHECKPOINT_PROC_ROOT");
  return configured == nullptr || configured[0] != '/'
             ? std::string("/host/proc")
             : std::string(configured);
}

// CUDA_CHECKPOINT_JOB_FILE is a process-wide selector consumed while a
// checkpoint/restore handle is created. Serialize distinct job-file scopes so
// none can observe another launch job. A restore batch creates every handle in
// captured order under one scope after proving every alias names the same
// inode. The potentially long CustomStorage transfer runs after this section.
std::timed_mutex &CudaJobFileMutex() {
  static std::timed_mutex mutex;
  return mutex;
}

template <typename Operation>
CUresult InvokeWithCudaJobFile(const std::string &job_file,
                               const daemon_protocol::Request *request,
                               transfer::TransferCancellation *cancellation,
                               Operation operation,
                               const PinnedJobFile *pinned_job_file = nullptr) {
  std::unique_lock lock(CudaJobFileMutex(), std::defer_lock);
  while (!lock.try_lock_for(std::chrono::milliseconds(50))) {
    if (cancellation != nullptr && cancellation->IsCancelled()) {
      std::fprintf(stderr,
                   "CUDA job-file operation cancelled before dispatch\n");
      return CUDA_ERROR_OPERATING_SYSTEM;
    }
  }
  if (cancellation != nullptr && cancellation->IsCancelled()) {
    std::fprintf(stderr, "CUDA job-file operation cancelled before dispatch\n");
    return CUDA_ERROR_OPERATING_SYSTEM;
  }
  if (request != nullptr) {
    std::string identity_error;
    if (!daemon_protocol::ValidateProcessIdentity(*request, ProcessRoot(),
                                                  &identity_error)) {
      std::fprintf(stderr,
                   "process identity changed while waiting for CUDA job-file "
                   "dispatch: %s\n",
                   identity_error.c_str());
      return CUDA_ERROR_INVALID_VALUE;
    }
    if (request->expected_job_file_inode != 0) {
      RestoreBatchJobFileIdentity actual;
      if (!ResolveRestoreBatchJobFileIdentity(job_file, &actual,
                                              &identity_error) ||
          !MatchesExpectedJobFileIdentity(*request, actual)) {
        std::fprintf(stderr,
                     "CUDA job-file identity changed before dispatch: %s\n",
                     identity_error.empty() ? "identity mismatch"
                                            : identity_error.c_str());
        return CUDA_ERROR_INVALID_VALUE;
      }
    }
  }
  if (pinned_job_file != nullptr) {
    std::string validation_error;
    if (!pinned_job_file->Revalidate(&validation_error)) {
      std::fprintf(stderr, "%s\n", validation_error.c_str());
      return CUDA_ERROR_INVALID_VALUE;
    }
  }
  if ((!job_file.empty() &&
       setenv("CUDA_CHECKPOINT_JOB_FILE", job_file.c_str(), 1) != 0) ||
      (job_file.empty() && unsetenv("CUDA_CHECKPOINT_JOB_FILE") != 0)) {
    std::perror("configure CUDA_CHECKPOINT_JOB_FILE");
    return CUDA_ERROR_OPERATING_SYSTEM;
  }
  return operation();
}

OperationCompleteFn ResolveOperationComplete(bool *available) {
  void *symbol = nullptr;
  CUdriverProcAddressQueryResult query_status =
      CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  const CUresult status =
      cuGetProcAddress("cuCheckpointOperationComplete", &symbol, 13040,
                       CU_GET_PROC_ADDRESS_DEFAULT, &query_status);
  *available = status == CUDA_SUCCESS && symbol != nullptr &&
               query_status == CU_GET_PROC_ADDRESS_SUCCESS;
  return *available ? reinterpret_cast<OperationCompleteFn>(symbol) : nullptr;
}

double SecondsSince(Clock::time_point start) {
  return std::chrono::duration<double>(Clock::now() - start).count();
}

double SecondsBetween(Clock::time_point start, Clock::time_point end) {
  return std::chrono::duration<double>(end - start).count();
}

// Success has the detailed telemetry emitted at the end of the batch. This
// guard guarantees that every other exit still records enough structured data
// to distinguish job-file grouping, participant preparation, transfer, and
// completion failures without relying on interleaved worker-thread stderr.
class RestoreBatchFailureTelemetry {
public:
  RestoreBatchFailureTelemetry(std::size_t targets, Clock::time_point start)
      : targets_(targets), start_(start) {}

  RestoreBatchFailureTelemetry(const RestoreBatchFailureTelemetry &) = delete;
  RestoreBatchFailureTelemetry &
  operator=(const RestoreBatchFailureTelemetry &) = delete;

  ~RestoreBatchFailureTelemetry() {
    if (success_)
      return;
    const double prepare_seconds =
        preparation_complete_
            ? SecondsBetween(start_, preparation_end_)
            : SecondsSince(start_);
    std::fprintf(
        stdout,
        "{\"event\":\"cuda_custom_storage_restore_batch_failure\","
        "\"schema_version\":1,\"targets\":%zu,"
        "\"prepare_job_file_groups\":%zu,"
        "\"prepare_parallelism_observed\":%zu,"
        "\"failed_pid\":%u,\"failed_phase\":\"%s\","
        "\"cuda_status\":%d,\"prepare_wall_seconds\":%.6f,"
        "\"batch_total_seconds\":%.6f}\n",
        targets_, group_count_, observed_preparation_width_, failed_pid_,
        failed_phase_, static_cast<int>(failure_status_), prepare_seconds,
        SecondsSince(start_));
  }

  void SetGroupCount(std::size_t count) { group_count_ = count; }
  void ObservePreparationWidth(std::size_t width) {
    observed_preparation_width_ =
        std::max(observed_preparation_width_, width);
  }
  void FinishPreparation() {
    preparation_end_ = Clock::now();
    preparation_complete_ = true;
  }
  void Fail(const char *phase, CUresult status, std::uint32_t pid = 0) {
    failed_phase_ = phase;
    failure_status_ = status;
    failed_pid_ = pid;
  }
  void MarkSuccess() { success_ = true; }

private:
  std::size_t targets_ = 0;
  Clock::time_point start_;
  Clock::time_point preparation_end_;
  std::size_t group_count_ = 0;
  std::size_t observed_preparation_width_ = 0;
  std::uint32_t failed_pid_ = 0;
  const char *failed_phase_ = "request_validation";
  CUresult failure_status_ = CUDA_ERROR_INVALID_VALUE;
  bool preparation_complete_ = false;
  bool success_ = false;
};

CUresult DeviceUUID(CUdevice device, std::string *uuid_out);

class OperationContexts {
public:
  OperationContexts() = default;
  OperationContexts(const OperationContexts &) = delete;
  OperationContexts &operator=(const OperationContexts &) = delete;

  CUresult RetainAll(int *device_count, double *enumeration_seconds,
                     double *retain_seconds) {
    const auto enumeration_start = Clock::now();
    int count = 0;
    CUresult status = cuDeviceGetCount(&count);
    *enumeration_seconds = SecondsSince(enumeration_start);
    *device_count = count;
    if (status != CUDA_SUCCESS) {
      return status;
    }

    contexts_.reserve(count);
    for (int ordinal = 0; ordinal < count; ++ordinal) {
      const auto retain_start = Clock::now();
      CUdevice device = 0;
      status = cuDeviceGet(&device, ordinal);
      if (status != CUDA_SUCCESS) {
        *retain_seconds += SecondsSince(retain_start);
        return status;
      }
      CUcontext context = nullptr;
      status = cuDevicePrimaryCtxRetain(&context, device);
      *retain_seconds += SecondsSince(retain_start);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      contexts_.push_back({device, context});
    }
    return CUDA_SUCCESS;
  }

  CUresult RetainSelected(const std::vector<std::string> &selected_devices,
                          int *device_count, double *enumeration_seconds,
                          double *retain_seconds) {
    const auto enumeration_start = Clock::now();
    int count = 0;
    CUresult status = cuDeviceGetCount(&count);
    *enumeration_seconds = SecondsSince(enumeration_start);
    *device_count = count;
    if (status != CUDA_SUCCESS) {
      return status;
    }

    const std::unordered_set<std::string> selected(selected_devices.begin(),
                                                   selected_devices.end());
    contexts_.reserve(selected.size());
    for (int ordinal = 0; ordinal < count; ++ordinal) {
      CUdevice device = 0;
      status = cuDeviceGet(&device, ordinal);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      std::string uuid;
      status = DeviceUUID(device, &uuid);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      if (!selected.contains(uuid)) {
        continue;
      }
      const auto retain_start = Clock::now();
      CUcontext context = nullptr;
      status = cuDevicePrimaryCtxRetain(&context, device);
      *retain_seconds += SecondsSince(retain_start);
      if (status != CUDA_SUCCESS) {
        return status;
      }
      contexts_.push_back({device, context});
    }
    if (contexts_.size() != selected.size()) {
      return CUDA_ERROR_INVALID_DEVICE;
    }
    return CUDA_SUCCESS;
  }

  std::vector<CUdevice> DetachDevices() {
    std::vector<CUdevice> devices;
    devices.reserve(contexts_.size());
    for (const auto &entry : contexts_) {
      devices.push_back(entry.device);
    }
    contexts_.clear();
    return devices;
  }

  // Forget the bookkeeping without releasing the CUDA references. This is
  // reserved for allocation failures while transferring ownership to the
  // persistent target table: leaking until fatal helper shutdown is safer
  // than releasing a restored target's contexts before that target is killed.
  void Abandon() noexcept { contexts_.clear(); }

  CUresult ReleaseAll() {
    CUresult first_error = CUDA_SUCCESS;
    while (!contexts_.empty()) {
      const CUresult status =
          cuDevicePrimaryCtxRelease(contexts_.back().device);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
      contexts_.pop_back();
    }
    return first_error;
  }

  CUresult ReleaseUnused(const std::vector<CUdevice> &used_devices) {
    const std::unordered_set<CUdevice> used(used_devices.begin(),
                                            used_devices.end());
    CUresult first_error = CUDA_SUCCESS;
    auto retained = contexts_.begin();
    while (retained != contexts_.end()) {
      if (used.contains(retained->device)) {
        ++retained;
        continue;
      }
      const CUresult status = cuDevicePrimaryCtxRelease(retained->device);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
      if (status == CUDA_SUCCESS) {
        retained = contexts_.erase(retained);
      } else {
        // Keep the reference tracked so fatal cleanup can terminate the
        // identity-matched target and retry releasing it safely.
        ++retained;
      }
    }
    return first_error;
  }

  CUresult ContextAndDeviceForStream(CUstream stream, CUcontext *context_out,
                                     CUdevice *device_out) const {
    CUcontext stream_context = nullptr;
    CUresult status = cuStreamGetCtx(stream, &stream_context);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    for (const auto &retained : contexts_) {
      if (retained.context == stream_context) {
        *context_out = retained.context;
        *device_out = retained.device;
        return CUDA_SUCCESS;
      }
    }
    return CUDA_ERROR_INVALID_CONTEXT;
  }

  CUresult RetainForStream(CUstream stream, CUcontext *context_out,
                           CUdevice *device_out, double *retain_seconds) {
    CUcontext stream_context = nullptr;
    CUresult status = cuStreamGetCtx(stream, &stream_context);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    CUdevice stream_device = 0;
    status = cuCtxGetDevice_v2(&stream_device, stream_context);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    for (const auto &retained : contexts_) {
      if (retained.device == stream_device) {
        if (retained.context != stream_context) {
          return CUDA_ERROR_INVALID_CONTEXT;
        }
        *context_out = retained.context;
        *device_out = retained.device;
        return CUDA_SUCCESS;
      }
    }
    const auto retain_start = Clock::now();
    CUcontext retained_context = nullptr;
    status = cuDevicePrimaryCtxRetain(&retained_context, stream_device);
    *retain_seconds += SecondsSince(retain_start);
    if (status != CUDA_SUCCESS) {
      return status;
    }
    if (retained_context != stream_context) {
      (void)cuDevicePrimaryCtxRelease(stream_device);
      return CUDA_ERROR_INVALID_CONTEXT;
    }
    contexts_.push_back({stream_device, retained_context});
    *context_out = retained_context;
    *device_out = stream_device;
    return CUDA_SUCCESS;
  }

  ~OperationContexts() { (void)ReleaseAll(); }

  size_t size() const { return contexts_.size(); }

private:
  struct Entry {
    CUdevice device;
    CUcontext context;
  };

  std::vector<Entry> contexts_;
};

// CUDA 13.4 CustomStorage restore qualification found that releasing the
// helper's retained primary-context reference while the target remained alive
// could later fault that target. Keep one operation reference with the exact
// PID/start-time/cgroup identity and release it only after confirmed exit or
// PID reuse. An inconclusive /proc read must retain the reference and block new
// work rather than guessing that the target exited.
class PersistentTargetContexts {
public:
  PersistentTargetContexts() = default;
  PersistentTargetContexts(const PersistentTargetContexts &) = delete;
  PersistentTargetContexts &
  operator=(const PersistentTargetContexts &) = delete;

  CUresult Adopt(OperationContexts *contexts,
                 const daemon_protocol::Request &request) noexcept {
    try {
      std::lock_guard lock(mutex_);
      for (const auto &target : targets_) {
        if (SameIdentity(target.request, request)) {
          return contexts->ReleaseAll();
        }
      }
      std::vector<std::shared_ptr<daemon_protocol::PinnedProcess>> pinned;
      std::string pin_error;
      if (!daemon_protocol::PinMatchingProcesses(
              {request}, ProcessRoot(), &pinned, &pin_error) ||
          pinned.size() != 1) {
        // CUDA has already restored the target. Releasing the operation's
        // retained primary-context reference here can fault that live process.
        // Leak the reference into this fail-stopped worker and let PageBroker's
        // independently pinned target owner kill the workload before stopping
        // the worker.
        contexts->Abandon();
        std::fprintf(stderr,
                     "failed to pin restored target %u before retaining CUDA "
                     "contexts: %s\n",
                     request.pid,
                     pin_error.empty() ? "unknown error" : pin_error.c_str());
        return CUDA_ERROR_OPERATING_SYSTEM;
      }
      // Add the identity before detaching the contexts. DetachDevices has the
      // strong exception guarantee: allocation failure leaves |contexts|
      // intact. If either allocation fails, the catch below deliberately
      // abandons the bookkeeping so stack unwinding cannot release live
      // restored-target contexts before PageBroker's fatal shutdown.
      targets_.push_back({request, std::move(pinned.front()), {}});
      targets_.back().devices = contexts->DetachDevices();
      return CUDA_SUCCESS;
    } catch (const std::bad_alloc &) {
      contexts->Abandon();
      return CUDA_ERROR_OUT_OF_MEMORY;
    } catch (...) {
      contexts->Abandon();
      return CUDA_ERROR_OPERATING_SYSTEM;
    }
  }

  CUresult ReapExited(const std::string &,
                      std::string *identity_error) {
    std::lock_guard lock(mutex_);
    CUresult first_error = CUDA_SUCCESS;
    auto target = targets_.begin();
    while (target != targets_.end()) {
      bool exited = false;
      std::string target_error;
      if (!daemon_protocol::InspectPinnedProcess(*target->process, &exited,
                                                  &target_error)) {
        if (identity_error != nullptr && identity_error->empty()) {
          *identity_error = "cannot inspect pinned target " +
                            std::to_string(target->request.pid) +
                            ": " + target_error;
        }
        ++target;
        continue;
      }
      if (!exited) {
        ++target;
        continue;
      }
      const CUresult status = ReleaseDevices(target->devices);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
      target = targets_.erase(target);
    }
    return first_error;
  }

  CUresult ReleaseAll() {
    std::lock_guard lock(mutex_);
    return ReleaseAllLocked();
  }

  CUresult TerminateAll(const std::string &,
                        std::string *identity_error) {
    std::lock_guard lock(mutex_);
    std::vector<std::shared_ptr<daemon_protocol::PinnedProcess>> targets;
    targets.reserve(targets_.size());
    for (const auto &target : targets_) {
      targets.push_back(target.process);
    }
    if (!daemon_protocol::TerminatePinnedProcesses(
            targets, std::chrono::seconds(5), identity_error)) {
      return CUDA_ERROR_OPERATING_SYSTEM;
    }
    return CUDA_SUCCESS;
  }

  ~PersistentTargetContexts() { (void)ReleaseAll(); }

private:
  struct TargetContexts {
    daemon_protocol::Request request;
    std::shared_ptr<daemon_protocol::PinnedProcess> process;
    std::vector<CUdevice> devices;
  };

  static bool SameIdentity(const daemon_protocol::Request &left,
                           const daemon_protocol::Request &right) {
    return left.pid == right.pid &&
           left.expected_start_time_ticks == right.expected_start_time_ticks &&
           left.expected_cgroup == right.expected_cgroup;
  }

  static CUresult ReleaseDevices(const std::vector<CUdevice> &devices) {
    CUresult first_error = CUDA_SUCCESS;
    for (const CUdevice device : devices) {
      const CUresult status = cuDevicePrimaryCtxRelease(device);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
    }
    return first_error;
  }

  CUresult ReleaseAllLocked() {
    CUresult first_error = CUDA_SUCCESS;
    for (const auto &target : targets_) {
      const CUresult status = ReleaseDevices(target.devices);
      if (first_error == CUDA_SUCCESS && status != CUDA_SUCCESS) {
        first_error = status;
      }
    }
    targets_.clear();
    return first_error;
  }

  std::mutex mutex_;
  std::vector<TargetContexts> targets_;
};

bool ParseUUID(const char *value, CUuuid *uuid_out) {
  if (value == nullptr || uuid_out == nullptr) {
    return false;
  }
  std::array<unsigned char, 16> bytes{};
  if (!storage::ParseGPUUUID(value, &bytes)) {
    return false;
  }
  static_assert(sizeof(uuid_out->bytes) == bytes.size());
  std::memcpy(uuid_out->bytes, bytes.data(), bytes.size());
  return true;
}

bool ParseDeviceMap(const std::string &device_map,
                    std::vector<CUcheckpointGpuPair> *pairs,
                    std::vector<storage::DevicePair> *storage_pairs = nullptr) {
  if (device_map.empty()) {
    return true;
  }
  std::unordered_set<std::string> source_uuids;
  std::unordered_set<std::string> destination_uuids;
  std::istringstream input(device_map);
  std::string pair;
  while (std::getline(input, pair, ',')) {
    size_t separator = pair.find('=');
    if (separator == std::string::npos ||
        pair.find('=', separator + 1) != std::string::npos) {
      return false;
    }
    CUcheckpointGpuPair parsed{};
    const std::string source_input = pair.substr(0, separator);
    const std::string destination_input = pair.substr(separator + 1);
    std::string source;
    std::string destination;
    if (!ParseUUID(source_input.c_str(), &parsed.oldUuid) ||
        !ParseUUID(destination_input.c_str(), &parsed.newUuid) ||
        !storage::CanonicalizeGPUUUID(source_input, &source) ||
        !storage::CanonicalizeGPUUUID(destination_input, &destination) ||
        !source_uuids.insert(source).second ||
        !destination_uuids.insert(destination).second) {
      return false;
    }
    pairs->push_back(parsed);
    if (storage_pairs != nullptr) {
      storage_pairs->push_back({std::move(source), std::move(destination)});
    }
  }
  return !pairs->empty();
}

bool ParseDeviceSelection(const std::string &selected_devices,
                          std::vector<std::string> *devices) {
  if (selected_devices.empty()) {
    return false;
  }
  std::unordered_set<std::string> seen;
  std::istringstream input(selected_devices);
  std::string value;
  while (std::getline(input, value, ',')) {
    std::string canonical;
    if (!storage::CanonicalizeGPUUUID(value, &canonical) ||
        !seen.insert(canonical).second) {
      return false;
    }
    devices->push_back(std::move(canonical));
  }
  return !devices->empty();
}

const daemon_protocol::Request::PinnedStorageFile *FindPinnedStorageFile(
    const daemon_protocol::Request &request, const std::string &filename) {
  const auto found = std::find_if(
      request.pinned_storage_files.begin(), request.pinned_storage_files.end(),
      [&](const auto &file) { return file.filename == filename; });
  return found == request.pinned_storage_files.end() ? nullptr : &*found;
}

bool ValidatePinnedStorageFiles(
    const daemon_protocol::Request &request,
    const std::vector<storage::ManifestExtent> &manifest,
    std::string *error) {
  if (request.pinned_storage_files.empty())
    return true;
  if (request.pinned_storage_files.size() != manifest.size()) {
    *error = "pinned carrier set does not exactly match manifest";
    return false;
  }
  std::unordered_set<std::string> seen;
  for (const auto &extent : manifest) {
    const auto *file = FindPinnedStorageFile(request, extent.filename);
    if (file == nullptr || !seen.insert(file->filename).second ||
        file->descriptor_fd < 0 || file->size != extent.size) {
      *error = "pinned carrier set does not exactly match manifest";
      return false;
    }
    struct stat status{};
    if (fstat(file->descriptor_fd, &status) != 0 ||
        !S_ISREG(status.st_mode) || status.st_size < 0 ||
        static_cast<uint64_t>(status.st_size) != file->size ||
        static_cast<uint64_t>(status.st_dev) != file->device ||
        static_cast<uint64_t>(status.st_ino) != file->inode) {
      *error = "pinned carrier changed identity or size";
      return false;
    }
  }
  return true;
}

CUresult DeviceUUID(CUdevice device, std::string *uuid_out) {
  CUuuid uuid{};
  CUresult status = cuDeviceGetUuid(&uuid, device);
  if (status != CUDA_SUCCESS) {
    return status;
  }
  std::array<unsigned char, 16> bytes{};
  static_assert(sizeof(uuid.bytes) == bytes.size());
  std::memcpy(bytes.data(), uuid.bytes, bytes.size());
  *uuid_out = storage::FormatGPUUUID(bytes);
  return CUDA_SUCCESS;
}

struct CustomStorageResult {
  CUresult status = CUDA_SUCCESS;
  daemon_protocol::OperationState operation;
  bool fatal = false;
};

struct PreparedCustomStorageRestore {
  const daemon_protocol::Request *request = nullptr;
  std::filesystem::path storage_dir;
  transfer::TransferOptions transfer_options;
  std::unique_ptr<OperationContexts> operation_contexts;
  CUcheckpointCustomStorageInfo *info = nullptr;
  daemon_protocol::OperationState operation{.handle_returned = true};
  std::vector<storage::ManifestExtent> manifest;
  std::vector<storage::TransferJob> transfer_jobs;
  std::vector<transfer::ScheduledTransfer> scheduled_transfers;
  OwnedDirectoryDescriptor direct_storage_directory;
  size_t total_bytes = 0;
  size_t pinned_bytes = 0;
  int visible_cuda_device_count = 0;
  double storage_directory_validation_seconds = 0.0;
  double device_enumeration_seconds = 0.0;
  double target_context_discovery_seconds = 0.0;
  double primary_context_retain_seconds = 0.0;
  double primary_context_release_seconds = 0.0;
  double manifest_validation_seconds = 0.0;
  double device_map_preparation_seconds = 0.0;
  double cuda_process_api_seconds = 0.0;
  double metadata_job_construction_seconds = 0.0;
};

CustomStorageResult
FailPreparedRestore(PreparedCustomStorageRestore *prepared, CUresult failure,
                    PersistentTargetContexts *persistent_contexts) {
  const CUresult status =
      static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
          false, failure, [] { return static_cast<int32_t>(CUDA_SUCCESS); },
          &prepared->operation));
  const CUresult release_status =
      persistent_contexts == nullptr
          ? prepared->operation_contexts->ReleaseAll()
          : persistent_contexts->Adopt(prepared->operation_contexts.get(),
                                       *prepared->request);
  if (release_status != CUDA_SUCCESS) {
    std::fprintf(stderr,
                 "failed to preserve operation CUDA contexts with status %d "
                 "while handling status %d\n",
                 static_cast<int>(release_status), static_cast<int>(status));
  }
  return {.status = status,
          .operation = prepared->operation,
          .fatal =
              prepared->operation.fatal() || release_status != CUDA_SUCCESS};
}

// Any unexpected exception after the first batch restore handle must preserve
// every outstanding target context before stack unwinding destroys the
// prepared operations. PageBroker has already registered the complete target
// identity set and treats the escaping exception as fatal.
class PreparedRestoreContextsGuard {
public:
  PreparedRestoreContextsGuard(
      std::vector<std::unique_ptr<PreparedCustomStorageRestore>> *prepared,
      PersistentTargetContexts *persistent_contexts)
      : prepared_(prepared), persistent_contexts_(persistent_contexts) {}

  PreparedRestoreContextsGuard(const PreparedRestoreContextsGuard &) = delete;
  PreparedRestoreContextsGuard &
  operator=(const PreparedRestoreContextsGuard &) = delete;

  ~PreparedRestoreContextsGuard() {
    for (auto &target : *prepared_) {
      if (target != nullptr && target->operation_contexts != nullptr &&
          target->operation_contexts->size() != 0) {
        (void)persistent_contexts_->Adopt(target->operation_contexts.get(),
                                          *target->request);
      }
    }
  }

private:
  std::vector<std::unique_ptr<PreparedCustomStorageRestore>> *prepared_;
  PersistentTargetContexts *persistent_contexts_;
};

CustomStorageResult PrepareCustomStorageRestore(
    const daemon_protocol::Request &request,
    const transfer::TransferOptions &transfer_options,
    OperationCompleteFn operation_complete,
    PersistentTargetContexts *persistent_contexts,
    transfer::TransferCancellation *cancellation,
    bool report_failure_details,
    std::unique_ptr<PreparedCustomStorageRestore> *prepared_out) {
  if (operation_complete == nullptr) {
    if (report_failure_details)
      std::fprintf(stderr, "CUDA custom storage unavailable\n");
    return {CUDA_ERROR_NOT_SUPPORTED, {}};
  }
  auto prepared = std::make_unique<PreparedCustomStorageRestore>();
  prepared->request = &request;
  prepared->storage_dir = request.storage_dir;
  prepared->transfer_options = transfer_options;
  prepared->operation_contexts = std::make_unique<OperationContexts>();

  const auto storage_directory_start = Clock::now();
  struct stat directory_stat{};
  if (!prepared->storage_dir.is_absolute() ||
      lstat(prepared->storage_dir.c_str(), &directory_stat) != 0 ||
      !S_ISDIR(directory_stat.st_mode) ||
      (directory_stat.st_mode & 0022) != 0) {
    if (report_failure_details) {
      std::fprintf(stderr, "custom storage directory is missing or invalid\n");
    }
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  if (request.storage_directory_fd >= 0) {
    std::string direct_error;
    prepared->direct_storage_directory = OpenDirectStorageDirectory(
        request.storage_directory_fd, request.storage_relative_dir,
        &direct_error);
    if (prepared->direct_storage_directory.get() < 0) {
      if (report_failure_details)
        std::fprintf(stderr, "%s\n", direct_error.c_str());
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  } else if (!request.storage_relative_dir.empty()) {
    if (report_failure_details) {
      std::fprintf(stderr,
                   "descriptor-backed storage path has no source descriptor\n");
    }
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  prepared->storage_directory_validation_seconds =
      SecondsSince(storage_directory_start);

  std::vector<std::string> selected_devices;
  if (!ParseDeviceSelection(request.selected_devices, &selected_devices)) {
    if (report_failure_details)
      std::fprintf(stderr, "invalid selected CUDA devices\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  CUresult status = prepared->operation_contexts->RetainSelected(
      selected_devices, &prepared->visible_cuda_device_count,
      &prepared->device_enumeration_seconds,
      &prepared->primary_context_retain_seconds);
  if (status != CUDA_SUCCESS) {
    if (report_failure_details) {
      PrintCudaStageError("retain selected CUDA primary contexts for restore",
                          request.pid, status);
    }
    return {status, {}};
  }

  const auto manifest_validation_start = Clock::now();
  std::string manifest_error;
  if (!storage::ReadManifest(prepared->storage_dir, &prepared->manifest,
                             &manifest_error) ||
      !(request.pinned_storage_files.empty()
            ? (prepared->direct_storage_directory.get() >= 0
            ? storage::ValidateExtentFilesAt(
                  prepared->direct_storage_directory.get(),
                  prepared->manifest, &manifest_error)
            : storage::ValidateExtentFiles(prepared->storage_dir,
                                           prepared->manifest,
                                           &manifest_error))
            : ValidatePinnedStorageFiles(request, prepared->manifest,
                                         &manifest_error))) {
    if (report_failure_details) {
      std::fprintf(stderr, "custom storage manifest validation failed: %s\n",
                   manifest_error.c_str());
    }
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  prepared->manifest_validation_seconds =
      SecondsSince(manifest_validation_start);

  std::vector<CUcheckpointGpuPair> gpu_pairs;
  std::vector<storage::DevicePair> storage_pairs;
  const auto device_map_preparation_start = Clock::now();
  if (!ParseDeviceMap(request.device_map, &gpu_pairs, &storage_pairs)) {
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  prepared->device_map_preparation_seconds =
      SecondsSince(device_map_preparation_start);

  std::string identity_error;
  if (!daemon_protocol::ValidateProcessIdentity(request, ProcessRoot(),
                                                &identity_error)) {
    if (report_failure_details) {
      std::fprintf(stderr,
                   "process identity changed before CUDA operation: %s\n",
                   identity_error.c_str());
    }
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const auto cuda_process_api_start = Clock::now();
  CUcheckpointRestoreArgs args{};
  args.gpuPairs = gpu_pairs.empty() ? nullptr : gpu_pairs.data();
  args.gpuPairsCount = gpu_pairs.size();
  args.customStorageInfo_out = &prepared->info;
  if (cancellation != nullptr && cancellation->IsCancelled()) {
    if (report_failure_details) {
      std::fprintf(
          stderr,
          "restore batch cancelled before CUDA participant preparation\n");
    }
    return {CUDA_ERROR_OPERATING_SYSTEM, {}};
  }
  // DoCustomStorageRestoreBatch owns one process-wide job-file scope for this
  // complete group. Calling the driver directly here lets every rank and
  // auxiliary participant create its restore handle in captured order without
  // racing the process-wide CUDA_CHECKPOINT_JOB_FILE environment variable.
  status = cuCheckpointProcessRestore(request.pid, &args);
  prepared->cuda_process_api_seconds = SecondsSince(cuda_process_api_start);
  if (status != CUDA_SUCCESS) {
    if (report_failure_details)
      PrintRestoreFailureState(request.pid, status);
    return {status, {}};
  }

  try {
    const auto metadata_start = Clock::now();
    if (prepared->info == nullptr || prepared->info->handle == nullptr ||
        prepared->info->deviceCount >
            static_cast<unsigned int>(prepared->visible_cuda_device_count) ||
        (prepared->info->deviceCount > 0 &&
         prepared->info->perDeviceData == nullptr)) {
      if (report_failure_details) {
        std::fprintf(stderr,
                     "CUDA returned invalid custom storage information\n");
      }
      return FailPreparedRestore(prepared.get(), CUDA_ERROR_INVALID_VALUE,
                                 persistent_contexts);
    }
    std::string transfer_config_error;
    if (!transfer::CalculatePinnedBytes(
            prepared->info->deviceCount, transfer_options,
            &prepared->pinned_bytes, &transfer_config_error)) {
      if (report_failure_details) {
        std::fprintf(stderr,
                     "custom storage transfer configuration invalid: %s\n",
                     transfer_config_error.c_str());
      }
      return FailPreparedRestore(prepared.get(), CUDA_ERROR_INVALID_VALUE,
                                 persistent_contexts);
    }

    std::vector<CUcontext> contexts(prepared->info->deviceCount);
    std::vector<CUdevice> devices(prepared->info->deviceCount);
    std::vector<storage::DeviceExtent> device_extents;
    device_extents.reserve(prepared->info->deviceCount);
    for (unsigned int index = 0; index < prepared->info->deviceCount; ++index) {
      const auto discovery_start = Clock::now();
      status = prepared->operation_contexts->ContextAndDeviceForStream(
          prepared->info->perDeviceData[index].stream, &contexts[index],
          &devices[index]);
      prepared->target_context_discovery_seconds +=
          SecondsSince(discovery_start);
      if (status != CUDA_SUCCESS) {
        return FailPreparedRestore(prepared.get(), status, persistent_contexts);
      }
      std::string uuid;
      status = DeviceUUID(devices[index], &uuid);
      if (status != CUDA_SUCCESS) {
        return FailPreparedRestore(prepared.get(), status, persistent_contexts);
      }
      device_extents.push_back(
          {std::move(uuid), prepared->info->perDeviceData[index].size});
    }

    // NVML-negative coordinator processes receive the pod's full GPU set so
    // every possible restore stream can be resolved. Once CUDA reports the
    // actual streams, discard unused helper references before preparing the
    // next target in the batch. Otherwise a broad coordinator can retain every
    // GPU while narrow worker targets create their restore handles.
    const auto unused_context_release_start = Clock::now();
    status = prepared->operation_contexts->ReleaseUnused(devices);
    prepared->primary_context_release_seconds +=
        SecondsSince(unused_context_release_start);
    if (status != CUDA_SUCCESS) {
      return FailPreparedRestore(prepared.get(), status, persistent_contexts);
    }

    if (!storage::BuildTransferJobs(prepared->manifest, device_extents,
                                    storage_pairs, &prepared->transfer_jobs,
                                    &manifest_error)) {
      if (report_failure_details) {
        std::fprintf(stderr, "invalid restore custom storage mapping: %s\n",
                     manifest_error.c_str());
      }
      return FailPreparedRestore(prepared.get(), CUDA_ERROR_INVALID_VALUE,
                                 persistent_contexts);
    }
    for (const auto &extent : prepared->manifest) {
      if (extent.size >
          std::numeric_limits<size_t>::max() - prepared->total_bytes) {
        if (report_failure_details)
          std::fprintf(stderr, "custom storage byte count overflow\n");
        return FailPreparedRestore(prepared.get(), CUDA_ERROR_INVALID_VALUE,
                                   persistent_contexts);
      }
      prepared->total_bytes += extent.size;
    }

    size_t job_index = 0;
    prepared->scheduled_transfers.reserve(prepared->transfer_jobs.size());
    for (; job_index < prepared->transfer_jobs.size(); ++job_index) {
      const auto &job = prepared->transfer_jobs[job_index];
      const auto &device_data = prepared->info->perDeviceData[job.device_index];
      const auto &extent = prepared->manifest[job.extent_index];
      const auto *pinned = FindPinnedStorageFile(*prepared->request,
                                                 extent.filename);
      prepared->scheduled_transfers.push_back({
          .device_ptr = device_data.devPtr,
          .extent_size = device_data.size,
          .stream = device_data.stream,
          .context = contexts[job.device_index],
          .storage = {{{pinned != nullptr
                            ? std::filesystem::path(extent.filename)
                            : prepared->direct_storage_directory.get() >= 0
                            ? std::filesystem::path(
                                  extent.filename)
                            : prepared->storage_dir /
                                  extent.filename,
                        device_data.size,
                        prepared->direct_storage_directory.get(),
                        pinned == nullptr ? -1 : pinned->descriptor_fd,
                        pinned == nullptr ? 0 : pinned->device,
                        pinned == nullptr ? 0 : pinned->inode}},
                      {{0, device_data.size, 0, 0}}},
          .device_index = job.device_index,
      });
    }
    prepared->metadata_job_construction_seconds = SecondsSince(metadata_start);
    *prepared_out = std::move(prepared);
    return {CUDA_SUCCESS, {}, false};
  } catch (const std::exception &exception) {
    if (report_failure_details) {
      std::fprintf(stderr,
                   "custom storage restore setup failed after CUDA returned a "
                   "handle: %s\n",
                   exception.what());
    }
    return FailPreparedRestore(prepared.get(), CUDA_ERROR_OPERATING_SYSTEM,
                               persistent_contexts);
  } catch (...) {
    if (report_failure_details) {
      std::fprintf(stderr,
                   "custom storage restore setup failed after CUDA returned a "
                   "handle: unknown exception\n");
    }
    return FailPreparedRestore(prepared.get(), CUDA_ERROR_OPERATING_SYSTEM,
                               persistent_contexts);
  }
}

CustomStorageResult
DoCustomStorage(int pid, bool checkpoint, const std::string &device_map,
                const std::filesystem::path &storage_dir,
                const transfer::TransferOptions &transfer_options,
                Clock::time_point operation_deadline,
                Clock::time_point helper_main_start,
                OperationCompleteFn operation_complete,
                const daemon_protocol::Request *daemon_request,
                PersistentTargetContexts *persistent_contexts,
                transfer::TransferCancellation *cancellation) {
  const auto custom_storage_start = Clock::now();
  if (operation_complete == nullptr) {
    std::fprintf(stderr, "CUDA custom storage unavailable\n");
    return {CUDA_ERROR_NOT_SUPPORTED, {}};
  }
  const auto storage_directory_start = Clock::now();
  if (!storage_dir.is_absolute()) {
    std::fprintf(stderr, "custom storage directory must be absolute\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  if (checkpoint) {
    std::error_code filesystem_error;
    std::filesystem::create_directories(storage_dir, filesystem_error);
    struct stat directory_stat{};
    if (filesystem_error || lstat(storage_dir.c_str(), &directory_stat) != 0 ||
        !S_ISDIR(directory_stat.st_mode) ||
        chmod(storage_dir.c_str(), 0700) != 0) {
      std::fprintf(stderr, "failed to create custom storage directory\n");
      return {CUDA_ERROR_OPERATING_SYSTEM, {}};
    }
    std::string remove_error;
    if (!storage::RemoveManifest(storage_dir, &remove_error)) {
      std::fprintf(stderr,
                   "failed to clear stale custom storage manifest: %s\n",
                   remove_error.c_str());
      return {CUDA_ERROR_OPERATING_SYSTEM, {}};
    }
  } else {
    struct stat directory_stat{};
    if (lstat(storage_dir.c_str(), &directory_stat) != 0 ||
        !S_ISDIR(directory_stat.st_mode) ||
        (directory_stat.st_mode & 0022) != 0) {
      std::fprintf(stderr, "custom storage directory is missing or invalid\n");
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }
  OwnedDirectoryDescriptor direct_storage_directory;
  if (daemon_request != nullptr && daemon_request->storage_directory_fd >= 0) {
    if (checkpoint) {
      std::fprintf(stderr,
                   "descriptor-backed storage is restore-only\n");
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
    std::string direct_error;
    direct_storage_directory = OpenDirectStorageDirectory(
        daemon_request->storage_directory_fd,
        daemon_request->storage_relative_dir, &direct_error);
    if (direct_storage_directory.get() < 0) {
      std::fprintf(stderr, "%s\n", direct_error.c_str());
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  } else if (daemon_request != nullptr &&
             !daemon_request->storage_relative_dir.empty()) {
    std::fprintf(stderr,
                 "descriptor-backed storage path has no source descriptor\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const double storage_directory_validation_seconds =
      SecondsSince(storage_directory_start);

  int visible_cuda_device_count = 0;
  double device_enumeration_seconds = 0.0;
  double target_context_discovery_seconds = 0.0;
  double primary_context_retain_seconds = 0.0;
  double primary_context_release_seconds = 0.0;
  OperationContexts operation_contexts;
  std::vector<std::string> selected_devices;
  if (daemon_request != nullptr &&
      !ParseDeviceSelection(daemon_request->selected_devices,
                            &selected_devices)) {
    std::fprintf(stderr, "invalid selected CUDA devices\n");
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  CUresult status = CUDA_SUCCESS;
  // The CUDA checkpoint API requires the helper's primary contexts to exist
  // before it can construct and return the CustomStorage streams. Retain the
  // target's selected devices for this operation, narrow the set to the
  // returned streams below, and release all checkpoint references when the
  // operation completes. Restore references are instead transferred to the
  // identity-matched retained-context table.
  if (selected_devices.empty()) {
    status = operation_contexts.RetainAll(&visible_cuda_device_count,
                                          &device_enumeration_seconds,
                                          &primary_context_retain_seconds);
  } else {
    status = operation_contexts.RetainSelected(
        selected_devices, &visible_cuda_device_count,
        &device_enumeration_seconds, &primary_context_retain_seconds);
  }
  if (status != CUDA_SUCCESS) {
    PrintCudaStageError(checkpoint ? "retain CUDA primary contexts for checkpoint"
                                   : "retain CUDA primary contexts for restore",
                        pid, status);
    return {status, {}};
  }

  const auto manifest_validation_start = Clock::now();
  std::vector<storage::ManifestExtent> manifest;
  std::string manifest_error;
  if (!checkpoint &&
      (!storage::ReadManifest(storage_dir, &manifest, &manifest_error) ||
       !(daemon_request != nullptr &&
                 !daemon_request->pinned_storage_files.empty()
             ? ValidatePinnedStorageFiles(*daemon_request, manifest,
                                          &manifest_error)
             : direct_storage_directory.get() >= 0
             ? storage::ValidateExtentFilesAt(direct_storage_directory.get(),
                                              manifest, &manifest_error)
             : storage::ValidateExtentFiles(storage_dir, manifest,
                                            &manifest_error)))) {
    std::fprintf(stderr, "custom storage manifest validation failed: %s\n",
                 manifest_error.c_str());
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const double manifest_validation_seconds =
      SecondsSince(manifest_validation_start);

  CUcheckpointCustomStorageInfo *info = nullptr;
  std::vector<CUcheckpointGpuPair> gpu_pairs;
  std::vector<storage::DevicePair> storage_pairs;
  const auto device_map_preparation_start = Clock::now();
  if (!checkpoint && !ParseDeviceMap(device_map, &gpu_pairs, &storage_pairs)) {
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const double device_map_preparation_seconds =
      SecondsSince(device_map_preparation_start);
  if (daemon_request != nullptr) {
    std::string identity_error;
    if (!daemon_protocol::ValidateProcessIdentity(
            *daemon_request, ProcessRoot(), &identity_error)) {
      std::fprintf(stderr,
                   "process identity changed before CUDA operation: %s\n",
                   identity_error.c_str());
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }
  const auto cuda_process_api_start = Clock::now();
  status = InvokeWithCudaJobFile(
      daemon_request == nullptr ? std::string{} : daemon_request->job_file,
      daemon_request, cancellation,
      [&] {
        if (checkpoint) {
          CUcheckpointCheckpointArgs args{};
          args.customStorageInfo_out = &info;
          return cuCheckpointProcessCheckpoint(pid, &args);
        }
        CUcheckpointRestoreArgs args{};
        args.gpuPairs = gpu_pairs.empty() ? nullptr : gpu_pairs.data();
        args.gpuPairsCount = gpu_pairs.size();
        args.customStorageInfo_out = &info;
        return cuCheckpointProcessRestore(pid, &args);
      });
  const double cuda_process_api_seconds = SecondsSince(cuda_process_api_start);
  if (status != CUDA_SUCCESS) {
    if (checkpoint) {
      PrintCudaStageError("cuCheckpointProcessCheckpoint", pid, status);
    } else {
      PrintRestoreFailureState(pid, status);
    }
    return {status, {}};
  }
  daemon_protocol::OperationState operation{.handle_returned = true};
  const auto post_handle_failure = [&operation, &operation_contexts, checkpoint,
                                    daemon_request,
                                    persistent_contexts](CUresult failure) {
    const CUresult status =
        static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
            false, failure, [] { return static_cast<int32_t>(CUDA_SUCCESS); },
            &operation));
    const bool preserve_for_target = !checkpoint && daemon_request != nullptr &&
                                     persistent_contexts != nullptr;
    const CUresult release_status =
        preserve_for_target
            ? persistent_contexts->Adopt(&operation_contexts, *daemon_request)
            : operation_contexts.ReleaseAll();
    if (release_status != CUDA_SUCCESS) {
      std::fprintf(
          stderr,
          "failed to preserve operation CUDA contexts with status %d while "
          "handling status %d\n",
          static_cast<int>(release_status), static_cast<int>(status));
    }
    return CustomStorageResult{.status = status,
                               .operation = operation,
                               .fatal = operation.fatal() ||
                                        release_status != CUDA_SUCCESS};
  };
  try {
    const auto metadata_job_construction_start = Clock::now();
    if (info == nullptr || info->handle == nullptr ||
        info->deviceCount >
            static_cast<unsigned int>(visible_cuda_device_count) ||
        (info->deviceCount > 0 && info->perDeviceData == nullptr)) {
      std::fprintf(stderr,
                   "CUDA returned invalid custom storage information\n");
      return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
    }

    size_t pinned_bytes = 0;
    std::string transfer_config_error;
    if (!transfer::CalculatePinnedBytes(info->deviceCount, transfer_options,
                                        &pinned_bytes,
                                        &transfer_config_error)) {
      std::fprintf(stderr,
                   "custom storage transfer configuration invalid: %s\n",
                   transfer_config_error.c_str());
      return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
    }

    std::vector<CUcontext> contexts(info->deviceCount);
    std::vector<CUdevice> devices(info->deviceCount);
    std::vector<storage::DeviceExtent> device_extents;
    device_extents.reserve(info->deviceCount);
    for (unsigned int index = 0; index < info->deviceCount; ++index) {
      const auto target_context_discovery_start = Clock::now();
      status = checkpoint
                   ? operation_contexts.RetainForStream(
                         info->perDeviceData[index].stream, &contexts[index],
                         &devices[index], &primary_context_retain_seconds)
                   : operation_contexts.ContextAndDeviceForStream(
                         info->perDeviceData[index].stream, &contexts[index],
                         &devices[index]);
      target_context_discovery_seconds +=
          SecondsSince(target_context_discovery_start);
      if (status != CUDA_SUCCESS) {
        return post_handle_failure(status);
      }
      std::string uuid;
      status = DeviceUUID(devices[index], &uuid);
      if (status != CUDA_SUCCESS) {
        return post_handle_failure(status);
      }
      if (checkpoint && !selected_devices.empty() &&
          std::find(selected_devices.begin(), selected_devices.end(), uuid) ==
              selected_devices.end()) {
        return post_handle_failure(CUDA_ERROR_INVALID_DEVICE);
      }
      device_extents.push_back(
          {std::move(uuid), info->perDeviceData[index].size});
    }

    // NVML may omit CUDA coordinator processes even though the driver reports a
    // restore TID for them. The caller therefore supplies the pod's full GPU
    // set for those targets so every possible CustomStorage stream can be
    // resolved. Once the driver has returned the actual streams, release helper
    // primary contexts for devices that this target did not use. Retaining
    // those broad fallback contexts can make a later worker on the same device
    // fail its checkpoint while processing a multi-process tree.
    const auto unused_context_release_start = Clock::now();
    status = operation_contexts.ReleaseUnused(devices);
    primary_context_release_seconds +=
        SecondsSince(unused_context_release_start);
    if (status != CUDA_SUCCESS) {
      return post_handle_failure(status);
    }

    if (checkpoint && !storage::BuildCheckpointManifest(
                          device_extents, &manifest, &manifest_error)) {
      std::fprintf(stderr, "invalid checkpoint custom storage mapping: %s\n",
                   manifest_error.c_str());
      return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
    }
    std::vector<storage::TransferJob> transfer_jobs;
    if (!storage::BuildTransferJobs(
            manifest, device_extents,
            checkpoint ? std::vector<storage::DevicePair>{} : storage_pairs,
            &transfer_jobs, &manifest_error)) {
      std::fprintf(stderr, "invalid restore custom storage mapping: %s\n",
                   manifest_error.c_str());
      return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
    }

    size_t total_bytes = 0;
    for (const auto &extent : manifest) {
      if (extent.size > std::numeric_limits<size_t>::max() - total_bytes) {
        std::fprintf(stderr, "custom storage byte count overflow\n");
        return post_handle_failure(CUDA_ERROR_INVALID_VALUE);
      }
      total_bytes += extent.size;
    }
    const double metadata_job_construction_seconds =
        SecondsSince(metadata_job_construction_start);

    const auto start = Clock::now();
    std::vector<transfer::ScheduledTransfer> scheduled_transfers;
    size_t scheduling_job_index = 0;
    try {
      scheduled_transfers.reserve(transfer_jobs.size());
      for (; scheduling_job_index < transfer_jobs.size();
           ++scheduling_job_index) {
        const auto &job = transfer_jobs[scheduling_job_index];
        const auto &device_data = info->perDeviceData[job.device_index];
        const auto &extent = manifest[job.extent_index];
        const auto *pinned =
            daemon_request == nullptr
                ? nullptr
                : FindPinnedStorageFile(*daemon_request, extent.filename);
        scheduled_transfers.push_back({
            .device_ptr = device_data.devPtr,
            .extent_size = device_data.size,
            .stream = device_data.stream,
            .context = contexts[job.device_index],
            .storage = {{{pinned != nullptr
                              ? std::filesystem::path(extent.filename)
                              : direct_storage_directory.get() >= 0
                              ? std::filesystem::path(
                                    extent.filename)
                              : storage_dir / extent.filename,
                          device_data.size,
                          direct_storage_directory.get(),
                          pinned == nullptr ? -1 : pinned->descriptor_fd,
                          pinned == nullptr ? 0 : pinned->device,
                          pinned == nullptr ? 0 : pinned->inode}},
                        {{0, device_data.size, 0, 0}}},
            .device_index = job.device_index,
        });
      }
    } catch (const std::exception &exception) {
      const size_t device_index =
          scheduling_job_index < transfer_jobs.size()
              ? transfer_jobs[scheduling_job_index].device_index
              : scheduling_job_index;
      std::fprintf(stderr,
                   "custom storage transfer setup failed for device index %zu: "
                   "%s\n",
                   device_index, exception.what());
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    } catch (...) {
      const size_t device_index =
          scheduling_job_index < transfer_jobs.size()
              ? transfer_jobs[scheduling_job_index].device_index
              : scheduling_job_index;
      std::fprintf(stderr,
                   "custom storage transfer setup failed for device index %zu: "
                   "unknown exception\n",
                   device_index);
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
    transfer::TransferBatchResult transfer_result;
    if (!transfer::TransferBatch(scheduled_transfers,
                                 checkpoint
                                     ? transfer::TransferOperation::kCheckpoint
                                     : transfer::TransferOperation::kRestore,
                                 transfer_options, operation_deadline,
                                 &transfer_result, cancellation)) {
      std::fprintf(stderr, "%s\n", transfer_result.error.c_str());
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
    const double worker_orchestration_seconds =
        transfer_result.orchestration_seconds;

    size_t transferred_bytes = 0;
    double setup_service_seconds = 0.0;
    double pipeline_service_seconds = 0.0;
    double storage_service_seconds = 0.0;
    double cuda_wait_service_seconds = 0.0;
    double fsync_service_seconds = 0.0;
    double cleanup_service_seconds = 0.0;
    for (const auto &metrics : transfer_result.metrics) {
      if (metrics.bytes >
          std::numeric_limits<size_t>::max() - transferred_bytes) {
        std::fprintf(stderr,
                     "custom storage transferred byte count overflow\n");
        return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
      }
      transferred_bytes += metrics.bytes;
      setup_service_seconds += metrics.setup_seconds;
      pipeline_service_seconds += metrics.pipeline_seconds;
      storage_service_seconds += metrics.storage_seconds;
      cuda_wait_service_seconds += metrics.cuda_wait_seconds;
      fsync_service_seconds += metrics.fsync_seconds;
      cleanup_service_seconds += metrics.cleanup_seconds;
    }
    if (transferred_bytes != total_bytes) {
      std::fprintf(stderr,
                   "custom storage transfer coverage mismatch: transferred=%zu "
                   "expected=%zu\n",
                   transferred_bytes, total_bytes);
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }
    std::vector<std::string> extent_digests;
    extent_digests.reserve(transfer_result.metrics.size());
    for (const auto &metrics : transfer_result.metrics) {
      extent_digests.push_back(metrics.sha256);
    }
    if (!storage::ApplyOrVerifyExtentDigests(checkpoint, transfer_jobs,
                                             extent_digests, &manifest,
                                             &manifest_error)) {
      std::fprintf(stderr, "%s\n", manifest_error.c_str());
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }

    const auto post_transfer_validation_start = Clock::now();
    if (checkpoint) {
      if (!storage::ValidateExtentFiles(storage_dir, manifest,
                                        &manifest_error)) {
        std::fprintf(stderr, "custom storage extent validation failed: %s\n",
                     manifest_error.c_str());
        return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
      }
      if (!storage::WriteManifest(storage_dir, manifest, &manifest_error)) {
        std::fprintf(stderr, "custom storage manifest write failed: %s\n",
                     manifest_error.c_str());
        return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
      }
    }
    const double post_transfer_validation_seconds =
        SecondsSince(post_transfer_validation_start);

    if (cancellation != nullptr && cancellation->IsCancelled()) {
      std::fprintf(stderr, "custom storage operation cancelled before CUDA "
                           "acknowledgment\n");
      return post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
    }

    // This is the sole acknowledgment point; CUDA exposes no public abort for
    // failures above.
    const auto operation_complete_start = Clock::now();
    status = static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
        true, CUDA_SUCCESS,
        [operation_complete, info] {
          return static_cast<int32_t>(operation_complete(info->handle));
        },
        &operation));
    const double cuda_operation_complete_seconds =
        SecondsSince(operation_complete_start);
    if (status != CUDA_SUCCESS) {
      if (checkpoint &&
          !storage::RemoveManifest(storage_dir, &manifest_error)) {
        std::fprintf(stderr,
                     "failed to remove custom storage manifest after CUDA "
                     "completion failure: %s\n",
                     manifest_error.c_str());
      }
      return post_handle_failure(status);
    }

    // Preserve the original transfer interval: worker setup through CUDA
    // acknowledgment.
    const double seconds = SecondsSince(start);
    const double gib_per_second =
        seconds == 0.0 ? 0.0
                       : static_cast<double>(total_bytes) /
                             (1024.0 * 1024.0 * 1024.0) / seconds;
    const size_t retained_context_count = operation_contexts.size();
    const auto primary_context_release_start = Clock::now();
    const bool persist_for_target = !checkpoint && daemon_request != nullptr &&
                                    persistent_contexts != nullptr;
    const CUresult primary_context_release_status =
        persist_for_target
            ? persistent_contexts->Adopt(&operation_contexts, *daemon_request)
            : operation_contexts.ReleaseAll();
    primary_context_release_seconds +=
        SecondsSince(primary_context_release_start);
    const char *primary_context_release_state =
        persist_for_target ? "deferred_until_target_exit" : "completed";
    const char *context_lifecycle =
        persist_for_target ? "target_identity" : "invocation";
    const auto telemetry_end = Clock::now();
    const double custom_storage_total_seconds =
        SecondsBetween(custom_storage_start, telemetry_end);
    const double helper_main_to_telemetry_seconds =
        SecondsBetween(helper_main_start, telemetry_end);
    std::fprintf(
        stdout,
        "{\"event\":\"cuda_custom_storage_transfer\",\"schema_version\":1,"
        "\"operation\":\"%s\",\"devices\":%zu,\"bytes\":%zu,"
        "\"duration_seconds\":%.6f,\"effective_gib_per_second\":%.6f,"
        "\"transfer_buffer_count\":%zu,\"transfer_chunk_bytes\":%zu,"
        "\"pinned_bytes\":%zu,\"setup_service_seconds\":%.6f,"
        "\"pipeline_service_seconds\":%.6f,\"storage_service_seconds\":%.6f,"
        "\"cuda_wait_service_seconds\":%.6f,\"fsync_service_seconds\":%.6f,"
        "\"cleanup_service_seconds\":%.6f,"
        "\"timing_scope\":\"monotonic_wall;totals_contain_subphases;"
        "service_seconds_are_cross_worker_sums_and_may_overlap\","
        "\"helper_main_to_telemetry_seconds\":%.6f,"
        "\"custom_storage_total_seconds\":%.6f,"
        "\"storage_directory_validation_seconds\":%.6f,"
        "\"cuda_device_count\":%d,"
        "\"retained_context_count\":%zu,"
        "\"device_enumeration_seconds\":%.6f,"
        "\"target_context_discovery_seconds\":%.6f,"
        "\"primary_context_retain_seconds\":%.6f,"
        "\"manifest_validation_seconds\":%.6f,"
        "\"device_map_preparation_seconds\":%.6f,"
        "\"cuda_process_api_seconds\":%.6f,"
        "\"metadata_job_construction_seconds\":%.6f,"
        "\"worker_orchestration_seconds\":%.6f,"
        "\"post_transfer_validation_seconds\":%.6f,"
        "\"cuda_operation_complete_seconds\":%.6f,"
        "\"primary_context_release_seconds\":%.6f,"
        "\"primary_context_release_state\":\"%s\","
        "\"primary_context_release_success\":%s,"
        "\"primary_context_release_status\":%d,"
        "\"context_lifecycle\":\"%s\"}\n",
        checkpoint ? "checkpoint" : "restore", manifest.size(), total_bytes,
        seconds, gib_per_second, transfer_options.buffer_count,
        transfer_options.chunk_bytes, pinned_bytes, setup_service_seconds,
        pipeline_service_seconds, storage_service_seconds,
        cuda_wait_service_seconds, fsync_service_seconds,
        cleanup_service_seconds, helper_main_to_telemetry_seconds,
        custom_storage_total_seconds, storage_directory_validation_seconds,
        visible_cuda_device_count, retained_context_count,
        device_enumeration_seconds, target_context_discovery_seconds,
        primary_context_retain_seconds, manifest_validation_seconds,
        device_map_preparation_seconds, cuda_process_api_seconds,
        metadata_job_construction_seconds, worker_orchestration_seconds,
        post_transfer_validation_seconds, cuda_operation_complete_seconds,
        primary_context_release_seconds, primary_context_release_state,
        primary_context_release_status == CUDA_SUCCESS ? "true" : "false",
        static_cast<int>(primary_context_release_status), context_lifecycle);
    if (primary_context_release_status != CUDA_SUCCESS) {
      std::fprintf(stderr,
                   "warning: retained CUDA primary context release failed with "
                   "status %d "
                   "after operation acknowledgment\n",
                   static_cast<int>(primary_context_release_status));
    }
    if (primary_context_release_status != CUDA_SUCCESS) {
      return {.status = primary_context_release_status,
              .operation = operation,
              .fatal = true};
    }
    return {CUDA_SUCCESS, operation, false};
  } catch (const std::exception &exception) {
    std::fprintf(stderr,
                 "custom storage operation failed after CUDA returned a "
                 "handle: %s\n",
                 exception.what());
  } catch (...) {
    std::fprintf(stderr,
                 "custom storage operation failed after CUDA returned a "
                 "handle: unknown exception\n");
  }
  CustomStorageResult failure =
      post_handle_failure(CUDA_ERROR_OPERATING_SYSTEM);
  failure.fatal = true;
  return failure;
}

CustomStorageResult DoCustomStorageRestoreBatch(
    const std::vector<daemon_protocol::Request> &requests,
    Clock::time_point operation_deadline,
    Clock::time_point operation_dispatch_start,
    OperationCompleteFn operation_complete,
    PersistentTargetContexts *persistent_contexts,
    transfer::TransferCancellation *cancellation) {
  const auto batch_total_start = Clock::now();
  RestoreBatchFailureTelemetry failure_telemetry(requests.size(),
                                                 batch_total_start);
  if (requests.size() < 2 ||
      requests.size() > kMaxRestoreBatchTargets ||
      operation_complete == nullptr ||
      persistent_contexts == nullptr) {
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  const transfer::TransferOptions options{
      .buffer_count = requests.front().transfer_buffer_count,
      .chunk_bytes = static_cast<size_t>(requests.front().transfer_chunk_bytes),
  };
  std::string validation_error;
  if (!transfer::ValidateTransferOptions(options, &validation_error)) {
    std::fprintf(stderr, "invalid transfer configuration: %s\n",
                 validation_error.c_str());
    failure_telemetry.Fail("transfer_configuration",
                           CUDA_ERROR_INVALID_VALUE);
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  for (const auto &request : requests) {
    if (request.action != daemon_protocol::Action::kRestore ||
        request.backend != daemon_protocol::Backend::kPosix ||
        request.transfer_buffer_count != options.buffer_count ||
        request.transfer_chunk_bytes != options.chunk_bytes) {
      std::fprintf(stderr,
                   "restore batch targets must use identical POSIX transfer "
                   "configuration\n");
      failure_telemetry.Fail("request_validation", CUDA_ERROR_INVALID_VALUE,
                             request.pid);
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }

  std::vector<std::unique_ptr<PreparedCustomStorageRestore>> prepared(
      requests.size());
  PreparedRestoreContextsGuard prepared_contexts_guard(&prepared,
                                                       persistent_contexts);
  std::vector<std::string> job_files;
  job_files.reserve(requests.size());
  for (const auto &request : requests)
    job_files.push_back(request.job_file);
  std::vector<RestoreBatchJobFileGroup> job_file_groups;
  std::string job_file_error;
  const auto resolve_expected_job_file =
      [&requests](const std::string &path,
                  RestoreBatchJobFileIdentity *identity,
                  std::string *error) {
        if (!ResolveRestoreBatchJobFileIdentity(path, identity, error))
          return false;
        const auto request = std::find_if(
            requests.begin(), requests.end(),
            [&](const daemon_protocol::Request &candidate) {
              return candidate.job_file == path;
            });
        if (request == requests.end() ||
            !MatchesExpectedJobFileIdentity(*request, *identity)) {
          *error = "CUDA job-file identity changed after PageBroker grouping";
          return false;
        }
        return true;
      };
  if (!GroupRestoreBatchJobFiles(job_files, resolve_expected_job_file,
                                 &job_file_groups, &job_file_error)) {
    failure_telemetry.SetGroupCount(job_file_groups.size());
    failure_telemetry.Fail("job_file_identity", CUDA_ERROR_INVALID_VALUE);
    std::fprintf(stderr, "restore batch job-file validation failed: %s\n",
                 job_file_error.c_str());
    return {CUDA_ERROR_INVALID_VALUE, {}};
  }
  failure_telemetry.SetGroupCount(job_file_groups.size());

  std::vector<PinnedJobFile> pinned_job_files(job_file_groups.size());
  for (size_t group_index = 0; group_index < job_file_groups.size();
       ++group_index) {
    if (!PinnedJobFile::Open(job_file_groups[group_index],
                             &pinned_job_files[group_index],
                             &job_file_error)) {
      const size_t request_index =
          job_file_groups[group_index].request_indices.front();
      failure_telemetry.Fail("job_file_pin", CUDA_ERROR_INVALID_VALUE,
                             requests[request_index].pid);
      std::fprintf(stderr, "restore batch job-file pin failed: %s\n",
                   job_file_error.c_str());
      return {CUDA_ERROR_INVALID_VALUE, {}};
    }
  }

  // One CUDA restore preparation may be in flight for a launch job. The
  // prepared extents are still submitted together to TransferBatch below.
  constexpr size_t preparation_parallelism_limit = 1;

  std::vector<CustomStorageResult> preparation_results(requests.size());
  constexpr size_t kMaxLoggedParticipantFailures = 8;
  size_t preparation_parallelism_observed = 0;
  for (size_t job_group_index = 0;
       job_group_index < job_file_groups.size(); ++job_group_index) {
    const auto &group = job_file_groups[job_group_index];
    size_t first_exception_group_index = group.request_indices.size();
    size_t group_preparation_width = 0;
    CUresult scope_status = CUDA_SUCCESS;
    try {
      scope_status = InvokeWithCudaJobFile(
          group.representative, nullptr, cancellation,
          [&] {
            RunRestoreBatchPreparation(
                group.request_indices.size(),
                [&](size_t group_index) {
                  const size_t request_index =
                      group.request_indices[group_index];
                  preparation_results[request_index] =
                      PrepareCustomStorageRestore(
                          requests[request_index], options, operation_complete,
                          persistent_contexts, cancellation,
                          request_index < kMaxLoggedParticipantFailures,
                          &prepared[request_index]);
                },
                &first_exception_group_index, &group_preparation_width);
            preparation_parallelism_observed =
                std::max(preparation_parallelism_observed,
                         group_preparation_width);
            failure_telemetry.ObservePreparationWidth(group_preparation_width);
            return CUDA_SUCCESS;
          },
          &pinned_job_files[job_group_index]);
    } catch (const std::exception &exception) {
      failure_telemetry.ObservePreparationWidth(group_preparation_width);
      const size_t request_index =
          first_exception_group_index < group.request_indices.size()
              ? group.request_indices[first_exception_group_index]
              : group.request_indices.front();
      failure_telemetry.Fail("participant_preparation_exception",
                             CUDA_ERROR_OPERATING_SYSTEM,
                             requests[request_index].pid);
      std::fprintf(stderr,
                   "restore batch participant preparation threw for pid %u: "
                   "%s\n",
                   requests[request_index].pid, exception.what());
      for (auto &target : prepared) {
        if (target != nullptr)
          (void)FailPreparedRestore(target.get(),
                                    CUDA_ERROR_OPERATING_SYSTEM,
                                    persistent_contexts);
      }
      return {.status = CUDA_ERROR_OPERATING_SYSTEM,
              .operation = {},
              .fatal = true};
    } catch (...) {
      failure_telemetry.ObservePreparationWidth(group_preparation_width);
      const size_t request_index =
          first_exception_group_index < group.request_indices.size()
              ? group.request_indices[first_exception_group_index]
              : group.request_indices.front();
      failure_telemetry.Fail("participant_preparation_exception",
                             CUDA_ERROR_OPERATING_SYSTEM,
                             requests[request_index].pid);
      std::fprintf(stderr,
                   "restore batch participant preparation threw for pid %u\n",
                   requests[request_index].pid);
      for (auto &target : prepared) {
        if (target != nullptr)
          (void)FailPreparedRestore(target.get(),
                                    CUDA_ERROR_OPERATING_SYSTEM,
                                    persistent_contexts);
      }
      return {.status = CUDA_ERROR_OPERATING_SYSTEM,
              .operation = {},
              .fatal = true};
    }
    if (scope_status != CUDA_SUCCESS) {
      const size_t request_index = group.request_indices.front();
      failure_telemetry.Fail("job_file_dispatch", scope_status,
                             requests[request_index].pid);
      bool fatal = false;
      for (auto &target : prepared) {
        if (target == nullptr)
          continue;
        const CustomStorageResult cleanup = FailPreparedRestore(
            target.get(), scope_status, persistent_contexts);
        fatal = fatal || cleanup.fatal || cleanup.operation.fatal();
      }
      return {.status = scope_status, .operation = {}, .fatal = fatal};
    }
    size_t failed_request = requests.size();
    size_t failed_count = 0;
    for (const size_t request_index : group.request_indices) {
      const auto &participant = preparation_results[request_index];
      if (participant.status == CUDA_SUCCESS)
        continue;
      if (failed_request == requests.size())
        failed_request = request_index;
      if (failed_count < kMaxLoggedParticipantFailures) {
        std::fprintf(stderr,
                     "restore batch participant preparation failed for pid "
                     "%u with CUDA status %d\n",
                     requests[request_index].pid,
                     static_cast<int>(participant.status));
      }
      ++failed_count;
    }
    if (failed_count > kMaxLoggedParticipantFailures) {
      std::fprintf(stderr,
                   "restore batch participant preparation omitted %zu "
                   "additional failures\n",
                   failed_count - kMaxLoggedParticipantFailures);
    }
    if (failed_request != requests.size()) {
      const auto &result = preparation_results[failed_request];
      failure_telemetry.Fail(
          cancellation != nullptr && cancellation->IsCancelled()
              ? "participant_preparation_cancelled"
              : "participant_preparation",
          result.status, requests[failed_request].pid);
      bool fatal = false;
      for (const size_t request_index : group.request_indices) {
        const auto &participant = preparation_results[request_index];
        if (participant.status != CUDA_SUCCESS) {
          fatal = fatal || participant.fatal || participant.operation.fatal();
        }
      }
      for (auto &target : prepared) {
        if (target == nullptr)
          continue;
        const CustomStorageResult cleanup = FailPreparedRestore(
            target.get(), result.status, persistent_contexts);
        fatal = fatal || cleanup.fatal || cleanup.operation.fatal();
      }
      return {.status = result.status,
              .operation = result.operation,
              .fatal = fatal};
    }
  }
  failure_telemetry.FinishPreparation();
  const double prepare_wall_seconds = SecondsSince(batch_total_start);

  std::vector<transfer::ScheduledTransfer> transfers;
  size_t total_bytes = 0;
  std::vector<size_t> target_pinned_bytes;
  target_pinned_bytes.reserve(prepared.size());
  try {
    for (const auto &target : prepared) {
      if (target->total_bytes >
          std::numeric_limits<size_t>::max() - total_bytes) {
        throw std::overflow_error("restore batch payload byte count overflow");
      }
      total_bytes += target->total_bytes;
      target_pinned_bytes.push_back(target->pinned_bytes);
      transfers.insert(transfers.end(), target->scheduled_transfers.begin(),
                       target->scheduled_transfers.end());
    }
  } catch (const std::exception &exception) {
    std::fprintf(stderr, "restore batch construction failed: %s\n",
                 exception.what());
    failure_telemetry.Fail("batch_construction", CUDA_ERROR_OPERATING_SYSTEM);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                persistent_contexts);
    }
    return {
        .status = CUDA_ERROR_OPERATING_SYSTEM, .operation = {}, .fatal = true};
  }
  size_t total_pinned_bytes = 0;
  std::string pinned_memory_error;
  if (!transfer::CalculateBatchPinnedBytes(
          target_pinned_bytes, &total_pinned_bytes, &pinned_memory_error)) {
    std::fprintf(stderr, "restore batch configuration invalid: %s\n",
                 pinned_memory_error.c_str());
    failure_telemetry.Fail("pinned_memory_admission", CUDA_ERROR_OUT_OF_MEMORY);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OUT_OF_MEMORY,
                                persistent_contexts);
    }
    return {.status = CUDA_ERROR_OUT_OF_MEMORY, .operation = {}, .fatal = true};
  }

  const auto batch_transfer_start = Clock::now();
  transfer::TransferBatchResult transfer_result;
  if (!transfer::TransferBatch(transfers, transfer::TransferOperation::kRestore,
                               options, operation_deadline, &transfer_result,
                               cancellation)) {
    std::fprintf(stderr, "%s\n", transfer_result.error.c_str());
    failure_telemetry.Fail(
        cancellation != nullptr && cancellation->IsCancelled()
            ? "transfer_cancelled"
            : "transfer",
        CUDA_ERROR_OPERATING_SYSTEM);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                persistent_contexts);
    }
    return {
        .status = CUDA_ERROR_OPERATING_SYSTEM, .operation = {}, .fatal = true};
  }
  const double batch_transfer_seconds = SecondsSince(batch_transfer_start);
  if (transfer_result.metrics.size() != transfers.size()) {
    std::fprintf(stderr,
                 "restore batch returned an invalid transfer metric count\n");
    failure_telemetry.Fail("transfer_metrics", CUDA_ERROR_OPERATING_SYSTEM);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                persistent_contexts);
    }
    return {
        .status = CUDA_ERROR_OPERATING_SYSTEM, .operation = {}, .fatal = true};
  }

  size_t metric_offset = 0;
  for (auto &target : prepared) {
    std::vector<std::string> extent_digests;
    extent_digests.reserve(target->transfer_jobs.size());
    for (size_t index = 0; index < target->transfer_jobs.size(); ++index) {
      extent_digests.push_back(
          transfer_result.metrics[metric_offset + index].sha256);
    }
    metric_offset += target->transfer_jobs.size();
    std::string digest_error;
    if (!storage::ApplyOrVerifyExtentDigests(false, target->transfer_jobs,
                                             extent_digests, &target->manifest,
                                             &digest_error)) {
      std::fprintf(stderr,
                   "restore batch extent digest verification failed for pid "
                   "%u: %s\n",
                   target->request->pid, digest_error.c_str());
      failure_telemetry.Fail("extent_digest_verification",
                             CUDA_ERROR_OPERATING_SYSTEM,
                             target->request->pid);
      for (auto &remaining : prepared) {
        (void)FailPreparedRestore(remaining.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                  persistent_contexts);
      }
      return {.status = CUDA_ERROR_OPERATING_SYSTEM,
              .operation = target->operation,
              .fatal = true};
    }
  }

  size_t transferred_bytes = 0;
  double storage_service_seconds = 0.0;
  double cuda_wait_service_seconds = 0.0;
  double storage_directory_validation_service_seconds = 0.0;
  double device_enumeration_service_seconds = 0.0;
  double primary_context_retain_service_seconds = 0.0;
  double primary_context_release_service_seconds = 0.0;
  double manifest_validation_service_seconds = 0.0;
  double device_map_preparation_service_seconds = 0.0;
  double cuda_process_api_service_seconds = 0.0;
  double target_context_discovery_service_seconds = 0.0;
  double metadata_job_construction_service_seconds = 0.0;
  for (const auto &target : prepared) {
    storage_directory_validation_service_seconds +=
        target->storage_directory_validation_seconds;
    device_enumeration_service_seconds += target->device_enumeration_seconds;
    primary_context_retain_service_seconds +=
        target->primary_context_retain_seconds;
    primary_context_release_service_seconds +=
        target->primary_context_release_seconds;
    manifest_validation_service_seconds += target->manifest_validation_seconds;
    device_map_preparation_service_seconds +=
        target->device_map_preparation_seconds;
    cuda_process_api_service_seconds += target->cuda_process_api_seconds;
    target_context_discovery_service_seconds +=
        target->target_context_discovery_seconds;
    metadata_job_construction_service_seconds +=
        target->metadata_job_construction_seconds;
  }
  for (const auto &metrics : transfer_result.metrics) {
    if (metrics.bytes >
        std::numeric_limits<size_t>::max() - transferred_bytes) {
      std::fprintf(stderr, "restore batch transferred byte count overflow\n");
      failure_telemetry.Fail("transfer_byte_accounting",
                             CUDA_ERROR_OPERATING_SYSTEM);
      for (auto &target : prepared) {
        (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                  persistent_contexts);
      }
      return {.status = CUDA_ERROR_OPERATING_SYSTEM,
              .operation = {},
              .fatal = true};
    }
    transferred_bytes += metrics.bytes;
    storage_service_seconds += metrics.storage_seconds;
    cuda_wait_service_seconds += metrics.cuda_wait_seconds;
  }
  if (transferred_bytes != total_bytes) {
    std::fprintf(stderr,
                 "restore batch transfer coverage mismatch: transferred=%zu "
                 "expected=%zu\n",
                 transferred_bytes, total_bytes);
    failure_telemetry.Fail("transfer_coverage", CUDA_ERROR_OPERATING_SYSTEM);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                persistent_contexts);
    }
    return {
        .status = CUDA_ERROR_OPERATING_SYSTEM, .operation = {}, .fatal = true};
  }

  const auto operation_complete_start = Clock::now();
  if (cancellation != nullptr && cancellation->IsCancelled()) {
    std::fprintf(stderr,
                 "restore batch cancelled before CUDA acknowledgment\n");
    failure_telemetry.Fail("completion_cancelled",
                           CUDA_ERROR_OPERATING_SYSTEM);
    for (auto &target : prepared) {
      (void)FailPreparedRestore(target.get(), CUDA_ERROR_OPERATING_SYSTEM,
                                persistent_contexts);
    }
    return {
        .status = CUDA_ERROR_OPERATING_SYSTEM, .operation = {}, .fatal = true};
  }
  // Completing a CUDA operation resumes its target. The request order follows
  // the captured process tree (parents before children), so release the tree in
  // reverse order. In particular, TRT-LLM ranks may reap their compile-worker
  // children as soon as the rank resumes; completing the parent first would
  // make the later child completion fail with CUDA_ERROR_OPERATING_SYSTEM.
  for (size_t completed = 0; completed < prepared.size(); ++completed) {
    auto &target =
        prepared[RestoreBatchCompletionIndex(completed, prepared.size())];
    const CUresult status =
        static_cast<CUresult>(daemon_protocol::FinishHandledOperation(
            true, CUDA_SUCCESS,
            [operation_complete, info = target->info] {
              return static_cast<int32_t>(operation_complete(info->handle));
            },
            &target->operation));
    if (status != CUDA_SUCCESS) {
      std::fprintf(stderr,
                   "restore batch CUDA operation completion failed for pid "
                   "%u\n",
                   target->request->pid);
      failure_telemetry.Fail("operation_complete", status,
                             target->request->pid);
      for (auto &remaining : prepared) {
        (void)persistent_contexts->Adopt(remaining->operation_contexts.get(),
                                         *remaining->request);
      }
      return {.status = status, .operation = target->operation, .fatal = true};
    }
  }
  const double operation_complete_service_seconds =
      SecondsSince(operation_complete_start);

  const auto context_adopt_start = Clock::now();
  CUresult context_adopt_status = CUDA_SUCCESS;
  std::uint32_t context_adopt_failed_pid = 0;
  daemon_protocol::OperationState context_adopt_operation;
  for (auto &target : prepared) {
    const CUresult status = persistent_contexts->Adopt(
        target->operation_contexts.get(), *target->request);
    if (status != CUDA_SUCCESS) {
      std::fprintf(stderr,
                   "failed to retain restored target CUDA contexts for pid "
                   "%u\n",
                   target->request->pid);
      if (context_adopt_status == CUDA_SUCCESS) {
        context_adopt_status = status;
        context_adopt_operation = target->operation;
        context_adopt_failed_pid = target->request->pid;
      }
    }
  }
  if (context_adopt_status != CUDA_SUCCESS) {
    failure_telemetry.Fail("context_adopt", context_adopt_status,
                           context_adopt_failed_pid);
    return {.status = context_adopt_status,
            .operation = context_adopt_operation,
            .fatal = true};
  }
  const double context_adopt_service_seconds =
      SecondsSince(context_adopt_start);
  const auto telemetry_end = Clock::now();
  const double batch_total_seconds =
      SecondsBetween(batch_total_start, telemetry_end);
  const double operation_dispatch_to_telemetry_seconds =
      SecondsBetween(operation_dispatch_start, telemetry_end);

  const double gib_per_second = batch_transfer_seconds == 0.0
                                    ? 0.0
                                    : static_cast<double>(total_bytes) /
                                          (1024.0 * 1024.0 * 1024.0) /
                                          batch_transfer_seconds;
  std::fprintf(
      stdout,
      "{\"event\":\"cuda_custom_storage_restore_batch\","
      "\"schema_version\":1,\"targets\":%zu,\"transfer_jobs\":%zu,"
      "\"bytes\":%zu,\"duration_seconds\":%.6f,"
      "\"effective_gib_per_second\":%.6f,"
      "\"transfer_buffer_count\":%zu,\"transfer_chunk_bytes\":%zu,"
      "\"pinned_bytes\":%zu,\"storage_service_seconds\":%.6f,"
      "\"cuda_wait_service_seconds\":%.6f,"
      "\"worker_orchestration_seconds\":%.6f,"
      "\"prepare_wall_seconds\":%.6f,\"prepare_job_file_groups\":%zu,"
      "\"prepare_parallelism_limit\":%zu,"
      "\"prepare_parallelism\":%zu,\"batch_total_seconds\":%.6f,"
      "\"operation_dispatch_to_telemetry_seconds\":%.6f,"
      "\"storage_directory_validation_service_seconds\":%.6f,"
      "\"device_enumeration_service_seconds\":%.6f,"
      "\"primary_context_retain_service_seconds\":%.6f,"
      "\"primary_context_release_service_seconds\":%.6f,"
      "\"manifest_validation_service_seconds\":%.6f,"
      "\"device_map_preparation_service_seconds\":%.6f,"
      "\"cuda_process_api_service_seconds\":%.6f,"
      "\"target_context_discovery_service_seconds\":%.6f,"
      "\"metadata_job_construction_service_seconds\":%.6f,"
      "\"operation_complete_service_seconds\":%.6f,"
      "\"context_adopt_service_seconds\":%.6f,"
      "\"context_lifecycle\":\"target_identity\"}\n",
      prepared.size(), transfers.size(), total_bytes, batch_transfer_seconds,
      gib_per_second, options.buffer_count, options.chunk_bytes,
      total_pinned_bytes, storage_service_seconds, cuda_wait_service_seconds,
      transfer_result.orchestration_seconds, prepare_wall_seconds,
      job_file_groups.size(), preparation_parallelism_limit,
      preparation_parallelism_observed, batch_total_seconds,
      operation_dispatch_to_telemetry_seconds,
      storage_directory_validation_service_seconds,
      device_enumeration_service_seconds,
      primary_context_retain_service_seconds,
      primary_context_release_service_seconds,
      manifest_validation_service_seconds,
      device_map_preparation_service_seconds, cuda_process_api_service_seconds,
      target_context_discovery_service_seconds,
      metadata_job_construction_service_seconds,
      operation_complete_service_seconds, context_adopt_service_seconds);
  failure_telemetry.MarkSuccess();
  return {CUDA_SUCCESS, {}, false};
}

CUresult DoRegularCheckpoint(int pid) {
  CUcheckpointCheckpointArgs args{};
  return cuCheckpointProcessCheckpoint(pid, &args);
}

CUresult DoLegacyRestore(int pid, const std::string &device_map) {
  std::vector<CUcheckpointGpuPair> pairs;
  if (!ParseDeviceMap(device_map, &pairs)) {
    return CUDA_ERROR_INVALID_VALUE;
  }
  CUcheckpointRestoreArgs args{};
  args.gpuPairs = pairs.empty() ? nullptr : pairs.data();
  args.gpuPairsCount = pairs.size();
  return cuCheckpointProcessRestore(pid, &args);
}

bool RestoreOrCloseCapturedDescriptor(int saved_fd, int target_fd) {
  if (saved_fd < 0) {
    return false;
  }
  const bool restore_failed = dup2(saved_fd, target_fd) < 0;
  close(saved_fd);
  if (!restore_failed) {
    return false;
  }

  const int null_fd = open("/dev/null", O_WRONLY | O_CLOEXEC);
  if (null_fd < 0 || dup2(null_fd, target_fd) < 0) {
    // Closing the descriptor is the last-resort guarantee that the capture
    // pipe no longer has a live writer before Finish joins its reader.
    (void)close(target_fd);
  }
  if (null_fd >= 0) {
    close(null_fd);
  }
  return true;
}

daemon_protocol::Response
RunDaemonOperation(const daemon_protocol::Request &request,
                   OperationCompleteFn operation_complete,
                   std::chrono::seconds max_operation_duration,
                   PersistentTargetContexts *persistent_contexts,
                   bool capture_output,
                   transfer::TransferCancellation *cancellation) {
  daemon_protocol::Response response;
  // Until the driver lock call begins, every failure leaves the source process
  // running. Clear this immediately before the call so callers only treat
  // failures that are known to precede CUDA mutation as safe to leave alive.
  if (request.action == daemon_protocol::Action::kLock) {
    response.flags |= daemon_protocol::kResponseLockNotAcquired;
  }
  std::string reap_error;
  const CUresult release_status =
      persistent_contexts->ReapExited(ProcessRoot(), &reap_error);
  if (release_status != CUDA_SUCCESS) {
    response.cuda_status = release_status;
    response.flags |= daemon_protocol::kResponseFatal;
    response.error =
        "failed to release CUDA primary contexts for an exited target";
    return response;
  }
  if (!reap_error.empty()) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.error = reap_error + "; retained contexts and deferred operation";
    return response;
  }
  constexpr size_t kPerStreamCaptureLimit =
      daemon_protocol::kMaxResponseSize / 2 - 256;
  daemon_protocol::BoundedOutputCapture output_capture(kPerStreamCaptureLimit);
  daemon_protocol::BoundedOutputCapture error_capture(kPerStreamCaptureLimit);
  int saved_stdout = -1;
  int saved_stderr = -1;
  if (capture_output) {
    std::string capture_setup_error;
    if (!output_capture.Start(&capture_setup_error) ||
        !error_capture.Start(&capture_setup_error)) {
      response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
      response.flags |= daemon_protocol::kResponseFatal;
      response.error = capture_setup_error;
      return response;
    }
    (void)std::fflush(stdout);
    (void)std::fflush(stderr);
    saved_stdout = dup(STDOUT_FILENO);
    saved_stderr = dup(STDERR_FILENO);
    if (saved_stdout < 0 || saved_stderr < 0 ||
        dup2(output_capture.write_fd(), STDOUT_FILENO) < 0 ||
        dup2(error_capture.write_fd(), STDERR_FILENO) < 0) {
      response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
      response.flags |= daemon_protocol::kResponseFatal;
      response.error = "failed to redirect daemon operation output";
    }
  }
  if (response.error.empty()) {
    if (request.backend == daemon_protocol::Backend::kPosix &&
        operation_complete == nullptr) {
      response.cuda_status = CUDA_ERROR_NOT_SUPPORTED;
      std::fprintf(
          stderr,
          "CUDA POSIX CustomStorage backend requested but the CUDA 13.4 "
          "driver API or transfer adapter is unavailable\n");
    } else if (request.action == daemon_protocol::Action::kLock ||
               request.action == daemon_protocol::Action::kUnlock) {
      std::string identity_error;
      if (!daemon_protocol::ValidateProcessIdentity(request, ProcessRoot(),
                                                    &identity_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(
            stderr, "process identity changed immediately before CUDA %s: %s\n",
            daemon_protocol::ActionName(request.action),
            identity_error.c_str());
      } else if (request.action == daemon_protocol::Action::kLock) {
        CUcheckpointLockArgs lock_args{};
        std::string timeout_error;
        if (!daemon_protocol::OperationTimeoutMilliseconds(
                max_operation_duration, &lock_args.timeoutMs, &timeout_error)) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          response.flags |= daemon_protocol::kResponseFatal;
          std::fprintf(stderr, "%s\n", timeout_error.c_str());
        } else {
          response.cuda_status = InvokeWithCudaJobFile(
              request.job_file, &request, cancellation, [&] {
                // This is the first instruction that can mutate the source.
                // Keep the pre-dispatch classification through admission,
                // job-file serialization, cancellation, and identity checks.
                response.flags &= ~daemon_protocol::kResponseLockNotAcquired;
                return cuCheckpointProcessLock(request.pid, &lock_args);
              });
          if (response.cuda_status == CUDA_ERROR_NOT_READY) {
            // CUDA guarantees a timed-out lock leaves the process RUNNING.
            response.flags |= daemon_protocol::kResponseLockNotAcquired;
          }
        }
      } else {
        CUcheckpointUnlockArgs args{};
        response.cuda_status = InvokeWithCudaJobFile(
            request.job_file, &request, cancellation, [&] {
              return cuCheckpointProcessUnlock(request.pid, &args);
            });
      }
    } else if (request.action == daemon_protocol::Action::kRestoreBatch) {
      const auto operation_start = Clock::now();
      if (max_operation_duration > Clock::time_point::max() - operation_start) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        response.flags |= daemon_protocol::kResponseFatal;
        std::fprintf(stderr,
                     "configured operation duration exceeds the steady clock "
                     "range\n");
      } else {
        const CustomStorageResult result = DoCustomStorageRestoreBatch(
            request.targets, operation_start + max_operation_duration,
            operation_start, operation_complete, persistent_contexts,
            cancellation);
        response.cuda_status = result.status;
        if (result.operation.fatal() || result.fatal) {
          response.flags |= daemon_protocol::kResponseFatal;
        }
      }
    } else if (request.backend == daemon_protocol::Backend::kRegular) {
      std::string identity_error;
      if (!daemon_protocol::ValidateProcessIdentity(request, ProcessRoot(),
                                                    &identity_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(stderr,
                     "process identity changed immediately before regular "
                     "CUDA %s: %s\n",
                     daemon_protocol::ActionName(request.action),
                     identity_error.c_str());
      } else if (request.action == daemon_protocol::Action::kCheckpoint) {
        response.cuda_status = InvokeWithCudaJobFile(
            request.job_file, &request, cancellation,
            [&] { return DoRegularCheckpoint(request.pid); });
      } else {
        response.cuda_status = InvokeWithCudaJobFile(
            request.job_file, &request, cancellation, [&] {
              return DoLegacyRestore(request.pid, request.device_map);
            });
      }
    } else {
      transfer::TransferOptions options{
          .buffer_count = request.transfer_buffer_count,
          .chunk_bytes = static_cast<size_t>(request.transfer_chunk_bytes),
      };
      std::string validation_error;
      if (!transfer::ValidateTransferOptions(options, &validation_error)) {
        response.cuda_status = CUDA_ERROR_INVALID_VALUE;
        std::fprintf(stderr, "invalid transfer configuration: %s\n",
                     validation_error.c_str());
      } else {
        const auto operation_start = Clock::now();
        if (max_operation_duration >
            Clock::time_point::max() - operation_start) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          std::fprintf(stderr,
                       "configured operation duration exceeds the steady "
                       "clock range\n");
        } else {
          const CustomStorageResult result = DoCustomStorage(
              request.pid,
              request.action == daemon_protocol::Action::kCheckpoint,
              request.device_map, request.storage_dir, options,
              operation_start + max_operation_duration, operation_start,
              operation_complete, &request, persistent_contexts, cancellation);
          response.cuda_status = result.status;
          if (result.operation.fatal() || result.fatal) {
            response.flags |= daemon_protocol::kResponseFatal;
          }
        }
      }
    }
    if (response.cuda_status != CUDA_SUCCESS) {
      PrintCudaError(static_cast<CUresult>(response.cuda_status));
    }
  }
  if (!capture_output) {
    return response;
  }
  (void)std::fflush(stdout);
  (void)std::fflush(stderr);
  const bool stdout_restore_failed =
      RestoreOrCloseCapturedDescriptor(saved_stdout, STDOUT_FILENO);
  const bool stderr_restore_failed =
      RestoreOrCloseCapturedDescriptor(saved_stderr, STDERR_FILENO);
  const bool output_restore_failed =
      stdout_restore_failed || stderr_restore_failed;
  if (output_restore_failed) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
  }
  bool output_truncated = false;
  bool error_truncated = false;
  std::string captured_output;
  std::string captured_error;
  std::string output_capture_error;
  std::string error_capture_error;
  const bool output_finished = output_capture.Finish(
      &captured_output, &output_truncated, &output_capture_error);
  const bool error_finished = error_capture.Finish(
      &captured_error, &error_truncated, &error_capture_error);
  if (!output_finished || !error_finished) {
    response.cuda_status = CUDA_ERROR_OPERATING_SYSTEM;
    response.flags |= daemon_protocol::kResponseFatal;
    for (const std::string *capture_error :
         {&output_capture_error, &error_capture_error}) {
      if (capture_error->empty()) {
        continue;
      }
      if (!response.error.empty()) {
        response.error += '\n';
      }
      response.error += *capture_error;
    }
  }
  if (!captured_output.empty()) {
    if (!response.output.empty()) {
      response.output += '\n';
    }
    response.output += captured_output;
  }
  if (!captured_error.empty()) {
    if (!response.error.empty()) {
      response.error += '\n';
    }
    response.error += captured_error;
  }
  if (output_truncated) {
    response.output += "\n[stdout truncated at daemon response limit]\n";
  }
  if (error_truncated) {
    response.error += "\n[stderr truncated at daemon response limit]\n";
  }
  if (output_restore_failed) {
    if (!response.error.empty()) {
      response.error += '\n';
    }
    response.error += "failed to restore daemon output descriptors";
  }
  return response;
}

} // namespace

class Service::Impl {
  struct ActiveOperation;

public:
  explicit Impl(std::chrono::seconds max_operation_duration)
      : max_operation_duration_(max_operation_duration) {}

  bool Initialize(InitializationMetrics *metrics, std::string *error) {
    if (metrics == nullptr || error == nullptr) {
      return false;
    }
    const auto init_start = Clock::now();
    CUresult status = cuInit(0);
    metrics->cuda_init_seconds = SecondsSince(init_start);
    if (status != CUDA_SUCCESS) {
      *error = CudaError(status);
      return false;
    }

    const auto enumeration_start = Clock::now();
    status = cuDeviceGetCount(&metrics->cuda_device_count);
    metrics->device_enumeration_seconds = SecondsSince(enumeration_start);
    if (status != CUDA_SUCCESS) {
      *error = CudaError(status);
      return false;
    }
    (void)cuDriverGetVersion(&metrics->cuda_driver_version);
    operation_complete_ =
        ResolveOperationComplete(&metrics->custom_storage_driver_api_available);
    metrics->custom_storage_transfer_backend_available =
        transfer::TransferBackendAvailable();
    metrics->custom_storage_available =
        metrics->custom_storage_driver_api_available &&
        metrics->custom_storage_transfer_backend_available;
    if (!metrics->custom_storage_available) {
      operation_complete_ = nullptr;
    }
    initialized_ = true;
    return true;
  }

  daemon_protocol::Response Execute(const daemon_protocol::Request &request) {
    return ExecuteOperation(request, true);
  }

  daemon_protocol::Response
  ExecuteUncaptured(const daemon_protocol::Request &request) {
    daemon_protocol::Response response = ExecuteOperation(request, false);
    if (response.cuda_status != CUDA_SUCCESS && response.error.empty()) {
      response.error = CudaError(static_cast<CUresult>(response.cuda_status));
    }
    return response;
  }

  CUresult BeginShutdown(const std::string &proc_root,
                         std::string *identity_error) {
    shutting_down_.store(true, std::memory_order_release);
    std::vector<std::shared_ptr<ActiveOperation>> active;
    {
      std::lock_guard lock(active_operations_mutex_);
      active = active_operations_;
    }
    std::vector<daemon_protocol::Request> targets;
    for (const auto &operation : active) {
      operation->cancellation->Cancel();
      if (operation->restored_workload) {
        targets.insert(targets.end(), operation->targets.begin(),
                       operation->targets.end());
      }
    }
    if (targets.empty()) {
      identity_error->clear();
      return CUDA_SUCCESS;
    }
    if (!daemon_protocol::TerminateMatchingProcesses(
            targets, proc_root, std::chrono::seconds(5), identity_error)) {
      return CUDA_ERROR_OPERATING_SYSTEM;
    }
    return CUDA_SUCCESS;
  }

  CUresult ReapExited(const std::string &proc_root,
                      std::string *identity_error) {
    return persistent_contexts_.ReapExited(proc_root, identity_error);
  }

  CUresult TerminateRetainedTargets(const std::string &proc_root,
                                    std::string *identity_error) {
    return persistent_contexts_.TerminateAll(proc_root, identity_error);
  }

  CUresult ReleaseAll() { return persistent_contexts_.ReleaseAll(); }

private:
  struct ActiveOperation {
    std::shared_ptr<transfer::TransferCancellation> cancellation;
    std::vector<daemon_protocol::Request> targets;
    // A checkpoint source belongs to the caller and must survive PageBroker
    // shutdown. Only restore and restore-batch operations create workloads
    // PageBroker may terminate before releasing retained restore contexts.
    bool restored_workload = false;
  };

  daemon_protocol::Response
  ExecuteOperation(const daemon_protocol::Request &request,
                   bool capture_output) {
    if (!initialized_) {
      return {.cuda_status = CUDA_ERROR_NOT_INITIALIZED,
              .flags = 0,
              .output = {},
              .error = "CUDA operation service is not initialized"};
    }
    const auto now = Clock::now();
    auto operation = std::make_shared<ActiveOperation>();
    operation->cancellation = std::make_shared<transfer::TransferCancellation>(
        now + max_operation_duration_);
    operation->targets =
        request.action == daemon_protocol::Action::kRestoreBatch
            ? request.targets
            : std::vector<daemon_protocol::Request>{request};
    operation->restored_workload =
        request.action == daemon_protocol::Action::kRestore ||
        request.action == daemon_protocol::Action::kRestoreBatch;
    {
      std::lock_guard lock(active_operations_mutex_);
      if (shutting_down_.load(std::memory_order_acquire)) {
        return {.cuda_status = CUDA_ERROR_OPERATING_SYSTEM,
                .flags = daemon_protocol::kResponseFatal,
                .output = {},
                .error = "CUDA operation service is shutting down"};
      }
      active_operations_.push_back(operation);
    }
    daemon_protocol::Response response;
    try {
      response = RunDaemonOperation(
          request, operation_complete_, max_operation_duration_,
          &persistent_contexts_, capture_output, operation->cancellation.get());
    } catch (...) {
      RemoveActiveOperation(operation);
      throw;
    }
    RemoveActiveOperation(operation);
    return response;
  }

  void
  RemoveActiveOperation(const std::shared_ptr<ActiveOperation> &operation) {
    std::lock_guard lock(active_operations_mutex_);
    const auto item = std::find(active_operations_.begin(),
                                active_operations_.end(), operation);
    if (item != active_operations_.end()) {
      active_operations_.erase(item);
    }
  }

  static std::string CudaError(CUresult status) {
    const char *name = nullptr;
    const char *message = nullptr;
    (void)cuGetErrorName(status, &name);
    (void)cuGetErrorString(status, &message);
    return std::string(name == nullptr ? "CUDA_ERROR_UNKNOWN" : name) + ": " +
           (message == nullptr ? "unknown CUDA error" : message);
  }

  std::chrono::seconds max_operation_duration_;
  OperationCompleteFn operation_complete_ = nullptr;
  PersistentTargetContexts persistent_contexts_;
  std::mutex active_operations_mutex_;
  std::vector<std::shared_ptr<ActiveOperation>> active_operations_;
  std::atomic<bool> shutting_down_{false};
  bool initialized_ = false;
};

Service::Service(std::chrono::seconds max_operation_duration)
    : impl_(std::make_unique<Impl>(max_operation_duration)) {}

Service::~Service() = default;

bool Service::Initialize(InitializationMetrics *metrics, std::string *error) {
  return impl_->Initialize(metrics, error);
}

daemon_protocol::Response
Service::Execute(const daemon_protocol::Request &request) {
  return impl_->Execute(request);
}

daemon_protocol::Response
Service::ExecuteUncaptured(const daemon_protocol::Request &request) {
  return impl_->ExecuteUncaptured(request);
}

CUresult Service::BeginShutdown(const std::string &proc_root,
                                std::string *identity_error) {
  return impl_->BeginShutdown(proc_root, identity_error);
}

CUresult Service::ReapExited(const std::string &proc_root,
                             std::string *identity_error) {
  return impl_->ReapExited(proc_root, identity_error);
}

CUresult Service::TerminateRetainedTargets(const std::string &proc_root,
                                           std::string *identity_error) {
  return impl_->TerminateRetainedTargets(proc_root, identity_error);
}

CUresult Service::ReleaseAll() { return impl_->ReleaseAll(); }

} // namespace cuda_checkpoint_operation
