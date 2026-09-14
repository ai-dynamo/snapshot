// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <filesystem>
#include <memory>
#include <string>
#include <vector>

#include "pagebroker_types.hpp"
#include "direct_restore_source.hpp"

namespace cuda_checkpoint_operation {
class Service;
}
namespace cuda_checkpoint_daemon {
struct Request;
}
namespace snapshot::pagebroker {
class CuinterposeCoordinator;
class CudaRestoreWorkerPool;
struct CudaWorkerRpcResult;
class CudaRestoreWorkerRetention;
class CudaPinnedTarget;
}

namespace snapshot::pagebroker {

struct CudaOperationResult {
  bool succeeded = false;
  bool target_may_be_mutated = false;
  bool fatal = false;
  Failure::Code failure_code = Failure::CUDA_ERROR;
  size_t target_count = 0;
  std::string error;
  bool has_cuinterpose_state = false;
  uint32_t cuinterpose_protocol_version = 0;
  uint64_t cuinterpose_state_size = 0;
  std::string cuinterpose_state_sha256;
  uint32_t cuinterpose_participant_count = 0;
};

// Opaque PageBroker-owned reservation held from pre-CRIU admission until the
// matching CUDA restore starts or the transaction is aborted. Implementations
// may also use it to exclude checkpoint operations across the boundary.
class RestoreAdmission {
 public:
  virtual ~RestoreAdmission() = default;
};

class CheckpointAdmission {
 public:
  virtual ~CheckpointAdmission() = default;
};

struct CheckpointAdmissionResult {
  std::unique_ptr<CheckpointAdmission> admission;
  CudaOperationResult operation;
};

struct RestoreAdmissionResult {
  std::unique_ptr<RestoreAdmission> admission;
  CudaOperationResult operation;
};

// CudaEngine is the PageBroker-owned CUDA execution boundary. Broker tests use
// a fake implementation; production constructs the CUDA 13.4 implementation
// when either the regular or CustomStorage backend is enabled.
class CudaEngine {
 public:
  virtual ~CudaEngine() = default;
  virtual CheckpointAdmissionResult BeginCheckpoint(
      CudaStorageBackend storage_backend, size_t target_count) = 0;
  virtual CudaOperationResult Checkpoint(
      const CudaCheckpointRequest& request,
      const std::filesystem::path& staging_directory,
      CheckpointAdmission& admission) = 0;
  RestoreAdmissionResult BeginRestore(
      CudaStorageBackend storage_backend, size_t target_count) {
    return BeginRestore(storage_backend, target_count, target_count);
  }
  // CustomStorage batches targets sharing a launch-job identity onto one
  // worker. The caller derives the expected count from immutable staged
  // artifact state; Restore revalidates it against live identities before
  // dispatch.
  virtual RestoreAdmissionResult BeginRestore(
      CudaStorageBackend storage_backend, size_t target_count,
      size_t expected_dispatch_group_count) = 0;
  virtual CudaOperationResult Restore(
      const CudaRestoreRequest& request, const std::filesystem::path& staging_directory,
      RestoreAdmission& admission,
      const DirectRestoreProcesses* direct_processes = nullptr) = 0;
  // Release retained primary-context references after their identity-matched
  // restored targets have exited.
  virtual bool ReapExited(std::string* error) = 0;
  // Close admission and dispatch immediately after an irrevocable result.
  // This is intentionally nonblocking; the daemon's normal shutdown phase
  // performs cancellation, identity termination, and context release.
  virtual void FailStop() noexcept = 0;
  // True when the complete engine must stop. A failed worker generation is
  // drained by its backend owner before daemon shutdown; PageBroker then
  // exits so the DaemonSet can provide a complete prewarmed replacement.
  virtual bool ShutdownRequired() const = 0;
  // Stop admission, cancel active transfers, and terminate only
  // identity-matched restored workloads before the daemon waits for request
  // handlers. Checkpoint source workloads remain caller-owned and survive.
  virtual bool BeginShutdown(std::string* error) = 0;
  // Releasing retained primary-context references under a live restored target
  // can fault that target. Graceful shutdown must terminate identity-matched
  // retained targets before releasing their contexts.
  virtual bool Shutdown(std::string* error) = 0;
};

class CudaPinnedTarget {
 public:
  virtual ~CudaPinnedTarget() = default;
  virtual uint32_t pid() const = 0;
};

using CudaPinnedTargets = std::vector<std::shared_ptr<CudaPinnedTarget>>;

class CudaTargetTerminator {
 public:
  virtual ~CudaTargetTerminator() = default;
  virtual bool Pin(
      const std::vector<cuda_checkpoint_daemon::Request>& targets,
      const std::string& process_root,
      CudaPinnedTargets* pinned,
      std::string* error) = 0;
  virtual bool Terminate(const CudaPinnedTargets& targets,
                         std::chrono::milliseconds timeout,
                         std::string* error) = 0;
  virtual bool Exited(const CudaPinnedTarget& target, bool* exited,
                      std::string* error) = 0;
};

// Small execution boundary between the PageBroker transaction engine and its
// prewarmed worker processes. A lease owns an exact, stable worker set for the
// complete native restore/unlock sequence. CustomStorage carrier descriptors
// are transferred to its separate worker generation over the private socket.
class CudaRestoreWorkerLease {
 public:
  virtual ~CudaRestoreWorkerLease() = default;
  virtual size_t size() const = 0;
  virtual CudaWorkerRpcResult Call(
      size_t worker_index,
      const cuda_checkpoint_daemon::Request& request) const = 0;
  // A transport/protocol failure after admission makes the worker unsafe for
  // reuse even if its process has not exited yet.
  virtual void Poison(size_t worker_index) = 0;
  // Keeps fail-stop ownership associated with a worker after the capacity
  // lease is released and until the matching restored target exits.
  virtual std::shared_ptr<CudaRestoreWorkerRetention> Retain(
      size_t worker_index) = 0;
};

class CudaRestoreWorkerRetention {
 public:
  virtual ~CudaRestoreWorkerRetention() = default;
};

class CudaRestoreWorkerPool {
 public:
  virtual ~CudaRestoreWorkerPool() = default;
  virtual size_t capacity() const = 0;
  virtual std::unique_ptr<CudaRestoreWorkerLease> TryAcquire(size_t weight) = 0;
  virtual bool fail_stop_required() const = 0;
  virtual bool Shutdown(std::chrono::milliseconds timeout,
                        std::string* error) = 0;
};

std::unique_ptr<CudaEngine> CreateCudaEngine(
    std::chrono::seconds max_operation_duration,
    const std::filesystem::path& custom_storage_root,
    // The regular backend owns the prewarmed worker subprocess generation.
    bool enable_regular_backend,
    bool allow_posix_custom_storage,
    size_t max_parallel_restores,
    size_t cuda_worker_count,
    size_t cuda_custom_storage_worker_count);

// Deterministic seam for exercising the production ServiceCudaEngine without
// requiring a CUDA device. The supplied service has the same lifetime and
// shutdown contract as the production cuda_checkpoint_operation::Service.
// Passing enable_regular_backend=false leaves the worker pool absent.
std::unique_ptr<CudaEngine> CreateCudaEngineForTesting(
    std::chrono::seconds max_operation_duration,
    bool allow_posix_custom_storage,
    size_t max_parallel_restores,
    std::unique_ptr<cuda_checkpoint_operation::Service> service,
    std::unique_ptr<CudaTargetTerminator> target_terminator = nullptr,
    std::unique_ptr<CuinterposeCoordinator> cuinterpose = nullptr,
    std::unique_ptr<CudaRestoreWorkerPool> worker_pool = nullptr,
    bool enable_regular_backend = true,
    std::unique_ptr<CudaRestoreWorkerPool> custom_storage_worker_pool =
        nullptr);

}  // namespace snapshot::pagebroker
