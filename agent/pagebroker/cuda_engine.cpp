// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "cuda_engine.hpp"

#include <fcntl.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <exception>
#include <fstream>
#include <future>
#include <limits>
#include <mutex>
#include <semaphore>
#include <set>
#include <stdexcept>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include "cuda_operation.h"
#include "cuda_worker_pool.hpp"
#include "cuinterpose_coordinator.hpp"
#include "daemon_protocol.h"
#include "restore_batch_order.h"
#include "transfer_config.h"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

namespace cuda_daemon = cuda_checkpoint_daemon;
namespace cuda_operation = cuda_checkpoint_operation;
namespace cuda_transfer = cuda_checkpoint_transfer;

constexpr std::string_view kCustomStorageDirectory = "cuda-custom-storage";
constexpr std::string_view kProcessDirectoryPrefix = "process-nspid-";
constexpr std::string_view kCudaJobFileName = "cuda-checkpoint-job";
constexpr std::string_view kCudaJobFilePath =
    "snapshot-control/cuda-checkpoint-job";
constexpr std::string_view kDefaultProcRoot = "/proc";
constexpr std::string_view kCuinterposeCoordinator =
    "/usr/local/bin/cuinterpose-coordinator";
constexpr size_t kMaximumFailureMessage = 4096;
constexpr std::ptrdiff_t kMaximumParallelRestores = 1024;

class RestorePermit {
public:
  explicit RestorePermit(
      std::counting_semaphore<kMaximumParallelRestores> &slots)
      : slots_(slots), acquired_(slots_.try_acquire()) {}
  RestorePermit(const RestorePermit &) = delete;
  RestorePermit &operator=(const RestorePermit &) = delete;
  ~RestorePermit() {
    if (acquired_)
      slots_.release();
  }
  explicit operator bool() const { return acquired_; }

private:
  std::counting_semaphore<kMaximumParallelRestores> &slots_;
  bool acquired_ = false;
};

// Admissions cross PageBroker RPC boundaries: BeginCheckpoint/BeginRestore
// and the corresponding CUDA command can run on different connection
// threads. std::shared_mutex ownership is thread-affine, so an RAII lock
// acquired by the Begin RPC cannot safely be released by the CUDA RPC. Keep
// the same checkpoint-exclusive/restore-shared policy with state protected by
// an ordinary mutex; releasing this lease is intentionally thread-neutral.
class OperationAdmissionGate {
public:
  bool TryAcquireCheckpoint() {
    std::lock_guard lock(mutex_);
    if (checkpoint_active_ || active_restores_ != 0)
      return false;
    checkpoint_active_ = true;
    return true;
  }

  bool TryAcquireRestore() {
    std::lock_guard lock(mutex_);
    if (checkpoint_active_)
      return false;
    ++active_restores_;
    return true;
  }

  void ReleaseCheckpoint() noexcept {
    std::lock_guard lock(mutex_);
    if (!checkpoint_active_)
      std::terminate();
    checkpoint_active_ = false;
  }

  void ReleaseRestore() noexcept {
    std::lock_guard lock(mutex_);
    if (active_restores_ == 0)
      std::terminate();
    --active_restores_;
  }

private:
  std::mutex mutex_;
  size_t active_restores_ = 0;
  bool checkpoint_active_ = false;
};

class ServiceRestoreAdmission final : public RestoreAdmission {
public:
  ServiceRestoreAdmission(
      std::counting_semaphore<kMaximumParallelRestores> &slots,
      OperationAdmissionGate &operation_gate,
      cuda_daemon::Backend backend, size_t target_count,
      size_t expected_dispatch_group_count)
      : permit_(slots), operation_gate_(&operation_gate),
        backend_(backend), target_count_(target_count),
        expected_dispatch_group_count_(expected_dispatch_group_count) {
    if (permit_)
      operation_acquired_ = operation_gate_->TryAcquireRestore();
  }

  ~ServiceRestoreAdmission() override {
    if (operation_acquired_)
      operation_gate_->ReleaseRestore();
  }

  explicit operator bool() const {
    return static_cast<bool>(permit_) && operation_acquired_;
  }

  void SetWorkerLease(std::unique_ptr<CudaRestoreWorkerLease> lease) {
    worker_lease_ = std::move(lease);
  }
  CudaRestoreWorkerLease *worker_lease() const { return worker_lease_.get(); }
  cuda_daemon::Backend backend() const { return backend_; }
  size_t target_count() const { return target_count_; }
  size_t expected_dispatch_group_count() const {
    return expected_dispatch_group_count_;
  }

private:
  RestorePermit permit_;
  OperationAdmissionGate *operation_gate_;
  bool operation_acquired_ = false;
  cuda_daemon::Backend backend_;
  size_t target_count_;
  size_t expected_dispatch_group_count_;
  std::unique_ptr<CudaRestoreWorkerLease> worker_lease_;
};

class ServiceCheckpointAdmission final : public CheckpointAdmission {
public:
  ServiceCheckpointAdmission(OperationAdmissionGate &operation_gate,
                             cuda_daemon::Backend backend,
                             size_t target_count)
      : operation_gate_(&operation_gate),
        acquired_(operation_gate_->TryAcquireCheckpoint()), backend_(backend),
        target_count_(target_count) {}

  ~ServiceCheckpointAdmission() override {
    if (acquired_)
      operation_gate_->ReleaseCheckpoint();
  }

  explicit operator bool() const { return acquired_; }
  cuda_daemon::Backend backend() const { return backend_; }
  size_t target_count() const { return target_count_; }

private:
  OperationAdmissionGate *operation_gate_;
  bool acquired_ = false;
  cuda_daemon::Backend backend_;
  size_t target_count_;
};

class PoolRestoreWorkerLease final : public CudaRestoreWorkerLease {
public:
  explicit PoolRestoreWorkerLease(CudaWorkerPool::Lease lease)
      : lease_(std::move(lease)) {}

  size_t size() const override { return lease_.workers().size(); }

  CudaWorkerRpcResult
  Call(size_t worker_index,
       const cuda_daemon::Request &request) const override {
    return lease_.Call(worker_index, request);
  }

  void Poison(size_t worker_index) override { lease_.Poison(worker_index); }

  std::shared_ptr<CudaRestoreWorkerRetention>
  Retain(size_t worker_index) override;

private:
  CudaWorkerPool::Lease lease_;
};

class PoolRestoreWorkerRetention final : public CudaRestoreWorkerRetention {
public:
  explicit PoolRestoreWorkerRetention(std::shared_ptr<void> retention)
      : retention_(std::move(retention)) {}

private:
  std::shared_ptr<void> retention_;
};

std::shared_ptr<CudaRestoreWorkerRetention>
PoolRestoreWorkerLease::Retain(size_t worker_index) {
  auto retention = lease_.Retain(worker_index);
  if (retention == nullptr)
    return nullptr;
  return std::make_shared<PoolRestoreWorkerRetention>(std::move(retention));
}

class PoolRestoreWorkerPool final : public CudaRestoreWorkerPool {
public:
  explicit PoolRestoreWorkerPool(CudaWorkerPoolConfig config)
      : capacity_(config.worker_count), pool_(std::move(config)) {
    std::string error;
    if (!pool_.Start(&error))
      throw std::runtime_error("start PageBroker CUDA worker pool: " + error);
  }

  size_t capacity() const override { return capacity_; }

  std::unique_ptr<CudaRestoreWorkerLease>
  TryAcquire(size_t weight) override {
    auto lease = pool_.TryAcquire(weight);
    if (!lease)
      return nullptr;
    return std::make_unique<PoolRestoreWorkerLease>(std::move(*lease));
  }

  bool fail_stop_required() const override {
    return pool_.fail_stop_required();
  }

  bool Shutdown(std::chrono::milliseconds timeout,
                std::string *error) override {
    return pool_.Shutdown(timeout, error);
  }

private:
  size_t capacity_;
  CudaWorkerPool pool_;
};

class InProcessRestoreWorkerLease final : public CudaRestoreWorkerLease {
public:
  InProcessRestoreWorkerLease(cuda_operation::Service *service, size_t size)
      : service_(service), size_(size) {}

  size_t size() const override { return size_; }

  CudaWorkerRpcResult
  Call(size_t worker_index,
       const cuda_daemon::Request &request) const override {
    if (worker_index >= size_)
      return {.status = CudaWorkerRpcStatus::kInvalidRequest,
              .error = "CUDA test worker lease index is out of range"};
    return {.status = CudaWorkerRpcStatus::kOk,
            .response = service_->ExecuteUncaptured(request)};
  }

  void Poison(size_t) override {}

  std::shared_ptr<CudaRestoreWorkerRetention> Retain(size_t) override {
    class Retention final : public CudaRestoreWorkerRetention {};
    return std::make_shared<Retention>();
  }

private:
  cuda_operation::Service *service_;
  size_t size_;
};

class InProcessRestoreWorkerPool final : public CudaRestoreWorkerPool {
public:
  explicit InProcessRestoreWorkerPool(cuda_operation::Service *service)
      : service_(service) {}

  size_t capacity() const override {
    return cuda_daemon::kMaxRestoreBatchTargets;
  }

  std::unique_ptr<CudaRestoreWorkerLease>
  TryAcquire(size_t weight) override {
    return std::make_unique<InProcessRestoreWorkerLease>(service_, weight);
  }

  bool fail_stop_required() const override { return false; }

  bool Shutdown(std::chrono::milliseconds, std::string *error) override {
    error->clear();
    return true;
  }

private:
  cuda_operation::Service *service_;
};

class SystemCudaPinnedTarget final : public CudaPinnedTarget {
public:
  explicit SystemCudaPinnedTarget(
      std::shared_ptr<cuda_daemon::PinnedProcess> process)
      : process_(std::move(process)) {}

  uint32_t pid() const override { return process_->request().pid; }
  const std::shared_ptr<cuda_daemon::PinnedProcess> &process() const {
    return process_;
  }

private:
  std::shared_ptr<cuda_daemon::PinnedProcess> process_;
};

class SystemCudaTargetTerminator final : public CudaTargetTerminator {
public:
  bool Pin(const std::vector<cuda_daemon::Request> &targets,
           const std::string &process_root, CudaPinnedTargets *pinned,
           std::string *error) override {
    std::vector<std::shared_ptr<cuda_daemon::PinnedProcess>> processes;
    if (!cuda_daemon::PinMatchingProcesses(targets, process_root, &processes,
                                           error))
      return false;
    pinned->clear();
    pinned->reserve(processes.size());
    for (auto &process : processes)
      pinned->push_back(
          std::make_shared<SystemCudaPinnedTarget>(std::move(process)));
    return true;
  }

  bool Terminate(const CudaPinnedTargets &targets,
                 std::chrono::milliseconds timeout,
                 std::string *error) override {
    std::vector<std::shared_ptr<cuda_daemon::PinnedProcess>> processes;
    processes.reserve(targets.size());
    for (const auto &target : targets) {
      const auto pinned =
          std::dynamic_pointer_cast<SystemCudaPinnedTarget>(target);
      if (pinned == nullptr) {
        *error = "CUDA target was not pinned by the system terminator";
        return false;
      }
      processes.push_back(pinned->process());
    }
    return cuda_daemon::TerminatePinnedProcesses(processes, timeout, error);
  }

  bool Exited(const CudaPinnedTarget &target, bool *exited,
              std::string *error) override {
    const auto *pinned = dynamic_cast<const SystemCudaPinnedTarget *>(&target);
    if (pinned == nullptr) {
      *error = "CUDA target was not pinned by the system terminator";
      return false;
    }
    return cuda_daemon::InspectPinnedProcess(*pinned->process(), exited, error);
  }
};

std::string JoinSelectedDevices(const CudaProcessTarget &target) {
  std::string devices;
  for (const auto &device : target.selected_devices()) {
    if (!devices.empty())
      devices += ',';
    devices += device;
  }
  return devices;
}

std::string BoundedMessage(const cuda_daemon::Response &response) {
  std::string message = response.error;
  if (!response.output.empty()) {
    if (!message.empty())
      message += "; ";
    message += response.output;
  }
  if (message.empty())
    message = "CUDA operation failed with status " +
              std::to_string(response.cuda_status);
  if (message.size() > kMaximumFailureMessage)
    message.resize(kMaximumFailureMessage);
  return message;
}

std::string BoundedWorkerMessage(const CudaWorkerRpcResult &result) {
  if (result.status == CudaWorkerRpcStatus::kOk)
    return BoundedMessage(result.response);
  std::string message = result.error;
  if (message.empty())
    message = "CUDA worker RPC failed";
  if (message.size() > kMaximumFailureMessage)
    message.resize(kMaximumFailureMessage);
  return message;
}

void EmitWorkerOutput(size_t target_index,
                      const cuda_daemon::Request &target,
                      std::string_view output) {
  const std::string bounded(output.substr(0, kMaximumFailureMessage));
  std::fprintf(stdout,
               "{\"event\":\"pagebroker_cuda_worker_output\","
               "\"schema_version\":1,\"target_index\":%zu,\"pid\":%u,"
               "\"start_time_ticks\":%llu,\"output\":%s,"
               "\"truncated\":%s}\n",
               target_index, target.pid,
               static_cast<unsigned long long>(target.expected_start_time_ticks),
               cuda_transfer::JsonEscape(bounded).c_str(),
               output.size() > bounded.size() ? "true" : "false");
  std::fflush(stdout);
}

std::string CudaError(CUresult status) {
  const char *name = nullptr;
  const char *message = nullptr;
  (void)cuGetErrorName(status, &name);
  (void)cuGetErrorString(status, &message);
  if (name != nullptr && message != nullptr)
    return std::string(name) + ": " + message;
  if (name != nullptr)
    return name;
  if (message != nullptr)
    return message;
  return "CUDA error " + std::to_string(static_cast<int>(status));
}

void ValidateTarget(const CudaProcessTarget &target,
                    cuda_daemon::Backend backend) {
  if (!target.has_host_pid() || target.host_pid() == 0 ||
      !target.has_namespace_pid() || target.namespace_pid() == 0 ||
      !target.has_start_time_ticks() || target.start_time_ticks() == 0 ||
      !target.has_cgroup() || target.cgroup().empty())
    throw std::invalid_argument("CUDA target requires host PID, namespace PID, "
                                "start time, and cgroup identity");
  if (target.cgroup().size() > cuda_daemon::kMaxCgroupSize)
    throw std::invalid_argument("CUDA target cgroup identity is too long");
  if (backend == cuda_daemon::Backend::kPosix &&
      target.selected_devices().empty())
    throw std::invalid_argument(
        "CUDA target requires at least one selected GPU");
  for (const auto &device : target.selected_devices()) {
    if (device.empty() || device.find(',') != std::string::npos)
      throw std::invalid_argument(
          "CUDA selected GPU must be a nonempty UUID without commas");
  }
}

template <typename RequestType>
cuda_daemon::Backend ValidateRequest(const RequestType &request) {
  if (!request.has_storage_backend())
    throw std::invalid_argument("CUDA operation requires a storage backend");
  cuda_daemon::Backend backend;
  switch (request.storage_backend()) {
  case v1::CUDA_STORAGE_BACKEND_REGULAR:
    backend = cuda_daemon::Backend::kRegular;
    break;
  case v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE:
    backend = cuda_daemon::Backend::kPosix;
    break;
  default:
    throw std::invalid_argument(
        "CUDA operation has an unsupported storage backend");
  }
  if (request.targets().empty() ||
      request.targets_size() >
          static_cast<int>(cuda_daemon::kMaxRestoreBatchTargets))
    throw std::invalid_argument(
        "CUDA operation requires between 1 and 64 targets");
  std::set<uint32_t> host_pids;
  std::set<uint32_t> namespace_pids;
  for (const auto &target : request.targets()) {
    ValidateTarget(target, backend);
    if (!host_pids.insert(target.host_pid()).second)
      throw std::invalid_argument(
          "CUDA operation contains a duplicate host PID");
    if (!namespace_pids.insert(target.namespace_pid()).second)
      throw std::invalid_argument(
          "CUDA operation contains a duplicate namespace PID");
  }
  return backend;
}

fs::path StorageDirectory(const fs::path &staging_directory,
                          uint32_t namespace_pid) {
  return staging_directory / kCustomStorageDirectory /
         (std::string(kProcessDirectoryPrefix) + std::to_string(namespace_pid));
}

fs::path ConfigureProcessRoot() {
  const char *configured = std::getenv("CUDA_CHECKPOINT_PROC_ROOT");
  if (configured == nullptr || configured[0] == '\0') {
    if (setenv("CUDA_CHECKPOINT_PROC_ROOT", kDefaultProcRoot.data(), 1) != 0)
      throw std::runtime_error("configure PageBroker CUDA process root: " +
                               std::string(std::strerror(errno)));
    configured = kDefaultProcRoot.data();
  }
  const fs::path root(configured);
  if (!root.is_absolute())
    throw std::runtime_error("PageBroker CUDA process root must be absolute");
  return root;
}

fs::path JobFileForTarget(const fs::path &process_root, uint32_t host_pid) {
  return process_root / std::to_string(host_pid) / "root" / kCudaJobFilePath;
}

std::vector<CuinterposeTarget>
CoordinatorTargets(const google::protobuf::RepeatedPtrField<CudaProcessTarget> &targets) {
  std::vector<CuinterposeTarget> result;
  result.reserve(targets.size());
  for (const auto &target : targets)
    result.push_back({.host_pid = target.host_pid(),
                      .namespace_pid = target.namespace_pid()});
  return result;
}

bool ValidateIdentities(const std::vector<cuda_daemon::Request> &targets,
                        const fs::path &process_root, std::string *error) {
  for (const auto &target : targets) {
    std::string identity_error;
    if (!cuda_daemon::ValidateProcessIdentity(target, process_root.string(),
                                              &identity_error)) {
      *error = "CUDA target " + std::to_string(target.pid) +
               " identity changed before cuinterpose: " + identity_error;
      return false;
    }
  }
  error->clear();
  return true;
}

cuda_daemon::Request BuildRequest(const CudaProcessTarget &target,
                                  cuda_daemon::Action action,
                                  cuda_daemon::Backend backend,
                                  const fs::path &staging_directory,
                                  const fs::path &job_file,
                                  const DirectRestoreProcess *direct_process =
                                      nullptr) {
  cuda_daemon::Request request;
  request.action = action;
  request.backend = backend;
  request.pid = target.host_pid();
  request.expected_start_time_ticks = target.start_time_ticks();
  request.expected_cgroup = target.cgroup();
  request.job_file = job_file.string();
  if (action == cuda_daemon::Action::kLock ||
      action == cuda_daemon::Action::kUnlock)
    return request;
  request.device_map = target.device_map();
  if (backend == cuda_daemon::Backend::kPosix) {
    request.transfer_buffer_count = cuda_transfer::kDefaultBufferCount;
    request.transfer_chunk_bytes = cuda_transfer::kDefaultChunkBytes;
    request.storage_dir =
        StorageDirectory(staging_directory, target.namespace_pid()).string();
    if (direct_process != nullptr) {
      if (direct_process->namespace_pid != target.namespace_pid())
        throw std::invalid_argument(
            "direct restore process descriptor does not match target");
      request.pinned_storage_files.reserve(direct_process->carriers.size());
      for (const auto &carrier : direct_process->carriers) {
        request.pinned_storage_files.push_back({
            .filename = carrier.filename,
            .descriptor_fd = carrier.descriptor.get(),
            .size = carrier.size,
            .device = carrier.device,
            .inode = carrier.inode});
      }
    }
    request.selected_devices = JoinSelectedDevices(target);
  }
  return request;
}

cuda_daemon::Request
BuildControlRequest(const cuda_daemon::Request &target,
                    cuda_daemon::Action action) {
  if (action != cuda_daemon::Action::kLock &&
      action != cuda_daemon::Action::kUnlock)
    throw std::invalid_argument("CUDA control request must lock or unlock");
  return {.action = action,
          .backend = target.backend,
          .pid = target.pid,
          .expected_start_time_ticks = target.expected_start_time_ticks,
          .expected_cgroup = target.expected_cgroup,
          .job_file = target.job_file,
          .expected_job_file_device = target.expected_job_file_device,
          .expected_job_file_inode = target.expected_job_file_inode};
}

bool CopyAll(int source, int destination, std::string *error) {
  std::vector<char> buffer(1 << 20);
  while (true) {
    ssize_t bytes;
    do {
      bytes = read(source, buffer.data(), buffer.size());
    } while (bytes < 0 && errno == EINTR);
    if (bytes < 0) {
      *error = "read live CUDA job file: " + std::string(std::strerror(errno));
      return false;
    }
    if (bytes == 0)
      return true;
    size_t offset = 0;
    while (offset < static_cast<size_t>(bytes)) {
      ssize_t written;
      do {
        written = write(destination, buffer.data() + offset,
                        static_cast<size_t>(bytes) - offset);
      } while (written < 0 && errno == EINTR);
      if (written <= 0) {
        *error =
            "write staged CUDA job file: " +
            std::string(written < 0 ? std::strerror(errno) : "no progress");
        return false;
      }
      offset += static_cast<size_t>(written);
    }
  }
}

bool RefreshJobFile(const fs::path &live_job_file,
                    const fs::path &staging_directory, std::string *error) {
  if (live_job_file.empty())
    return true;
  const fs::path output = staging_directory / kCudaJobFileName;
  const fs::path temporary =
      staging_directory / (std::string(kCudaJobFileName) + ".pagebroker.tmp");
  const int source =
      open(live_job_file.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (source < 0) {
    *error = "open live CUDA job file: " + std::string(std::strerror(errno));
    return false;
  }
  const int destination =
      open(temporary.c_str(),
           O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC | O_NOFOLLOW, 0600);
  if (destination < 0) {
    *error = "open staged CUDA job file: " + std::string(std::strerror(errno));
    close(source);
    return false;
  }
  const bool copied = CopyAll(source, destination, error);
  const bool synced = copied && fsync(destination) == 0;
  if (copied && !synced)
    *error = "sync staged CUDA job file: " + std::string(std::strerror(errno));
  const bool source_closed = close(source) == 0;
  const bool destination_closed = close(destination) == 0;
  const bool closed = source_closed && destination_closed;
  if (synced && !closed)
    *error = "close CUDA job file: " + std::string(std::strerror(errno));
  if (!synced || !closed) {
    std::error_code ignored;
    fs::remove(temporary, ignored);
    return false;
  }
  std::error_code rename_error;
  fs::rename(temporary, output, rename_error);
  if (rename_error) {
    *error = "publish staged CUDA job file: " + rename_error.message();
    fs::remove(temporary, rename_error);
    return false;
  }
  return true;
}

class ServiceCudaEngine final : public CudaEngine {
  using ActiveTargets = std::vector<cuda_daemon::Request>;

  enum class TargetOrigin { kCheckpoint, kRestore };

  struct RegisteredTargets {
    ActiveTargets targets;
    CudaPinnedTargets pinned_targets;
    TargetOrigin origin;
    cuda_daemon::Backend backend;
    std::vector<std::shared_ptr<CudaRestoreWorkerRetention>>
        worker_retentions;
  };

  using TargetRegistration = std::shared_ptr<const RegisteredTargets>;

  enum class RegistrationFailure { kNone, kBusy, kShuttingDown, kPinFailed };

  class ActiveTargetGuard {
  public:
    ActiveTargetGuard(ServiceCudaEngine *owner,
                      TargetRegistration registration)
        : owner_(owner), registration_(std::move(registration)),
          uncaught_exceptions_(std::uncaught_exceptions()) {}
    ActiveTargetGuard(const ActiveTargetGuard &) = delete;
    ActiveTargetGuard &operator=(const ActiveTargetGuard &) = delete;
    ~ActiveTargetGuard() {
      // An exception after request dispatch has an unknown CUDA outcome. Keep
      // the identities and their origin registered so shutdown applies the
      // correct checkpoint-versus-restore lifecycle policy.
      if (registration_ != nullptr &&
          std::uncaught_exceptions() == uncaught_exceptions_)
        owner_->UnregisterActiveTargets(registration_);
    }
    explicit operator bool() const { return registration_ != nullptr; }
    const ActiveTargets &targets() const { return registration_->targets; }
    const TargetRegistration &registration() const { return registration_; }
    void RetainForShutdown(
        std::vector<std::shared_ptr<CudaRestoreWorkerRetention>>
            worker_retentions = {}) {
      owner_->RetainTargets(registration_, std::move(worker_retentions));
      registration_.reset();
    }

  private:
    ServiceCudaEngine *owner_;
    TargetRegistration registration_;
    int uncaught_exceptions_;
  };

public:
  ServiceCudaEngine(std::chrono::seconds max_operation_duration,
                    bool enable_regular_backend,
                    bool allow_posix_custom_storage,
                    size_t max_parallel_restores,
                    std::unique_ptr<cuda_operation::Service> service,
                    std::unique_ptr<CudaTargetTerminator> target_terminator,
                    std::unique_ptr<CuinterposeCoordinator> cuinterpose,
                    std::unique_ptr<CudaRestoreWorkerPool> regular_worker_pool,
                    std::unique_ptr<CudaRestoreWorkerPool>
                        custom_storage_worker_pool)
      : restore_slots_(static_cast<std::ptrdiff_t>(max_parallel_restores)),
        regular_worker_pool_(std::move(regular_worker_pool)),
        custom_storage_worker_pool_(std::move(custom_storage_worker_pool)),
        process_root_(ConfigureProcessRoot()),
        cuinterpose_(std::move(cuinterpose)),
        service_(std::move(service)),
        target_terminator_(std::move(target_terminator)),
        regular_backend_enabled_(enable_regular_backend),
        custom_storage_enabled_(allow_posix_custom_storage) {
    if (service_ == nullptr || target_terminator_ == nullptr ||
        cuinterpose_ == nullptr)
      throw std::invalid_argument(
          "PageBroker CUDA operation service, target terminator, and "
          "cuinterpose coordinator are required");
    if (regular_backend_enabled_ != (regular_worker_pool_ != nullptr) ||
        custom_storage_enabled_ !=
            (custom_storage_worker_pool_ != nullptr))
      throw std::invalid_argument(
          "PageBroker CUDA backends and their worker pools must be enabled together");
    cuda_operation::InitializationMetrics metrics;
    std::string error;
    if (!service_->Initialize(&metrics, &error))
      throw std::runtime_error("initialize PageBroker CUDA engine: " + error);
    custom_storage_available_ =
        allow_posix_custom_storage && metrics.custom_storage_available;
  }

  CheckpointAdmissionResult
  BeginCheckpoint(CudaStorageBackend storage_backend,
                  size_t target_count) override {
    cuda_daemon::Backend backend;
    switch (storage_backend) {
    case v1::CUDA_STORAGE_BACKEND_REGULAR:
      backend = cuda_daemon::Backend::kRegular;
      break;
    case v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE:
      backend = cuda_daemon::Backend::kPosix;
      break;
    default:
      throw std::invalid_argument(
          "checkpoint admission has an unsupported CUDA storage backend");
    }
    if (backend == cuda_daemon::Backend::kPosix &&
        !custom_storage_available_)
      throw std::invalid_argument(
          "PageBroker CUDA POSIX CustomStorage is unavailable");
    if (backend == cuda_daemon::Backend::kRegular &&
        !regular_backend_enabled_)
      throw std::invalid_argument(
          "PageBroker regular CUDA backend is unavailable");
    if (target_count == 0 ||
        target_count > cuda_daemon::kMaxRestoreBatchTargets)
      throw std::invalid_argument(
          "checkpoint admission requires between 1 and 64 CUDA targets");

    CheckpointAdmissionResult result;
    result.operation.target_may_be_mutated = false;
    if (SetBackendUnavailable(backend, &result.operation)) {
      return result;
    }
    auto admission = std::make_unique<ServiceCheckpointAdmission>(
        operation_gate_, backend, target_count);
    if (!*admission) {
      result.operation.failure_code = Failure::BUSY;
      result.operation.error =
          "PageBroker CUDA checkpoint boundary is busy";
      return result;
    }
    result.operation.succeeded = true;
    result.admission = std::move(admission);
    return result;
  }

  CudaOperationResult Checkpoint(const CudaCheckpointRequest &request,
                                 const fs::path &staging_directory,
                                 CheckpointAdmission &admission) override {
    auto *service_admission =
        dynamic_cast<ServiceCheckpointAdmission *>(&admission);
    if (service_admission == nullptr)
      throw std::invalid_argument(
          "CUDA checkpoint admission does not belong to this engine");
    const auto backend = ValidateRequest(request);
    if (backend == cuda_daemon::Backend::kPosix &&
        !custom_storage_available_)
      throw std::invalid_argument(
          "PageBroker CUDA POSIX CustomStorage is unavailable");
    if (backend == cuda_daemon::Backend::kRegular &&
        !regular_backend_enabled_)
      throw std::invalid_argument(
          "PageBroker regular CUDA backend is unavailable");
    if (service_admission->backend() != backend ||
        service_admission->target_count() !=
            static_cast<size_t>(request.targets_size()))
      throw std::invalid_argument(
          "CUDA checkpoint does not match its backend and target admission");
    CudaOperationResult result{.target_count =
                                   static_cast<size_t>(request.targets_size())};
    if (SetBackendUnavailable(backend, &result)) {
      return result;
    }
    const fs::path job_file =
        request.uses_job_file()
            ? JobFileForTarget(process_root_, request.targets(0).host_pid())
            : fs::path();
    ActiveTargets active_targets;
    active_targets.reserve(request.targets_size());
    for (const auto &target : request.targets())
      active_targets.push_back(BuildRequest(target, cuda_daemon::Action::kLock,
                                            backend, staging_directory,
                                            job_file));
    RegistrationFailure registration_failure = RegistrationFailure::kNone;
    std::string registration_error;
    ActiveTargetGuard active(
        this, RegisterActiveTargets(std::move(active_targets),
                                    TargetOrigin::kCheckpoint, backend,
                                    &registration_failure,
                                    &registration_error));
    if (!active) {
      if (registration_failure == RegistrationFailure::kShuttingDown) {
        (void)SetBackendUnavailable(backend, &result);
      } else if (registration_failure == RegistrationFailure::kPinFailed) {
        result.error = "pin PageBroker CUDA target before checkpoint: " +
                       registration_error;
      } else {
        result.failure_code = Failure::BUSY;
        result.error = "PageBroker CUDA target already has an active operation";
      }
      return result;
    }
    const auto coordinator_targets = CoordinatorTargets(request.targets());
    std::string cuinterpose_error;
    if (!ValidateIdentities(active.targets(), process_root_,
                            &cuinterpose_error) ||
        !cuinterpose_->ValidateEndpoints(coordinator_targets,
                                         request.uses_cuinterpose(),
                                         &cuinterpose_error)) {
      result.error = cuinterpose_error;
      return result;
    }
    bool cuinterpose_prepared = false;
    if (request.uses_cuinterpose()) {
      const auto prepared = cuinterpose_->Prepare(
          coordinator_targets, staging_directory, shutting_down_,
          backend == cuda_daemon::Backend::kPosix);
      if (!prepared.succeeded) {
        result.target_may_be_mutated = prepared.dispatched;
        result.error = prepared.error;
        if (prepared.dispatched &&
            !CleanupFailedTargets(active.registration(), &result.error)) {
          result.fatal = true;
          active.RetainForShutdown();
        }
        return result;
      }
      cuinterpose_prepared = true;
      CuinterposeStateMetadata state;
      if (!cuinterpose_->ReadState(staging_directory, request.targets_size(),
                                   &state, &result.error)) {
        result.target_may_be_mutated = true;
        if (!CleanupFailedTargets(active.registration(), &result.error)) {
          result.fatal = true;
          active.RetainForShutdown();
        }
        return result;
      }
      result.has_cuinterpose_state = true;
      result.cuinterpose_protocol_version = state.protocol_version;
      result.cuinterpose_state_size = state.size_bytes;
      result.cuinterpose_state_sha256 = std::move(state.sha256);
      result.cuinterpose_participant_count = state.participant_count;
    }
    size_t locked = 0;
    for (const auto &target : request.targets()) {
      auto operation = BuildRequest(target, cuda_daemon::Action::kLock,
                                    backend, staging_directory, job_file);
      operation.storage_dir.clear();
      operation.selected_devices.clear();
      const auto response = service_->ExecuteUncaptured(operation);
      if (response.cuda_status != CUDA_SUCCESS) {
        result.fatal = (response.flags & cuda_daemon::kResponseFatal) != 0;
        result.target_may_be_mutated =
            cuinterpose_prepared || locked != 0 ||
            (response.flags & cuda_daemon::kResponseLockNotAcquired) == 0;
        result.error = BoundedMessage(response);
        if (result.target_may_be_mutated) {
          // A failed rank may remain locked. Preserve the complete identity
          // set for the approved fail-closed shutdown path; a normal return
          // must never unregister partially mutated siblings.
          result.fatal = true;
          active.RetainForShutdown();
        }
        return result;
      }
      ++locked;
    }
    for (const auto &target : request.targets()) {
      const auto operation =
          BuildRequest(target, cuda_daemon::Action::kCheckpoint,
                       backend, staging_directory, job_file);
      const auto response = service_->ExecuteUncaptured(operation);
      if (response.cuda_status != CUDA_SUCCESS) {
        result.fatal = (response.flags & cuda_daemon::kResponseFatal) != 0;
        result.target_may_be_mutated = true;
        result.error = BoundedMessage(response);
        result.fatal = true;
        active.RetainForShutdown();
        return result;
      }
    }
    if (!job_file.empty()) {
      const auto identity =
          BuildRequest(request.targets(0), cuda_daemon::Action::kCheckpoint,
                       backend, staging_directory, job_file);
      std::string identity_error;
      if (!cuda_daemon::ValidateProcessIdentity(
              identity, process_root_.string(), &identity_error)) {
        result.target_may_be_mutated = true;
        result.error =
            "CUDA launch-job target identity changed before persistence: " +
            identity_error;
        result.fatal = true;
        active.RetainForShutdown();
        return result;
      }
    }
    if (!RefreshJobFile(job_file, staging_directory, &result.error)) {
      result.target_may_be_mutated = true;
      result.fatal = true;
      active.RetainForShutdown();
      return result;
    }
    result.succeeded = true;
    result.target_may_be_mutated = true;
    return result;
  }

  RestoreAdmissionResult BeginRestore(
      CudaStorageBackend storage_backend, size_t target_count,
      size_t expected_dispatch_group_count) override {
    cuda_daemon::Backend backend;
    switch (storage_backend) {
    case v1::CUDA_STORAGE_BACKEND_REGULAR:
      backend = cuda_daemon::Backend::kRegular;
      break;
    case v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE:
      backend = cuda_daemon::Backend::kPosix;
      break;
    default:
      throw std::invalid_argument(
          "restore admission has an unsupported CUDA storage backend");
    }
    if (backend == cuda_daemon::Backend::kPosix &&
        !custom_storage_available_)
      throw std::invalid_argument(
          "PageBroker CUDA POSIX CustomStorage is unavailable");
    if (backend == cuda_daemon::Backend::kRegular &&
        !regular_backend_enabled_)
      throw std::invalid_argument(
          "PageBroker regular CUDA backend is unavailable");
    if (target_count == 0 ||
        target_count > cuda_daemon::kMaxRestoreBatchTargets)
      throw std::invalid_argument(
          "restore admission requires between 1 and 64 CUDA targets");
    if (expected_dispatch_group_count == 0 ||
        expected_dispatch_group_count > target_count)
      throw std::invalid_argument(
          "restore admission dispatch-group count must be between 1 and the "
          "CUDA target count");
    if (backend == cuda_daemon::Backend::kRegular &&
        expected_dispatch_group_count != target_count)
      throw std::invalid_argument(
          "regular CUDA restore requires one worker per target");
    CudaRestoreWorkerPool *worker_pool = WorkerPoolForBackend(backend);
    if (worker_pool == nullptr)
      throw std::invalid_argument(
          "PageBroker CUDA restore workers are unavailable");

    RestoreAdmissionResult result;
    result.operation.target_may_be_mutated = false;
    if (SetBackendUnavailable(backend, &result.operation)) {
      return result;
    }
    if (expected_dispatch_group_count > worker_pool->capacity()) {
      result.operation.failure_code = Failure::INVALID_REQUEST;
      result.operation.error =
          "CUDA restore requires " +
          std::to_string(expected_dispatch_group_count) +
          " workers but PageBroker is configured with " +
          std::to_string(worker_pool->capacity());
      return result;
    }
    auto admission = std::make_unique<ServiceRestoreAdmission>(
        restore_slots_, operation_gate_, backend, target_count,
        expected_dispatch_group_count);
    if (!*admission) {
      if (SetBackendUnavailable(backend, &result.operation)) {
      } else {
        result.operation.failure_code = Failure::BUSY;
        result.operation.error =
            "PageBroker CUDA restore capacity or checkpoint boundary is busy";
      }
      return result;
    }
    {
      auto workers = worker_pool->TryAcquire(expected_dispatch_group_count);
      if (workers == nullptr) {
        if (worker_pool->fail_stop_required()) {
          (void)SetBackendUnavailable(backend, &result.operation);
        } else {
          result.operation.failure_code = Failure::BUSY;
          result.operation.error =
              "PageBroker CUDA worker capacity is busy";
        }
        return result;
      }
      admission->SetWorkerLease(std::move(workers));
    }
    result.operation.succeeded = true;
    result.admission = std::move(admission);
    return result;
  }

  CudaOperationResult Restore(const CudaRestoreRequest &request,
                              const fs::path &staging_directory,
                              RestoreAdmission &admission,
                              const DirectRestoreProcesses *direct_processes)
      override {
    auto *service_admission =
        dynamic_cast<ServiceRestoreAdmission *>(&admission);
    if (service_admission == nullptr)
      throw std::invalid_argument("CUDA restore admission does not belong to this engine");
    const auto backend = ValidateRequest(request);
    if (backend == cuda_daemon::Backend::kPosix &&
        !custom_storage_available_)
      throw std::invalid_argument(
          "PageBroker CUDA POSIX CustomStorage is unavailable");
    if (service_admission->backend() != backend ||
        service_admission->target_count() !=
            static_cast<size_t>(request.targets_size()))
      throw std::invalid_argument(
          "CUDA restore does not match its backend and target admission");
    if (service_admission->worker_lease() == nullptr ||
        service_admission->worker_lease()->size() !=
            service_admission->expected_dispatch_group_count())
      throw std::invalid_argument(
          "CUDA restore worker lease does not match its dispatch-group "
          "admission");
    CudaOperationResult result{.target_may_be_mutated = true,
                               .target_count =
                                   static_cast<size_t>(request.targets_size())};
    if (SetBackendUnavailable(backend, &result)) {
      result.target_may_be_mutated = false;
      return result;
    }
    std::vector<cuda_daemon::Request> targets;
    targets.reserve(request.targets_size());
    for (const auto &target : request.targets()) {
      const DirectRestoreProcess *direct_process = nullptr;
      if (direct_processes != nullptr) {
        const auto found = std::find_if(
            direct_processes->begin(), direct_processes->end(),
            [&](const DirectRestoreProcess &candidate) {
              return candidate.namespace_pid == target.namespace_pid();
            });
        if (found == direct_processes->end())
          throw std::invalid_argument(
              "CUDA target has no direct carrier descriptors");
        direct_process = &*found;
      }
      const fs::path job_file =
          request.uses_job_file()
              ? JobFileForTarget(process_root_, target.host_pid())
              : fs::path();
      targets.push_back(BuildRequest(target, cuda_daemon::Action::kRestore,
                                     backend, staging_directory, job_file,
                                     direct_process));
    }
    std::vector<cuda_operation::RestoreBatchJobFileGroup>
        custom_storage_groups;
    if (request.uses_job_file()) {
      std::vector<std::string> job_files;
      job_files.reserve(targets.size());
      for (const auto &target : targets)
        job_files.push_back(target.job_file);
      std::string group_error;
      if (!cuda_operation::GroupRestoreBatchJobFiles(
              job_files,
              cuda_operation::ResolveRestoreBatchJobFileIdentity,
              &custom_storage_groups, &group_error)) {
        result.target_may_be_mutated = false;
        result.error = "group CUDA restore launch jobs: " + group_error;
        return result;
      }
      for (const auto &group : custom_storage_groups) {
        for (const size_t target_index : group.request_indices) {
          targets[target_index].expected_job_file_device =
              group.identity.device;
          targets[target_index].expected_job_file_inode =
              group.identity.inode;
        }
      }
    }
    if (backend == cuda_daemon::Backend::kPosix) {
      if (!request.uses_job_file()) {
        custom_storage_groups.reserve(targets.size());
        for (size_t index = 0; index < targets.size(); ++index) {
          custom_storage_groups.push_back(
              {.representative = {},
               .identity = {},
               .request_indices = {index}});
        }
      }
      if (custom_storage_groups.size() !=
          service_admission->expected_dispatch_group_count()) {
        result.target_may_be_mutated = false;
        result.failure_code = Failure::INVALID_REQUEST;
        result.error =
            "CustomStorage restore dispatch-group count does not match "
            "admission";
        return result;
      }
      for (const auto &group : custom_storage_groups) {
        if (group.request_indices.size() < 2)
          continue;
        size_t descriptor_count = 0;
        for (const size_t target_index : group.request_indices) {
          const size_t target_descriptors =
              targets[target_index].pinned_storage_files.size();
          if (target_descriptors >
              cuda_daemon::kMaxPinnedStorageFilesPerRequest -
                  descriptor_count) {
            result.target_may_be_mutated = false;
            result.failure_code = Failure::INVALID_REQUEST;
            result.error =
                "CustomStorage launch-job batch exceeds the 64-carrier "
                "worker transport limit";
            return result;
          }
          descriptor_count += target_descriptors;
        }
      }
    }
    RegistrationFailure registration_failure = RegistrationFailure::kNone;
    std::string registration_error;
    ActiveTargetGuard active(
        this, RegisterActiveTargets(targets, TargetOrigin::kRestore, backend,
                                    &registration_failure,
                                    &registration_error));
    if (!active) {
      result.target_may_be_mutated = false;
      if (registration_failure == RegistrationFailure::kShuttingDown) {
        (void)SetBackendUnavailable(backend, &result);
      } else if (registration_failure == RegistrationFailure::kPinFailed) {
        result.error = "pin PageBroker CUDA target before restore: " +
                       registration_error;
      } else {
        result.failure_code = Failure::BUSY;
        result.error = "PageBroker CUDA target already has an active operation";
      }
      return result;
    }
    const auto coordinator_targets = CoordinatorTargets(request.targets());
    std::string cuinterpose_error;
    if (request.uses_cuinterpose() != request.has_cuinterpose_state()) {
      result.target_may_be_mutated = false;
      result.error =
          "cuinterpose restore opt-in and state metadata must be present together";
      return result;
    }
    if (!ValidateIdentities(targets, process_root_, &cuinterpose_error) ||
        !cuinterpose_->ValidateEndpoints(coordinator_targets,
                                         request.uses_cuinterpose(),
                                         &cuinterpose_error)) {
      result.target_may_be_mutated = false;
      result.error = cuinterpose_error;
      return result;
    }
    if (request.uses_cuinterpose()) {
      CuinterposeStateMetadata actual;
      if (!cuinterpose_->ReadState(staging_directory, request.targets_size(),
                                   &actual, &cuinterpose_error)) {
        result.target_may_be_mutated = false;
        result.error = cuinterpose_error;
        return result;
      }
      const auto &expected = request.cuinterpose_state();
      if (expected.protocol_version() != actual.protocol_version ||
          expected.size_bytes() != actual.size_bytes ||
          expected.sha256() != actual.sha256 ||
          expected.participant_count() != actual.participant_count) {
        result.target_may_be_mutated = false;
        result.error = "cuinterpose state metadata mismatch";
        return result;
      }
      const auto validated =
          cuinterpose_->ValidateState(staging_directory, shutting_down_);
      if (!validated.succeeded) {
        result.target_may_be_mutated = false;
        result.error = validated.error;
        return result;
      }
    }
    // Admission can predate a fatal result from a sibling transaction. Close
    // that accepted-handler gap immediately before the first native dispatch;
    // FailStop() is nonblocking and only flips this PageBroker-owned gate.
    if (SetBackendUnavailable(backend, &result)) {
      result.target_may_be_mutated = false;
      return result;
    }
    struct RestoreDispatch {
      size_t worker_index;
      std::vector<size_t> target_indices;
      cuda_daemon::Request request;
    };
    std::vector<RestoreDispatch> restore_dispatches;
    std::vector<size_t> target_worker_indices(targets.size());
    if (backend == cuda_daemon::Backend::kPosix) {
      restore_dispatches.reserve(custom_storage_groups.size());
      for (size_t group_index = 0;
           group_index < custom_storage_groups.size(); ++group_index) {
        const auto &group = custom_storage_groups[group_index];
        cuda_daemon::Request operation;
        if (group.request_indices.size() == 1) {
          operation = targets[group.request_indices.front()];
        } else {
          operation.action = cuda_daemon::Action::kRestoreBatch;
          operation.backend = cuda_daemon::Backend::kPosix;
          operation.pid = static_cast<uint32_t>(group.request_indices.size());
          operation.targets.reserve(group.request_indices.size());
          for (const size_t target_index : group.request_indices)
            operation.targets.push_back(targets[target_index]);
        }
        for (const size_t target_index : group.request_indices)
          target_worker_indices[target_index] = group_index;
        restore_dispatches.push_back(
            {.worker_index = group_index,
             .target_indices = group.request_indices,
             .request = std::move(operation)});
      }
    } else {
      restore_dispatches.reserve(targets.size());
      for (size_t index = 0; index < targets.size(); ++index) {
        target_worker_indices[index] = index;
        restore_dispatches.push_back(
            {.worker_index = index,
             .target_indices = {index},
             .request = targets[index]});
      }
    }
    std::vector<CudaWorkerRpcResult> worker_restore_responses;
    auto *worker_lease = service_admission->worker_lease();
    const auto call_restore = [worker_lease](const RestoreDispatch &dispatch) {
      try {
        return worker_lease->Call(dispatch.worker_index, dispatch.request);
      } catch (const std::exception &error) {
        return CudaWorkerRpcResult{
            .status = CudaWorkerRpcStatus::kProtocolError,
            .error = std::string("CUDA worker RPC exception: ") + error.what(),
            .unknown_outcome = true};
      } catch (...) {
        return CudaWorkerRpcResult{
            .status = CudaWorkerRpcStatus::kProtocolError,
            .error = "unknown CUDA worker RPC exception",
            .unknown_outcome = true};
      }
    };
    worker_restore_responses.resize(restore_dispatches.size());
    if (backend == cuda_daemon::Backend::kRegular &&
        request.uses_job_file()) {
      // NVIDIA's launch-job contract requires processes sharing one job file
      // to be restored sequentially. The leased workers are distinct
      // processes, so the helper's process-local job-file mutex cannot protect
      // concurrent calls here. Preserve manifest (parent-first) order within
      // each stable job-file identity while allowing independent launch jobs
      // in this request to overlap.
      std::vector<std::future<
          std::vector<std::pair<size_t, CudaWorkerRpcResult>>>> restores;
      restores.reserve(custom_storage_groups.size());
      for (const auto &group : custom_storage_groups) {
        restores.push_back(std::async(
            std::launch::async,
            [call_restore, &restore_dispatches,
             indices = group.request_indices] {
              std::vector<std::pair<size_t, CudaWorkerRpcResult>> responses;
              responses.reserve(indices.size());
              for (const size_t index : indices) {
                responses.emplace_back(
                    index, call_restore(restore_dispatches[index]));
              }
              return responses;
            }));
      }
      for (auto &restore : restores) {
        for (auto &[index, response] : restore.get())
          worker_restore_responses[index] = std::move(response);
      }
    } else {
      std::vector<std::future<CudaWorkerRpcResult>> restores;
      restores.reserve(restore_dispatches.size());
      for (const auto &dispatch : restore_dispatches) {
        restores.push_back(std::async(
            std::launch::async,
            [call_restore, dispatch] {
              return call_restore(dispatch);
            }));
      }
      for (size_t index = 0; index < restores.size(); ++index)
        worker_restore_responses[index] = restores[index].get();
    }
    std::vector<std::shared_ptr<CudaRestoreWorkerRetention>>
        worker_retentions;
    worker_retentions.resize(targets.size());
    CudaRestoreWorkerPool *backend_pool = WorkerPoolForBackend(backend);
    for (size_t index = 0; index < worker_restore_responses.size(); ++index) {
        const auto &dispatch = restore_dispatches[index];
        const bool failed =
            worker_restore_responses[index].status !=
                CudaWorkerRpcStatus::kOk ||
            worker_restore_responses[index].response.cuda_status !=
                CUDA_SUCCESS;
        if (failed &&
            worker_restore_responses[index].status !=
                CudaWorkerRpcStatus::kPoolFailStopped)
          worker_lease->Poison(dispatch.worker_index);
        auto retention = worker_lease->Retain(dispatch.worker_index);
        if (retention == nullptr) {
          worker_lease->Poison(dispatch.worker_index);
          worker_restore_responses[index] = {
              .status = CudaWorkerRpcStatus::kProtocolError,
              .error = "failed to retain CUDA worker context ownership",
              .unknown_outcome = true};
        }
        for (const size_t target_index : dispatch.target_indices)
          worker_retentions[target_index] = retention;
    }
    std::string selected_failure;
    bool selected_failure_is_fatal = false;
    for (const auto &restore : worker_restore_responses) {
      const bool failed = restore.status != CudaWorkerRpcStatus::kOk ||
                          restore.response.cuda_status != CUDA_SUCCESS;
      if (!failed)
        continue;
      const bool fatal = restore.unknown_outcome ||
                         restore.status == CudaWorkerRpcStatus::kFatalResponse ||
                         (restore.response.flags & cuda_daemon::kResponseFatal) != 0 ||
                         backend_pool->fail_stop_required();
      if (selected_failure.empty() ||
          (fatal && !selected_failure_is_fatal)) {
        selected_failure = BoundedWorkerMessage(restore);
        selected_failure_is_fatal = fatal;
      }
    }
    if (!selected_failure.empty()) {
      // Every rank has already completed. A fatal response from any rank must
      // dominate a lower-index nonfatal response so PageBroker retains the
      // full identity set and enters fail-closed shutdown.
      result.error = std::move(selected_failure);
      // A nonfatal response only describes that rank. Another sibling may
      // already be restored and still locked, so any post-dispatch worker
      // failure quarantines the complete regular generation. The engine has
      // no safe in-process worker-generation swap, so PageBroker must exit
      // after draining identity-matched ownership instead of remaining Ready
      // with permanent regular admission loss.
      if (backend == cuda_daemon::Backend::kRegular) {
        regular_fail_stop_.store(true, std::memory_order_release);
        result.fatal = true;
      } else {
        shutting_down_.store(true, std::memory_order_release);
        result.fatal = true;
      }
      active.RetainForShutdown(std::move(worker_retentions));
      return result;
    }
    for (size_t index = 0; index < worker_restore_responses.size(); ++index) {
      const auto &response = worker_restore_responses[index].response;
      if (!response.output.empty()) {
        const size_t target_index =
            restore_dispatches[index].target_indices.front();
        EmitWorkerOutput(target_index, targets[target_index], response.output);
      }
    }
    // Match the child-first completion order used by the batched CustomStorage
    // restore. Resuming a parent rank first can let it reap compile-worker
    // children before their CUDA state has been fully released.
    for (size_t completed = 0; completed < targets.size(); ++completed) {
      const size_t target_index = cuda_operation::RestoreBatchCompletionIndex(
          completed, targets.size());
      auto unlock = BuildControlRequest(
          targets[target_index], cuda_daemon::Action::kUnlock);
      {
        CudaWorkerRpcResult response;
        const size_t worker_index = target_worker_indices[target_index];
        try {
          response =
              service_admission->worker_lease()->Call(worker_index, unlock);
        } catch (const std::exception &error) {
          response = {.status = CudaWorkerRpcStatus::kProtocolError,
                      .error = std::string("CUDA worker RPC exception: ") +
                               error.what(),
                      .unknown_outcome = true};
        } catch (...) {
          response = {.status = CudaWorkerRpcStatus::kProtocolError,
                      .error = "unknown CUDA worker RPC exception",
                      .unknown_outcome = true};
        }
        if (response.status == CudaWorkerRpcStatus::kOk &&
            response.response.cuda_status == CUDA_SUCCESS)
          continue;
        // Every target has completed restore and an earlier sibling may
        // already be running. An unlock failure is therefore a full-request
        // lifecycle failure even when the lower layer reports it nonfatal.
        if (backend == cuda_daemon::Backend::kRegular) {
          regular_fail_stop_.store(true, std::memory_order_release);
          result.fatal = true;
        } else {
          shutting_down_.store(true, std::memory_order_release);
          result.fatal = true;
        }
        result.error = BoundedWorkerMessage(response);
        if ((response.status != CudaWorkerRpcStatus::kOk ||
             response.response.cuda_status != CUDA_SUCCESS) &&
            response.status != CudaWorkerRpcStatus::kPoolFailStopped)
          service_admission->worker_lease()->Poison(worker_index);
        active.RetainForShutdown(std::move(worker_retentions));
        return result;
      }
    }
    if (request.uses_cuinterpose()) {
      if (!ValidateIdentities(targets, process_root_, &cuinterpose_error)) {
        result.error = cuinterpose_error;
        if (!CleanupFailedRestoreTargets(backend, active.registration(),
                                         &result.error)) {
          if (backend == cuda_daemon::Backend::kRegular)
            regular_fail_stop_.store(true, std::memory_order_release);
          result.fatal = true;
          active.RetainForShutdown(std::move(worker_retentions));
        }
        return result;
      }
      // The native CUDA restore contract requires targets to be unlocked
      // before cuinterpose rebuilds shared VMM and multicast topology. The
      // workload must remain at its application-level restore-complete gate
      // until the whole PageBroker request succeeds; a startup probe alone
      // does not prevent application threads from running here.
      const auto restored = cuinterpose_->Restore(
          coordinator_targets, staging_directory, shutting_down_);
      if (!restored.succeeded) {
        result.error = restored.error;
        if (!CleanupFailedRestoreTargets(backend, active.registration(),
                                         &result.error)) {
          if (backend == cuda_daemon::Backend::kRegular)
            regular_fail_stop_.store(true, std::memory_order_release);
          result.fatal = true;
          active.RetainForShutdown(std::move(worker_retentions));
        }
        return result;
      }
    }
    result.succeeded = true;
    // The CUDA service retains restored primary contexts until target exit;
    // retain the same restored identities so another transaction cannot
    // restore the live target concurrently and shutdown covers the workload.
    active.RetainForShutdown(std::move(worker_retentions));
    return result;
  }

  bool BeginShutdown(std::string *error) override {
    if (error == nullptr)
      return false;
    shutting_down_.store(true, std::memory_order_release);
    error->clear();
    std::string service_error;
    CUresult service_status = CUDA_ERROR_OPERATING_SYSTEM;
    bool service_threw = false;
    try {
      service_status =
          service_->BeginShutdown(process_root_.string(), &service_error);
    } catch (...) {
      // Shutdown() performs the fail-closed retry. Do not let an allocation or
      // diagnostic exception skip that phase and unwind retained contexts.
      service_error.clear();
      service_threw = true;
    }
    std::string target_error;
    bool targets_terminated = false;
    bool target_termination_threw = false;
    try {
      targets_terminated = TerminateActiveTargets(&target_error);
    } catch (...) {
      target_error.clear();
      target_termination_threw = true;
    }
    if (service_threw)
      *error = "stop active CUDA operation: unexpected exception";
    else if (service_status != CUDA_SUCCESS)
      *error = service_error.empty()
                   ? "stop active CUDA operation: " + CudaError(service_status)
                   : service_error;
    if (target_termination_threw) {
      if (!error->empty())
        *error += "; ";
      *error += "terminate active CUDA targets: unexpected exception";
    } else if (!targets_terminated) {
      if (!error->empty())
        *error += "; ";
      *error += target_error;
    }
    bool workers_stopped = true;
    std::string worker_error;
    if (targets_terminated) {
      workers_stopped =
          ShutdownRegularPool(std::chrono::seconds(5), &worker_error);
      std::string custom_error;
      const bool custom_stopped =
          ShutdownCustomStoragePool(std::chrono::seconds(5), &custom_error);
      if (!custom_stopped) {
        if (!worker_error.empty() && !custom_error.empty())
          worker_error += "; ";
        worker_error += custom_error;
      }
      workers_stopped = workers_stopped && custom_stopped;
    }
    if (!workers_stopped) {
      if (!error->empty())
        *error += "; ";
      *error += worker_error;
    }
    return !service_threw && service_status == CUDA_SUCCESS &&
           !target_termination_threw && targets_terminated && workers_stopped;
  }

  bool ReapExited(std::string *error) override {
    if (error == nullptr)
      return false;
    error->clear();
    bool regular_maintained = true;
    std::string regular_error;
    if (regular_worker_pool_ != nullptr &&
        (regular_fail_stop_.load(std::memory_order_acquire) ||
         regular_worker_pool_->fail_stop_required())) {
      if (!custom_storage_enabled_) {
        *error =
            "PageBroker CUDA worker exited with an unknown restore outcome";
        return false;
      }
      regular_fail_stop_.store(true, std::memory_order_release);
      regular_maintained = MaintainFailedRegularBackend(&regular_error);
    }
    if (custom_storage_worker_pool_ != nullptr &&
        custom_storage_worker_pool_->fail_stop_required()) {
      shutting_down_.store(true, std::memory_order_release);
      *error = "PageBroker CustomStorage worker exited with an unknown "
               "restore outcome";
      return false;
    }
    std::string service_error;
    const CUresult status =
        service_->ReapExited(process_root_.string(), &service_error);
    // PersistentTargetContexts discards an exited target's bookkeeping after
    // attempting release: retrying the whole device set could double-release
    // devices that succeeded before a later one failed. A CUDA release error is
    // therefore recoverable only by replacing this PageBroker process, whose
    // teardown releases any driver-owned references. Withdraw readiness and
    // reject new work instead of continuing with an untracked retained context.
    if (status != CUDA_SUCCESS)
      shutting_down_.store(true, std::memory_order_release);
    bool service_reaped = status == CUDA_SUCCESS && service_error.empty();
    if (service_reaped)
      service_reaped = ReapRetainedTargets(&service_error);
    else if (status != CUDA_SUCCESS && service_error.empty())
      service_error =
          "reap exited PageBroker CUDA targets: " + CudaError(status);
    if (!regular_maintained)
      *error = regular_error;
    if (!service_reaped) {
      if (!error->empty())
        *error += "; ";
      *error += service_error;
    }
    return regular_maintained && service_reaped;
  }

  void FailStop() noexcept override {
    shutting_down_.store(true, std::memory_order_release);
  }

  bool ShutdownRequired() const override {
    return shutting_down_.load(std::memory_order_acquire) ||
           (custom_storage_worker_pool_ != nullptr &&
            custom_storage_worker_pool_->fail_stop_required()) ||
           regular_fail_stop_.load(std::memory_order_acquire) ||
           (regular_worker_pool_ != nullptr &&
            regular_worker_pool_->fail_stop_required());
  }

  bool Shutdown(std::string *error) override {
    if (error == nullptr)
      return false;
    CUresult termination_status = CUDA_ERROR_OPERATING_SYSTEM;
    bool active_targets_terminated = false;
    while (termination_status != CUDA_SUCCESS || !active_targets_terminated) {
      std::string retained_error;
      try {
        termination_status = service_->TerminateRetainedTargets(
            process_root_.string(), &retained_error);
      } catch (...) {
        termination_status = CUDA_ERROR_OPERATING_SYSTEM;
        retained_error.clear();
      }
      std::string active_error;
      try {
        active_targets_terminated = TerminateActiveTargets(&active_error);
      } catch (...) {
        active_targets_terminated = false;
        active_error.clear();
      }
      if (termination_status == CUDA_SUCCESS && active_targets_terminated)
        break;
      // A signaling or pidfd-poll failure is not evidence that a restored
      // target exited. Retain its contexts and exact-lifetime handles and retry;
      // neither transient procfs loss nor numeric PID reuse participates here.
      std::fprintf(
          stderr,
          "terminate PageBroker CUDA targets failed; retaining contexts and "
          "retrying: retained=%s; active=%s\n",
          termination_status == CUDA_SUCCESS
              ? "ok"
              : (retained_error.empty() ? "exception" : retained_error.c_str()),
          active_targets_terminated
              ? "ok"
              : (active_error.empty() ? "exception" : active_error.c_str()));
      std::fflush(stderr);
      std::this_thread::sleep_for(std::chrono::seconds(1));
    }
    if (regular_worker_pool_ != nullptr &&
        !regular_pool_shutdown_complete_.load(std::memory_order_acquire)) {
      std::string worker_error;
      while (!regular_worker_pool_->Shutdown(std::chrono::seconds(1),
                                             &worker_error)) {
        std::fprintf(stderr,
                     "shutdown PageBroker CUDA workers failed; retaining "
                     "worker contexts and retrying: %s\n",
                     worker_error.empty() ? "unknown error"
                                          : worker_error.c_str());
        std::fflush(stderr);
        std::this_thread::sleep_for(std::chrono::seconds(1));
      }
      regular_pool_shutdown_complete_.store(true, std::memory_order_release);
    }
    if (custom_storage_worker_pool_ != nullptr &&
        !custom_pool_shutdown_complete_.load(std::memory_order_acquire)) {
      std::string worker_error;
      while (!custom_storage_worker_pool_->Shutdown(std::chrono::seconds(1),
                                                    &worker_error)) {
        std::fprintf(stderr,
                     "shutdown PageBroker CustomStorage workers failed; "
                     "retaining worker contexts and retrying: %s\n",
                     worker_error.empty() ? "unknown error"
                                          : worker_error.c_str());
        std::fflush(stderr);
        std::this_thread::sleep_for(std::chrono::seconds(1));
      }
      custom_pool_shutdown_complete_.store(true, std::memory_order_release);
    }
    const CUresult release_status = service_->ReleaseAll();
    if (release_status != CUDA_SUCCESS) {
      *error = "release PageBroker CUDA primary contexts: " +
               CudaError(release_status);
      return false;
    }
    error->clear();
    return true;
  }

private:
  bool GlobalFailStopRequired() {
    if (shutting_down_.load(std::memory_order_acquire))
      return true;
    if (regular_fail_stop_.load(std::memory_order_acquire)) {
      shutting_down_.store(true, std::memory_order_release);
      return true;
    }
    if (custom_storage_worker_pool_ != nullptr &&
        custom_storage_worker_pool_->fail_stop_required()) {
      shutting_down_.store(true, std::memory_order_release);
      return true;
    }
    if (regular_worker_pool_ != nullptr &&
        regular_worker_pool_->fail_stop_required()) {
      // A worker monitor can discover generation loss independently of a
      // request handler. Restart the complete engine generation even in mixed
      // mode; otherwise the daemon would remain Ready without regular
      // capacity after maintenance drains the failed pool.
      shutting_down_.store(true, std::memory_order_release);
      return true;
    }
    return false;
  }

  bool RegularFailStopRequired() {
    if (GlobalFailStopRequired())
      return true;
    if (regular_fail_stop_.load(std::memory_order_acquire))
      return true;
    if (regular_worker_pool_ != nullptr &&
        regular_worker_pool_->fail_stop_required()) {
      // Quarantine regular admission immediately. ShutdownRequired exposes
      // the lost fixed-size generation to the daemon so Kubernetes replaces
      // PageBroker after its identity-safe shutdown sequence.
      regular_fail_stop_.store(true, std::memory_order_release);
      return true;
    }
    return false;
  }

  bool BackendFailStopRequired(cuda_daemon::Backend backend) {
    return backend == cuda_daemon::Backend::kRegular
               ? RegularFailStopRequired()
               : GlobalFailStopRequired();
  }

  CudaRestoreWorkerPool *WorkerPoolForBackend(cuda_daemon::Backend backend) {
    return backend == cuda_daemon::Backend::kRegular
               ? regular_worker_pool_.get()
               : custom_storage_worker_pool_.get();
  }

  bool SetBackendUnavailable(cuda_daemon::Backend backend,
                             CudaOperationResult *result) {
    if (!BackendFailStopRequired(backend))
      return false;
    if (GlobalFailStopRequired()) {
      result->fatal = true;
      result->error = "PageBroker CUDA engine is shutting down";
    } else {
      result->failure_code = Failure::CUDA_ERROR;
      result->error =
          "PageBroker regular CUDA worker generation is fail-stopped";
    }
    return true;
  }

  bool CleanupFailedTargets(const TargetRegistration &targets,
                            std::string *error) {
    std::string cleanup_error;
    if (!target_terminator_->Terminate(targets->pinned_targets,
                                       std::chrono::seconds(5),
                                       &cleanup_error)) {
      if (!error->empty())
        *error += "; ";
      *error += "targeted CUDA failure cleanup: " + cleanup_error;
      return false;
    }
    const CUresult reap_status =
        service_->ReapExited(process_root_.string(), &cleanup_error);
    if (reap_status != CUDA_SUCCESS) {
      if (!error->empty())
        *error += "; ";
      *error += cleanup_error.empty()
                    ? "targeted CUDA context cleanup: " +
                          CudaError(reap_status)
                    : "targeted CUDA context cleanup: " + cleanup_error;
      return false;
    }
    return true;
  }

  bool CleanupFailedRestoreTargets(cuda_daemon::Backend backend,
                                   const TargetRegistration &targets,
                                   std::string *error) {
    std::string cleanup_error;
    if (!target_terminator_->Terminate(targets->pinned_targets,
                                       std::chrono::seconds(5),
                                       &cleanup_error)) {
      if (!error->empty())
        *error += "; ";
      *error += "targeted CUDA failure cleanup: " + cleanup_error;
      return false;
    }
    if (backend == cuda_daemon::Backend::kRegular)
      return true;
    const CUresult reap_status =
        service_->ReapExited(process_root_.string(), &cleanup_error);
    if (reap_status == CUDA_SUCCESS)
      return true;
    if (!error->empty())
      *error += "; ";
    *error += cleanup_error.empty()
                  ? "targeted CUDA context cleanup: " +
                        CudaError(reap_status)
                  : "targeted CUDA context cleanup: " + cleanup_error;
    return false;
  }

  bool TerminateRestoreTargets(
      std::optional<cuda_daemon::Backend> backend, std::string *error) {
    CudaPinnedTargets targets;
    {
      std::lock_guard lock(active_targets_mutex_);
      for (const auto &operation : active_targets_) {
        if (operation->origin == TargetOrigin::kRestore &&
            (!backend.has_value() || operation->backend == *backend))
          targets.insert(targets.end(), operation->pinned_targets.begin(),
                         operation->pinned_targets.end());
      }
      for (const auto &operation : retained_targets_) {
        if (operation->origin == TargetOrigin::kRestore &&
            (!backend.has_value() || operation->backend == *backend))
          targets.insert(targets.end(), operation->pinned_targets.begin(),
                         operation->pinned_targets.end());
      }
    }
    if (targets.empty()) {
      error->clear();
      return true;
    }
    return target_terminator_->Terminate(targets, std::chrono::seconds(5),
                                         error);
  }

  bool TerminateActiveTargets(std::string *error) {
    return TerminateRestoreTargets(std::nullopt, error);
  }

  bool HasRestoreTargets(cuda_daemon::Backend backend) {
    std::lock_guard lock(active_targets_mutex_);
    const auto matches = [backend](const TargetRegistration &operation) {
      return operation->origin == TargetOrigin::kRestore &&
             operation->backend == backend;
    };
    return std::any_of(active_targets_.begin(), active_targets_.end(),
                       matches) ||
           std::any_of(retained_targets_.begin(), retained_targets_.end(),
                       matches);
  }

  bool MaintainFailedRegularBackend(std::string *error) {
    std::string termination_error;
    if (!TerminateRestoreTargets(cuda_daemon::Backend::kRegular,
                                 &termination_error)) {
      *error = "terminate fail-stopped regular CUDA targets: " +
               termination_error;
      return false;
    }
    std::string reap_error;
    if (!ReapRetainedTargets(&reap_error,
                             cuda_daemon::Backend::kRegular)) {
      *error = "reap fail-stopped regular CUDA targets: " + reap_error;
      return false;
    }
    if (HasRestoreTargets(cuda_daemon::Backend::kRegular)) {
      *error = "fail-stopped regular CUDA operations are still draining";
      return false;
    }
    std::string shutdown_error;
    if (!ShutdownRegularPool(std::chrono::milliseconds(100),
                             &shutdown_error)) {
      *error = "shutdown fail-stopped regular CUDA workers: " +
               shutdown_error;
      return false;
    }
    error->clear();
    return true;
  }

  bool ShutdownRegularPool(std::chrono::milliseconds timeout,
                           std::string *error) {
    if (regular_worker_pool_ == nullptr ||
        regular_pool_shutdown_complete_.load(std::memory_order_acquire)) {
      error->clear();
      return true;
    }
    if (!regular_worker_pool_->Shutdown(timeout, error))
      return false;
    regular_pool_shutdown_complete_.store(true, std::memory_order_release);
    error->clear();
    return true;
  }

  bool ShutdownCustomStoragePool(std::chrono::milliseconds timeout,
                                 std::string *error) {
    if (custom_storage_worker_pool_ == nullptr ||
        custom_pool_shutdown_complete_.load(std::memory_order_acquire)) {
      error->clear();
      return true;
    }
    if (!custom_storage_worker_pool_->Shutdown(timeout, error))
      return false;
    custom_pool_shutdown_complete_.store(true, std::memory_order_release);
    error->clear();
    return true;
  }

  TargetRegistration RegisterActiveTargets(
      ActiveTargets targets, TargetOrigin origin,
      cuda_daemon::Backend backend,
      RegistrationFailure *failure = nullptr,
      std::string *registration_error = nullptr) {
    std::lock_guard lock(active_targets_mutex_);
    if (BackendFailStopRequired(backend)) {
      if (failure != nullptr)
        *failure = RegistrationFailure::kShuttingDown;
      return nullptr;
    }
    for (const auto &registered : active_targets_) {
      for (const auto &existing : registered->targets) {
        if (std::any_of(targets.begin(), targets.end(),
                        [&](const auto &target) {
                          return target.pid == existing.pid;
                        })) {
          if (failure != nullptr)
            *failure = RegistrationFailure::kBusy;
          return nullptr;
        }
      }
    }
    for (const auto &registered : retained_targets_) {
      for (const auto &existing : registered->targets) {
        if (std::any_of(targets.begin(), targets.end(),
                        [&](const auto &target) {
                          return target.pid == existing.pid;
                        })) {
          if (failure != nullptr)
            *failure = RegistrationFailure::kBusy;
          return nullptr;
        }
      }
    }
    CudaPinnedTargets pinned_targets;
    std::string pin_error;
    if (!target_terminator_->Pin(targets, process_root_.string(),
                                 &pinned_targets, &pin_error) ||
        pinned_targets.size() != targets.size()) {
      if (failure != nullptr)
        *failure = RegistrationFailure::kPinFailed;
      if (registration_error != nullptr)
        *registration_error = pin_error.empty()
                                  ? "target pin count mismatch"
                                  : std::move(pin_error);
      return nullptr;
    }
    auto active = std::make_shared<const RegisteredTargets>(
        RegisteredTargets{.targets = std::move(targets),
                          .pinned_targets = std::move(pinned_targets),
                          .origin = origin,
                          .backend = backend,
                          .worker_retentions = {}});
    active_targets_.push_back(active);
    if (failure != nullptr)
      *failure = RegistrationFailure::kNone;
    return active;
  }

  void UnregisterActiveTargets(const TargetRegistration &targets) {
    std::lock_guard lock(active_targets_mutex_);
    const auto item =
        std::find(active_targets_.begin(), active_targets_.end(), targets);
    if (item != active_targets_.end())
      active_targets_.erase(item);
  }

  void RetainTargets(
      const TargetRegistration &targets,
      std::vector<std::shared_ptr<CudaRestoreWorkerRetention>>
          worker_retentions) {
    if (targets == nullptr)
      return;
    std::lock_guard lock(active_targets_mutex_);
    const auto item =
        std::find(active_targets_.begin(), active_targets_.end(), targets);
    if (item != active_targets_.end()) {
      retained_targets_.push_back(
          std::make_shared<const RegisteredTargets>(RegisteredTargets{
              .targets = (*item)->targets,
              .pinned_targets = (*item)->pinned_targets,
              .origin = (*item)->origin,
              .backend = (*item)->backend,
              .worker_retentions = std::move(worker_retentions)}));
      active_targets_.erase(item);
    }
  }

  bool ReapRetainedTargets(
      std::string *error,
      std::optional<cuda_daemon::Backend> backend = std::nullopt) {
    std::lock_guard lock(active_targets_mutex_);
    std::vector<TargetRegistration> retained;
    retained.reserve(retained_targets_.size());
    for (const auto &operation : retained_targets_) {
      if (backend.has_value() && operation->backend != *backend) {
        retained.push_back(operation);
        continue;
      }
      ActiveTargets live;
      CudaPinnedTargets live_pinned;
      std::vector<std::shared_ptr<CudaRestoreWorkerRetention>>
          live_worker_retentions;
      live.reserve(operation->targets.size());
      live_pinned.reserve(operation->pinned_targets.size());
      live_worker_retentions.reserve(operation->worker_retentions.size());
      for (size_t index = 0; index < operation->targets.size(); ++index) {
        const auto &target = operation->targets[index];
        bool exited = false;
        std::string identity_error;
        if (index >= operation->pinned_targets.size() ||
            !target_terminator_->Exited(*operation->pinned_targets[index],
                                        &exited, &identity_error)) {
          *error = "cannot reap retained CUDA target " +
                   std::to_string(target.pid) + ": " + identity_error;
          return false;
        }
        if (!exited) {
          live.push_back(target);
          live_pinned.push_back(operation->pinned_targets[index]);
          if (index < operation->worker_retentions.size())
            live_worker_retentions.push_back(
                operation->worker_retentions[index]);
        }
      }
      if (live.size() == operation->targets.size())
        retained.push_back(operation);
      else if (!live.empty())
        retained.push_back(std::make_shared<const RegisteredTargets>(
            RegisteredTargets{.targets = std::move(live),
                              .pinned_targets = std::move(live_pinned),
                              .origin = operation->origin,
                              .backend = operation->backend,
                              .worker_retentions =
                                  std::move(live_worker_retentions)}));
    }
    retained_targets_ = std::move(retained);
    error->clear();
    return true;
  }

  // Admit a bounded number of independent restore transactions. Targets with
  // no launch job and distinct CustomStorage launch-job identities use
  // separate workers concurrently. Participants that share one launch-job
  // inode use one batched worker and ordered CUDA preparation while their
  // independent extents still transfer concurrently.
  std::counting_semaphore<kMaximumParallelRestores> restore_slots_;
  OperationAdmissionGate operation_gate_;
  std::atomic<bool> shutting_down_{false};
  std::atomic<bool> regular_fail_stop_{false};
  std::atomic<bool> regular_pool_shutdown_complete_{false};
  std::atomic<bool> custom_pool_shutdown_complete_{false};
  // Declared before target registrations so retained worker-owner tokens are
  // destroyed before their pool during normal object teardown.
  std::unique_ptr<CudaRestoreWorkerPool> regular_worker_pool_;
  std::unique_ptr<CudaRestoreWorkerPool> custom_storage_worker_pool_;
  std::mutex active_targets_mutex_;
  std::vector<TargetRegistration> active_targets_;
  std::vector<TargetRegistration> retained_targets_;
  fs::path process_root_;
  std::unique_ptr<CuinterposeCoordinator> cuinterpose_;
  std::unique_ptr<cuda_operation::Service> service_;
  std::unique_ptr<CudaTargetTerminator> target_terminator_;
  bool regular_backend_enabled_ = false;
  bool custom_storage_enabled_ = false;
  bool custom_storage_available_ = false;
};

} // namespace

std::unique_ptr<CudaEngine>
CreateCudaEngine(std::chrono::seconds max_operation_duration,
                 const fs::path &custom_storage_root,
                 bool enable_regular_backend,
                 bool allow_posix_custom_storage,
                 size_t max_parallel_restores,
                 size_t cuda_worker_count,
                 size_t cuda_custom_storage_worker_count) {
  if (!enable_regular_backend && !allow_posix_custom_storage)
    throw std::invalid_argument(
        "PageBroker CUDA requires at least one enabled backend");
  if (max_parallel_restores == 0 ||
      max_parallel_restores >
          static_cast<size_t>(kMaximumParallelRestores)) {
    throw std::invalid_argument(
        "PageBroker CUDA max parallel restores must be between 1 and " +
        std::to_string(kMaximumParallelRestores));
  }
  if (enable_regular_backend &&
      (cuda_worker_count == 0 || cuda_worker_count > 128))
    throw std::invalid_argument(
        "PageBroker CUDA worker count must be between 1 and 128");
  if (allow_posix_custom_storage &&
      (cuda_custom_storage_worker_count == 0 ||
       cuda_custom_storage_worker_count > 128))
    throw std::invalid_argument(
        "PageBroker CUDA CustomStorage worker count must be between 1 and 128");
  if (max_operation_duration <= std::chrono::seconds::zero() ||
      max_operation_duration > std::chrono::hours(24))
    throw std::invalid_argument(
        "PageBroker CUDA operation timeout must be between 1 and 86400 seconds");
  if (allow_posix_custom_storage &&
      (!custom_storage_root.is_absolute() ||
       custom_storage_root.lexically_normal() != custom_storage_root))
    throw std::invalid_argument(
        "PageBroker CUDA CustomStorage root must be an absolute normalized path");
  // Configure this once before constructing either pool. Workers inherit the
  // same environment used by the in-process final dispatch check, and their
  // explicit --proc-root must name that identical procfs view.
  const fs::path process_root = ConfigureProcessRoot();
  std::unique_ptr<CudaRestoreWorkerPool> regular_worker_pool;
  if (enable_regular_backend) {
    regular_worker_pool = std::make_unique<PoolRestoreWorkerPool>(
        CudaWorkerPoolConfig{
            .worker_count = cuda_worker_count,
            .process_root = process_root.string(),
            .max_operation_seconds =
                static_cast<uint64_t>(max_operation_duration.count()),
            .rpc_timeout =
                std::chrono::duration_cast<std::chrono::milliseconds>(
                max_operation_duration) +
                std::chrono::seconds(30)});
  }
  std::unique_ptr<CudaRestoreWorkerPool> custom_storage_worker_pool;
  if (allow_posix_custom_storage) {
    custom_storage_worker_pool = std::make_unique<PoolRestoreWorkerPool>(
        CudaWorkerPoolConfig{
            .worker_count = cuda_custom_storage_worker_count,
            .private_socket_directory =
                "/run/pagebroker/cuda-custom-storage-workers",
            .process_root = process_root.string(),
            .storage_root = custom_storage_root.string(),
            .max_operation_seconds =
                static_cast<uint64_t>(max_operation_duration.count()),
            .rpc_timeout =
                std::chrono::duration_cast<std::chrono::milliseconds>(
                    max_operation_duration) +
                std::chrono::seconds(30),
            .require_custom_storage = true});
  }
  return std::make_unique<ServiceCudaEngine>(
      max_operation_duration, enable_regular_backend,
      allow_posix_custom_storage,
      max_parallel_restores,
      std::make_unique<cuda_operation::Service>(max_operation_duration),
      std::make_unique<SystemCudaTargetTerminator>(),
      std::make_unique<CuinterposeCoordinator>(
          fs::path(kCuinterposeCoordinator), process_root,
          max_operation_duration),
      std::move(regular_worker_pool),
      std::move(custom_storage_worker_pool));
}

std::unique_ptr<CudaEngine>
CreateCudaEngineForTesting(
    std::chrono::seconds max_operation_duration,
    bool allow_posix_custom_storage,
    size_t max_parallel_restores,
    std::unique_ptr<cuda_operation::Service> service,
    std::unique_ptr<CudaTargetTerminator> target_terminator,
    std::unique_ptr<CuinterposeCoordinator> cuinterpose,
    std::unique_ptr<CudaRestoreWorkerPool> worker_pool,
    bool enable_regular_backend,
    std::unique_ptr<CudaRestoreWorkerPool> custom_storage_worker_pool) {
  if (!enable_regular_backend && !allow_posix_custom_storage)
    throw std::invalid_argument(
        "PageBroker CUDA requires at least one enabled backend");
  if (max_parallel_restores == 0 ||
      max_parallel_restores >
          static_cast<size_t>(kMaximumParallelRestores)) {
    throw std::invalid_argument(
        "PageBroker CUDA max parallel restores must be between 1 and " +
        std::to_string(kMaximumParallelRestores));
  }
  if (target_terminator == nullptr)
    target_terminator = std::make_unique<SystemCudaTargetTerminator>();
  if (cuinterpose == nullptr)
    cuinterpose = std::make_unique<CuinterposeCoordinator>(
        fs::path(kCuinterposeCoordinator), ConfigureProcessRoot(),
        max_operation_duration);
  if (enable_regular_backend && worker_pool == nullptr)
    worker_pool =
        std::make_unique<InProcessRestoreWorkerPool>(service.get());
  if (allow_posix_custom_storage && custom_storage_worker_pool == nullptr)
    custom_storage_worker_pool =
        std::make_unique<InProcessRestoreWorkerPool>(service.get());
  return std::make_unique<ServiceCudaEngine>(
      max_operation_duration, enable_regular_backend,
      allow_posix_custom_storage,
      max_parallel_restores, std::move(service), std::move(target_terminator),
      std::move(cuinterpose), std::move(worker_pool),
      std::move(custom_storage_worker_pool));
}

} // namespace snapshot::pagebroker
