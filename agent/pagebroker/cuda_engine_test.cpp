// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <thread>
#include <tuple>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>

#include "cuda_engine.hpp"
#include "cuda_operation.h"
#include "cuda_worker_client.hpp"
#include "cuinterpose_coordinator.hpp"
#include "daemon_protocol.h"
#include "protocol.h"

namespace fs = std::filesystem;
using namespace std::chrono_literals;
using namespace snapshot::pagebroker;
namespace cuda_daemon = cuda_checkpoint_daemon;
namespace cuda_operation = cuda_checkpoint_operation;

namespace {

class FakeOperationService final : public cuda_operation::Service {
 public:
  FakeOperationService() : cuda_operation::Service(1s) {}

  bool Initialize(cuda_operation::InitializationMetrics* metrics,
                  std::string* error) override
  {
    *metrics = {};
    metrics->custom_storage_available = true;
    error->clear();
    return true;
  }

  cuda_daemon::Response ExecuteUncaptured(const cuda_daemon::Request& request) override
  {
    std::unique_lock lock(mutex_);
    ++execute_calls;
    if (request.action == cuda_daemon::Action::kLock && block_lock) {
      lock_entered = true;
      changed_.notify_all();
      changed_.wait(lock, [&] { return release_lock; });
      return {};
    }
    if (request.action != cuda_daemon::Action::kRestore)
      return execute ? execute(request) : cuda_daemon::Response{};
    ++restore_calls;
    if (execute)
      return execute(request);
    if (!block_restore)
      return restore_response;
    restore_entered = true;
    changed_.notify_all();
    changed_.wait(lock, [&] { return release_restore; });
    return restore_response;
  }

  CUresult BeginShutdown(const std::string&, std::string* error) override
  {
    std::function<void()> callback;
    {
      std::lock_guard lock(mutex_);
      ++begin_shutdown_calls;
      release_restore = true;
      release_lock = true;
      callback = on_begin_shutdown;
    }
    if (callback)
      callback();
    changed_.notify_all();
    error->clear();
    return CUDA_SUCCESS;
  }

  CUresult ReapExited(const std::string&, std::string* error) override
  {
    ++reap_calls;
    *error = reap_error;
    return reap_status;
  }

  CUresult TerminateRetainedTargets(const std::string&, std::string* error) override
  {
    ++terminate_retained_calls;
    error->clear();
    return CUDA_SUCCESS;
  }

  CUresult ReleaseAll() override
  {
    ++release_all_calls;
    return CUDA_SUCCESS;
  }

  bool WaitForRestore()
  {
    std::unique_lock lock(mutex_);
    return changed_.wait_for(lock, 2s, [&] { return restore_entered; });
  }

  void ReleaseRestore()
  {
    {
      std::lock_guard lock(mutex_);
      release_restore = true;
    }
    changed_.notify_all();
  }

  bool WaitForLock()
  {
    std::unique_lock lock(mutex_);
    return changed_.wait_for(lock, 2s, [&] { return lock_entered; });
  }

  void ReleaseLock()
  {
    {
      std::lock_guard lock(mutex_);
      release_lock = true;
    }
    changed_.notify_all();
  }

  std::mutex mutex_;
  std::condition_variable changed_;
  bool block_restore = false;
  bool restore_entered = false;
  bool release_restore = false;
  bool block_lock = false;
  bool lock_entered = false;
  bool release_lock = false;
  cuda_daemon::Response restore_response;
  std::function<cuda_daemon::Response(const cuda_daemon::Request&)> execute;
  std::function<void()> on_begin_shutdown;
  std::atomic<size_t> execute_calls{0};
  std::atomic<size_t> restore_calls{0};
  std::atomic<size_t> begin_shutdown_calls{0};
  std::atomic<size_t> reap_calls{0};
  std::atomic<size_t> terminate_retained_calls{0};
  std::atomic<size_t> release_all_calls{0};
  CUresult reap_status = CUDA_SUCCESS;
  std::string reap_error;
};

class FakeRestoreWorkerPool;
class FakeRestoreWorkerRetention;

class FakeRestoreWorkerLease final : public CudaRestoreWorkerLease {
 public:
  FakeRestoreWorkerLease(FakeRestoreWorkerPool* owner, size_t size);
  ~FakeRestoreWorkerLease() override;

  size_t size() const override { return size_; }
  CudaWorkerRpcResult Call(
      size_t worker_index,
      const cuda_daemon::Request& request) const override;
  void Poison(size_t worker_index) override;
  std::shared_ptr<CudaRestoreWorkerRetention> Retain(
      size_t worker_index) override;

 private:
  FakeRestoreWorkerPool* owner_;
  size_t size_;
};

class FakeRestoreWorkerPool final : public CudaRestoreWorkerPool {
 public:
  explicit FakeRestoreWorkerPool(size_t capacity)
      : capacity_(capacity), available_(capacity) {}

  size_t capacity() const override { return capacity_; }

  std::unique_ptr<CudaRestoreWorkerLease> TryAcquire(size_t weight) override
  {
    std::lock_guard lock(mutex_);
    acquired_weights.push_back(weight);
    if (fail_stop || weight > available_)
      return nullptr;
    available_ -= weight;
    return std::make_unique<FakeRestoreWorkerLease>(this, weight);
  }

  bool fail_stop_required() const override
  {
    std::lock_guard lock(mutex_);
    return fail_stop;
  }

  bool Shutdown(std::chrono::milliseconds, std::string* error) override
  {
    ++shutdown_calls;
    if (on_shutdown)
      on_shutdown();
    error->clear();
    return true;
  }

  CudaWorkerRpcResult Call(
      size_t worker_index, const cuda_daemon::Request& request)
  {
    std::function<CudaWorkerRpcResult(size_t, const cuda_daemon::Request&)>
        callback;
    {
      std::lock_guard lock(mutex_);
      calls.emplace_back(worker_index, request);
      callback = execute;
    }
    if (callback)
      return callback(worker_index, request);
    return {.status = CudaWorkerRpcStatus::kOk};
  }

  void Release(size_t weight)
  {
    std::lock_guard lock(mutex_);
    available_ += weight;
  }

  void Poison(size_t worker_index)
  {
    std::lock_guard lock(mutex_);
    poisoned_workers.push_back(worker_index);
    fail_stop = true;
  }

  std::shared_ptr<CudaRestoreWorkerRetention> Retain(size_t worker_index);

  void ReleaseRetention()
  {
    --retained_workers;
  }

  mutable std::mutex mutex_;
  std::vector<size_t> acquired_weights;
  std::vector<std::pair<size_t, cuda_daemon::Request>> calls;
  std::vector<size_t> poisoned_workers;
  std::function<CudaWorkerRpcResult(
      size_t, const cuda_daemon::Request&)> execute;
  std::function<void()> on_shutdown;
  bool fail_stop = false;
  std::atomic<size_t> shutdown_calls{0};
  std::atomic<size_t> retained_workers{0};

 private:
  size_t capacity_;
  size_t available_;
};

class FakeRestoreWorkerRetention final : public CudaRestoreWorkerRetention {
 public:
  explicit FakeRestoreWorkerRetention(FakeRestoreWorkerPool* owner)
      : owner_(owner) {}
  ~FakeRestoreWorkerRetention() override { owner_->ReleaseRetention(); }

 private:
  FakeRestoreWorkerPool* owner_;
};

std::shared_ptr<CudaRestoreWorkerRetention>
FakeRestoreWorkerPool::Retain(size_t)
{
  ++retained_workers;
  return std::make_shared<FakeRestoreWorkerRetention>(this);
}

FakeRestoreWorkerLease::FakeRestoreWorkerLease(
    FakeRestoreWorkerPool* owner, size_t size)
    : owner_(owner), size_(size) {}

FakeRestoreWorkerLease::~FakeRestoreWorkerLease()
{
  owner_->Release(size_);
}

CudaWorkerRpcResult FakeRestoreWorkerLease::Call(
    size_t worker_index, const cuda_daemon::Request& request) const
{
  if (worker_index >= size_)
    return {.status = CudaWorkerRpcStatus::kInvalidRequest,
            .error = "fake CUDA worker index is out of range"};
  return owner_->Call(worker_index, request);
}

void FakeRestoreWorkerLease::Poison(size_t worker_index)
{
  owner_->Poison(worker_index);
}

std::shared_ptr<CudaRestoreWorkerRetention>
FakeRestoreWorkerLease::Retain(size_t worker_index)
{
  if (worker_index >= size_)
    return nullptr;
  return owner_->Retain(worker_index);
}

class FakePinnedTarget final : public CudaPinnedTarget {
 public:
  FakePinnedTarget(cuda_daemon::Request request, std::string process_root)
      : request(std::move(request)), process_root(std::move(process_root)) {}

  uint32_t pid() const override { return request.pid; }

  cuda_daemon::Request request;
  std::string process_root;
};

class FakeTargetTerminator final : public CudaTargetTerminator {
 public:
  bool Pin(const std::vector<cuda_daemon::Request>& targets,
           const std::string& process_root, CudaPinnedTargets* pinned,
           std::string* error) override
  {
    ++pin_calls;
    if (!pin_succeeds) {
      *error = "injected target pin failure";
      return false;
    }
    pinned->clear();
    for (const auto& target : targets)
      pinned->push_back(
          std::make_shared<FakePinnedTarget>(target, process_root));
    error->clear();
    return true;
  }

  bool Terminate(const CudaPinnedTargets& targets,
                 std::chrono::milliseconds,
                 std::string* error) override
  {
    std::vector<uint32_t> pids;
    for (const auto& target : targets)
      pids.push_back(target->pid());
    calls.push_back(std::move(pids));
    if (on_terminate)
      on_terminate();
    error->clear();
    return succeeds;
  }

  bool Exited(const CudaPinnedTarget& target, bool* exited,
              std::string* error) override
  {
    const auto* pinned = dynamic_cast<const FakePinnedTarget*>(&target);
    if (pinned == nullptr) {
      *error = "unexpected fake pinned target";
      return false;
    }
    const auto state = cuda_daemon::InspectProcessIdentity(
        pinned->request, pinned->process_root, error);
    if (state == cuda_daemon::ProcessIdentityState::kIndeterminate)
      return false;
    *exited = state == cuda_daemon::ProcessIdentityState::kExitedOrReused;
    error->clear();
    return true;
  }

  std::vector<std::vector<uint32_t>> calls;
  std::function<void()> on_terminate;
  std::atomic<size_t> pin_calls{0};
  bool pin_succeeds = true;
  bool succeeds = true;
};

class FakeCuinterposeCoordinator final : public CuinterposeCoordinator {
 public:
  explicit FakeCuinterposeCoordinator(std::vector<std::string>* events)
      : CuinterposeCoordinator("/bin/false", "/proc", 1s), events_(events) {}

  bool ValidateEndpoints(const std::vector<CuinterposeTarget>&,
                         bool,
                         std::string* error) const override
  {
    error->clear();
    return true;
  }

  bool ReadState(const fs::path&,
                 uint32_t participant_count,
                 CuinterposeStateMetadata* metadata,
                 std::string* error) const override
  {
    *metadata = {
        .protocol_version = CUINTERPOSE_VERSION,
        .size_bytes = 123,
        .sha256 = "test-state",
        .participant_count = participant_count,
    };
    error->clear();
    return true;
  }

  CuinterposeResult ValidateState(const fs::path&,
                                  const std::atomic<bool>&) const override
  {
    return {.succeeded = true};
  }

  CuinterposeResult Prepare(const std::vector<CuinterposeTarget>&,
                            const fs::path&,
                            const std::atomic<bool>&,
                            bool reject_legacy_ipc) const override
  {
    prepare_reject_legacy_ipc.push_back(reject_legacy_ipc);
    events_->push_back(reject_legacy_ipc
                           ? "cuinterpose-prepare-custom-storage"
                           : "cuinterpose-prepare-regular");
    return reject_legacy_ipc ? custom_prepare_result
                             : regular_prepare_result;
  }

  CuinterposeResult Restore(const std::vector<CuinterposeTarget>&,
                            const fs::path&,
                            const std::atomic<bool>&) const override
  {
    events_->push_back("cuinterpose-restore");
    return restore_result;
  }

  CuinterposeResult restore_result{.succeeded = true};
  CuinterposeResult regular_prepare_result{.succeeded = true};
  CuinterposeResult custom_prepare_result{.succeeded = true};
  mutable std::vector<bool> prepare_reject_legacy_ipc;

 private:
  std::vector<std::string>* events_;
};

class ServiceCudaEngineTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / "pagebroker-cuda-engine-tests" /
            ::testing::UnitTest::GetInstance()->current_test_info()->name();
    fs::remove_all(root_);
    fs::create_directories(root_);
    if (const char* previous = std::getenv("CUDA_CHECKPOINT_PROC_ROOT"))
      previous_process_root_ = previous;
    ASSERT_EQ(setenv("CUDA_CHECKPOINT_PROC_ROOT", root_.c_str(), 1), 0);
  }

  void TearDown() override
  {
    if (previous_process_root_)
      EXPECT_EQ(setenv("CUDA_CHECKPOINT_PROC_ROOT", previous_process_root_->c_str(), 1), 0);
    else
      EXPECT_EQ(unsetenv("CUDA_CHECKPOINT_PROC_ROOT"), 0);
    fs::remove_all(root_);
  }

  void CreateIdentity(uint32_t pid, uint64_t start_time)
  {
    const fs::path process = root_ / std::to_string(pid);
    fs::create_directories(process / "root" / "snapshot-control");
    std::ofstream stat(process / "stat");
    stat << pid << " (pagebroker-test) S";
    for (size_t field = 1; field < 20; ++field)
      stat << ' ' << (field == 19 ? start_time : 0);
    stat << '\n';
    std::ofstream(process / "cgroup") << "0::/pagebroker-test\n";
  }

  CudaRestoreRequest Request(uint32_t pid, uint64_t start_time) const
  {
    CudaRestoreRequest request;
    request.set_storage_backend(v1::CUDA_STORAGE_BACKEND_REGULAR);
    request.set_uses_job_file(false);
    request.set_uses_cuinterpose(false);
    auto* target = request.add_targets();
    target->set_host_pid(pid);
    target->set_namespace_pid(pid);
    target->set_start_time_ticks(start_time);
    target->set_cgroup("0::/pagebroker-test\n");
    return request;
  }

  CudaRestoreRequest CustomStorageRequest(
      uint32_t pid, uint64_t start_time) const
  {
    auto request = Request(pid, start_time);
    request.set_storage_backend(
        v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
    request.mutable_targets(0)->add_selected_devices(
        "GPU-00000000-0000-0000-0000-000000000000");
    return request;
  }

  CudaCheckpointRequest CheckpointRequest(uint32_t pid, uint64_t start_time) const
  {
    CudaCheckpointRequest request;
    request.set_storage_backend(v1::CUDA_STORAGE_BACKEND_REGULAR);
    request.set_uses_job_file(false);
    request.set_uses_cuinterpose(false);
    auto* target = request.add_targets();
    target->set_host_pid(pid);
    target->set_namespace_pid(pid);
    target->set_start_time_ticks(start_time);
    target->set_cgroup("0::/pagebroker-test\n");
    return request;
  }

  CudaCheckpointRequest CustomStorageCheckpointRequest(
      uint32_t pid, uint64_t start_time) const
  {
    auto request = CheckpointRequest(pid, start_time);
    request.set_storage_backend(
        v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
    request.mutable_targets(0)->add_selected_devices(
        "GPU-00000000-0000-0000-0000-000000000000");
    return request;
  }

  std::pair<std::unique_ptr<CudaEngine>, FakeOperationService*> Engine(size_t capacity)
  {
    auto service = std::make_unique<FakeOperationService>();
    auto* observed = service.get();
    auto engine = CreateCudaEngineForTesting(
        30s, false, capacity, std::move(service),
        std::make_unique<FakeTargetTerminator>());
    return {std::move(engine), observed};
  }

  std::tuple<std::unique_ptr<CudaEngine>, FakeOperationService*, FakeTargetTerminator*>
  EngineWithTerminator(size_t capacity)
  {
    auto service = std::make_unique<FakeOperationService>();
    auto* observed_service = service.get();
    auto terminator = std::make_unique<FakeTargetTerminator>();
    auto* observed_terminator = terminator.get();
    auto engine = CreateCudaEngineForTesting(
        30s, false, capacity, std::move(service), std::move(terminator));
    return {std::move(engine), observed_service, observed_terminator};
  }

  CudaOperationResult RestoreWithAdmission(
      CudaEngine& engine, const CudaRestoreRequest& request)
  {
    auto admitted = engine.BeginRestore(
        request.storage_backend(), static_cast<size_t>(request.targets_size()));
    if (admitted.admission == nullptr)
      return admitted.operation;
    return engine.Restore(request, root_, *admitted.admission);
  }

  fs::path root_;
  std::optional<std::string> previous_process_root_;
};

TEST_F(ServiceCudaEngineTest, RegularCheckpointAllowsLegacyIpcPolicy)
{
  constexpr uint32_t pid = 41070;
  CreateIdentity(pid, 170);
  std::vector<std::string> events;
  auto service = std::make_unique<FakeOperationService>();
  std::vector<cuda_daemon::Request> operations;
  service->execute = [&](const cuda_daemon::Request& operation) {
    operations.push_back(operation);
    return cuda_daemon::Response{};
  };
  auto coordinator = std::make_unique<FakeCuinterposeCoordinator>(&events);
  auto* observed_coordinator = coordinator.get();
  observed_coordinator->custom_prepare_result = {
      .succeeded = false,
      .error = "legacy CUDA IPC is unsafe only for CustomStorage",
  };
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), std::move(coordinator));
  auto request = CheckpointRequest(pid, 170);
  request.set_uses_cuinterpose(true);
  auto admission = engine->BeginCheckpoint(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);

  const auto result =
      engine->Checkpoint(request, root_, *admission.admission);

  EXPECT_TRUE(result.succeeded) << result.error;
  EXPECT_EQ(observed_coordinator->prepare_reject_legacy_ipc,
            (std::vector<bool>{false}));
  EXPECT_EQ(events,
            (std::vector<std::string>{"cuinterpose-prepare-regular"}));
  ASSERT_GE(operations.size(), 2u);
  EXPECT_EQ(operations.front().action, cuda_daemon::Action::kLock);
  EXPECT_EQ(operations.front().transfer_buffer_count, 0u);
  EXPECT_EQ(operations.front().transfer_chunk_bytes, 0u);
  EXPECT_TRUE(operations.front().device_map.empty());
  EXPECT_TRUE(operations.front().storage_dir.empty());
  EXPECT_TRUE(operations.front().selected_devices.empty());
  EXPECT_TRUE(operations.front().pinned_storage_files.empty());
}

TEST_F(ServiceCudaEngineTest, CustomStorageCheckpointRejectsLegacyIpcPolicyBeforeNativeCuda)
{
  constexpr uint32_t pid = 41071;
  CreateIdentity(pid, 171);
  std::vector<std::string> events;
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto coordinator = std::make_unique<FakeCuinterposeCoordinator>(&events);
  auto* observed_coordinator = coordinator.get();
  observed_coordinator->custom_prepare_result = {
      .succeeded = false,
      .dispatched = false,
      .error = "legacy CUDA IPC prevents CustomStorage checkpoint",
  };
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), std::move(coordinator));
  auto request = CustomStorageCheckpointRequest(pid, 171);
  request.set_uses_cuinterpose(true);
  auto admission = engine->BeginCheckpoint(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);

  const auto result =
      engine->Checkpoint(request, root_, *admission.admission);

  EXPECT_FALSE(result.succeeded);
  EXPECT_FALSE(result.target_may_be_mutated);
  EXPECT_EQ(result.error, "legacy CUDA IPC prevents CustomStorage checkpoint");
  EXPECT_EQ(observed_service->execute_calls.load(), 0u);
  EXPECT_EQ(observed_coordinator->prepare_reject_legacy_ipc,
            (std::vector<bool>{true}));
  EXPECT_EQ(events, (std::vector<std::string>{
                        "cuinterpose-prepare-custom-storage"}));
}

TEST_F(ServiceCudaEngineTest, SaturatedRestoreFailsBusyBeforeMutation)
{
  CreateIdentity(41001, 101);
  CreateIdentity(41002, 102);
  auto [engine, service] = Engine(1);
  service->block_restore = true;
  CudaOperationResult first_result;
  std::thread first([&] { first_result = RestoreWithAdmission(*engine, Request(41001, 101)); });
  const bool entered = service->WaitForRestore();
  if (!entered) {
    service->ReleaseRestore();
    first.join();
  }
  ASSERT_TRUE(entered);

  const auto before = service->execute_calls.load();
  const auto started = std::chrono::steady_clock::now();
  const auto saturated = RestoreWithAdmission(*engine, Request(41002, 102));
  const auto elapsed = std::chrono::steady_clock::now() - started;

  EXPECT_FALSE(saturated.succeeded);
  EXPECT_EQ(saturated.failure_code, Failure::BUSY);
  EXPECT_FALSE(saturated.target_may_be_mutated);
  EXPECT_FALSE(saturated.fatal);
  EXPECT_LT(elapsed, 500ms);
  EXPECT_EQ(service->execute_calls.load(), before);

  service->ReleaseRestore();
  first.join();
  EXPECT_TRUE(first_result.succeeded);
}

TEST_F(ServiceCudaEngineTest, RegularAdmissionReservesExactRankWeightBeforeMutation)
{
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(4);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 3, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));

  auto first = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 3);
  ASSERT_NE(first.admission, nullptr);
  EXPECT_TRUE(first.operation.succeeded);

  auto saturated = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 2);
  EXPECT_EQ(saturated.admission, nullptr);
  EXPECT_EQ(saturated.operation.failure_code, Failure::BUSY);
  EXPECT_FALSE(saturated.operation.target_may_be_mutated);

  first.admission.reset();
  auto released = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 2);
  EXPECT_NE(released.admission, nullptr);
  EXPECT_EQ(observed_pool->acquired_weights,
            (std::vector<size_t>{3, 2, 2}));
}

TEST_F(ServiceCudaEngineTest, RegularAdmissionRejectsPermanentlyUndersizedPool)
{
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(4);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));

  const auto rejected = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 5);
  EXPECT_EQ(rejected.admission, nullptr);
  EXPECT_EQ(rejected.operation.failure_code, Failure::INVALID_REQUEST);
  EXPECT_FALSE(rejected.operation.target_may_be_mutated);
  EXPECT_TRUE(observed_pool->acquired_weights.empty());
}

TEST_F(ServiceCudaEngineTest, RegularRestoreAndUnlockUseStableWorkerPerRank)
{
  CreateIdentity(41040, 140);
  CreateIdentity(41041, 141);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(2);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41040, 140);
  auto* second = request.add_targets();
  second->set_host_pid(41041);
  second->set_namespace_pid(41041);
  second->set_start_time_ticks(141);
  second->set_cgroup("0::/pagebroker-test\n");

  auto admission = engine->BeginRestore(request.storage_backend(), 2);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored =
      engine->Restore(request, root_, *admission.admission);
  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(observed_service->execute_calls.load(), 0U);

  {
    std::lock_guard lock(observed_pool->mutex_);
    ASSERT_EQ(observed_pool->calls.size(), 4U);
    EXPECT_EQ(observed_pool->calls[0].second.action,
              cuda_daemon::Action::kRestore);
    EXPECT_EQ(observed_pool->calls[1].second.action,
              cuda_daemon::Action::kRestore);
    for (size_t index = 2; index < observed_pool->calls.size(); ++index) {
      EXPECT_EQ(observed_pool->calls[index].second.action,
                cuda_daemon::Action::kUnlock);
      const auto pid = observed_pool->calls[index].second.pid;
      const auto matching_restore = std::find_if(
          observed_pool->calls.begin(), observed_pool->calls.begin() + 2,
          [&](const auto& call) { return call.second.pid == pid; });
      ASSERT_NE(matching_restore, observed_pool->calls.begin() + 2);
      EXPECT_EQ(observed_pool->calls[index].first, matching_restore->first);
    }
  }
  EXPECT_EQ(observed_pool->retained_workers.load(), 2U);
  fs::remove_all(root_ / "41040");
  fs::remove_all(root_ / "41041");
  std::string error;
  EXPECT_TRUE(engine->ReapExited(&error)) << error;
  EXPECT_EQ(observed_pool->retained_workers.load(), 0U);
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 1U);
}

TEST_F(ServiceCudaEngineTest,
       RegularLaunchJobRestoreSerializesParticipantsInManifestOrder)
{
  CreateIdentity(41050, 150);
  CreateIdentity(41051, 151);
  CreateIdentity(41052, 152);
  const auto job_file = [&](uint32_t pid) {
    return root_ / std::to_string(pid) / "root" / "snapshot-control" /
           "cuda-checkpoint-job";
  };
  std::ofstream(job_file(41050)) << "shared-launch-job";
  fs::create_hard_link(job_file(41050), job_file(41051));
  fs::create_hard_link(job_file(41050), job_file(41052));
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(3);
  auto* observed_pool = pool.get();
  std::atomic<size_t> in_flight{0};
  std::atomic<size_t> max_in_flight{0};
  std::mutex order_mutex;
  std::vector<uint32_t> restore_order;
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& operation) {
    if (operation.action != cuda_daemon::Action::kRestore)
      return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
    const size_t current = ++in_flight;
    size_t observed = max_in_flight.load();
    while (current > observed &&
           !max_in_flight.compare_exchange_weak(observed, current)) {
    }
    {
      std::lock_guard lock(order_mutex);
      restore_order.push_back(operation.pid);
    }
    std::this_thread::sleep_for(20ms);
    --in_flight;
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41050, 150);
  request.set_uses_job_file(true);
  for (const auto& [pid, start_time] :
       {std::pair<uint32_t, uint64_t>{41051, 151}, {41052, 152}}) {
    auto* target = request.add_targets();
    target->set_host_pid(pid);
    target->set_namespace_pid(pid);
    target->set_start_time_ticks(start_time);
    target->set_cgroup("0::/pagebroker-test\n");
  }

  auto admission = engine->BeginRestore(request.storage_backend(), 3);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);

  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(max_in_flight.load(), 1U);
  EXPECT_EQ(restore_order, (std::vector<uint32_t>{41050, 41051, 41052}));
  EXPECT_EQ(observed_pool->retained_workers.load(), 3U);
}

TEST_F(ServiceCudaEngineTest,
       RegularLaunchJobRestoreOverlapsDistinctInodesAndOrdersEachGroup)
{
  for (const auto& [pid, start_time] : {
           std::pair<uint32_t, uint64_t>{41060, 160},
           {41061, 161}, {41062, 162}, {41063, 163}}) {
    CreateIdentity(pid, start_time);
  }
  const auto job_file = [&](uint32_t pid) {
    return root_ / std::to_string(pid) / "root" / "snapshot-control" /
           "cuda-checkpoint-job";
  };
  std::ofstream(job_file(41060)) << "launch-job-a";
  fs::create_hard_link(job_file(41060), job_file(41062));
  std::ofstream(job_file(41061)) << "launch-job-b";
  fs::create_hard_link(job_file(41061), job_file(41063));

  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(4);
  auto* observed_pool = pool.get();
  std::mutex overlap_mutex;
  std::condition_variable overlap_changed;
  size_t group_heads = 0;
  size_t in_flight = 0;
  size_t max_in_flight = 0;
  size_t group_a_in_flight = 0;
  size_t group_b_in_flight = 0;
  bool same_group_overlap = false;
  std::vector<uint32_t> group_a_order;
  std::vector<uint32_t> group_b_order;
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& operation) {
    if (operation.action != cuda_daemon::Action::kRestore)
      return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
    const bool group_a = operation.pid == 41060 || operation.pid == 41062;
    {
      std::unique_lock lock(overlap_mutex);
      auto& order = group_a ? group_a_order : group_b_order;
      auto& own_in_flight =
          group_a ? group_a_in_flight : group_b_in_flight;
      same_group_overlap = same_group_overlap || own_in_flight != 0;
      order.push_back(operation.pid);
      ++own_in_flight;
      ++in_flight;
      max_in_flight = std::max(max_in_flight, in_flight);
      if (order.size() == 1) {
        ++group_heads;
        overlap_changed.notify_all();
        if (!overlap_changed.wait_for(
                lock, 2s, [&] { return group_heads == 2; })) {
          --own_in_flight;
          --in_flight;
          return CudaWorkerRpcResult{
              .status = CudaWorkerRpcStatus::kTimeout,
              .error = "distinct regular launch jobs did not overlap"};
        }
      }
    }
    std::this_thread::sleep_for(20ms);
    {
      std::lock_guard lock(overlap_mutex);
      auto& own_in_flight =
          group_a ? group_a_in_flight : group_b_in_flight;
      --own_in_flight;
      --in_flight;
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41060, 160);
  request.set_uses_job_file(true);
  for (const auto& [pid, start_time] : {
           std::pair<uint32_t, uint64_t>{41061, 161},
           {41062, 162}, {41063, 163}}) {
    auto* target = request.add_targets();
    target->set_host_pid(pid);
    target->set_namespace_pid(pid);
    target->set_start_time_ticks(start_time);
    target->set_cgroup("0::/pagebroker-test\n");
  }

  auto admission = engine->BeginRestore(request.storage_backend(), 4);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);

  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(max_in_flight, 2U);
  EXPECT_FALSE(same_group_overlap);
  EXPECT_EQ(group_a_order, (std::vector<uint32_t>{41060, 41062}));
  EXPECT_EQ(group_b_order, (std::vector<uint32_t>{41061, 41063}));
}

TEST_F(ServiceCudaEngineTest, RegularRestoreWithoutLaunchJobOverlapsTargets)
{
  CreateIdentity(41064, 164);
  CreateIdentity(41065, 165);
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(2);
  auto* observed_pool = pool.get();
  std::mutex overlap_mutex;
  std::condition_variable overlap_changed;
  size_t restore_entries = 0;
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestore) {
      std::unique_lock lock(overlap_mutex);
      ++restore_entries;
      overlap_changed.notify_all();
      if (!overlap_changed.wait_for(lock, 2s,
                                    [&] { return restore_entries == 2; })) {
        return CudaWorkerRpcResult{
            .status = CudaWorkerRpcStatus::kTimeout,
            .error = "regular targets without launch jobs did not overlap"};
      }
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41064, 164);
  auto* second = request.add_targets();
  second->set_host_pid(41065);
  second->set_namespace_pid(41065);
  second->set_start_time_ticks(165);
  second->set_cgroup("0::/pagebroker-test\n");

  auto admission = engine->BeginRestore(request.storage_backend(), 2);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);

  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(restore_entries, 2U);
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageWorkersOverlapDistinctLaunchJobScopes)
{
  CreateIdentity(41042, 142);
  CreateIdentity(41043, 143);
  std::ofstream(root_ / "41042" / "root" / "snapshot-control" /
                "cuda-checkpoint-job") << "gms";
  std::ofstream(root_ / "41043" / "root" / "snapshot-control" /
                "cuda-checkpoint-job") << "engine";
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(2);
  auto* observed_pool = pool.get();
  std::mutex overlap_mutex;
  std::condition_variable overlap_changed;
  size_t restore_entries = 0;
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestore) {
      std::unique_lock lock(overlap_mutex);
      ++restore_entries;
      overlap_changed.notify_all();
      if (!overlap_changed.wait_for(lock, 2s,
                                    [&] { return restore_entries == 2; })) {
        return CudaWorkerRpcResult{
            .status = CudaWorkerRpcStatus::kTimeout,
            .error = "restore scopes did not overlap"};
      }
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false,
      std::move(pool));
  auto request = CustomStorageRequest(41042, 142);
  request.set_uses_job_file(true);
  auto* second = request.add_targets();
  second->set_host_pid(41043);
  second->set_namespace_pid(41043);
  second->set_start_time_ticks(143);
  second->set_cgroup("0::/pagebroker-test\n");
  second->add_selected_devices(
      "GPU-00000000-0000-0000-0000-000000000001");
  auto admission = engine->BeginRestore(request.storage_backend(), 2);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);
  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(restore_entries, 2u);
  EXPECT_EQ(observed_pool->retained_workers.load(), 2u);
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageBatchesSameLaunchJobInManifestOrder)
{
  CreateIdentity(41053, 153);
  CreateIdentity(41054, 154);
  const fs::path first_job = root_ / "41053" / "root" /
      "snapshot-control" / "cuda-checkpoint-job";
  const fs::path second_job = root_ / "41054" / "root" /
      "snapshot-control" / "cuda-checkpoint-job";
  std::ofstream(first_job) << "shared-launch-job";
  fs::create_hard_link(first_job, second_job);

  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false,
      std::move(pool));
  auto request = CustomStorageRequest(41053, 153);
  request.set_uses_job_file(true);
  auto* second = request.add_targets();
  second->set_host_pid(41054);
  second->set_namespace_pid(41054);
  second->set_start_time_ticks(154);
  second->set_cgroup("0::/pagebroker-test\n");
  second->add_selected_devices(
      "GPU-00000000-0000-0000-0000-000000000001");

  auto admission = engine->BeginRestore(request.storage_backend(), 2, 1);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);
  EXPECT_TRUE(restored.succeeded) << restored.error;

  std::lock_guard lock(observed_pool->mutex_);
  ASSERT_EQ(observed_pool->calls.size(), 3u);
  EXPECT_EQ(observed_pool->acquired_weights,
            (std::vector<size_t>{1}));
  EXPECT_EQ(observed_pool->calls[0].first, 0u);
  EXPECT_EQ(observed_pool->calls[0].second.action,
            cuda_daemon::Action::kRestoreBatch);
  ASSERT_EQ(observed_pool->calls[0].second.targets.size(), 2u);
  EXPECT_EQ(observed_pool->calls[0].second.targets[0].pid, 41053u);
  EXPECT_EQ(observed_pool->calls[0].second.targets[1].pid, 41054u);
  EXPECT_NE(observed_pool->calls[0].second.targets[0]
                .expected_job_file_inode,
            0u);
  EXPECT_EQ(observed_pool->calls[0].second.targets[0]
                .expected_job_file_device,
            observed_pool->calls[0].second.targets[1]
                .expected_job_file_device);
  EXPECT_EQ(observed_pool->calls[0].second.targets[0]
                .expected_job_file_inode,
            observed_pool->calls[0].second.targets[1]
                .expected_job_file_inode);
  EXPECT_EQ(observed_pool->calls[1].first, 0u);
  EXPECT_EQ(observed_pool->calls[1].second.action,
            cuda_daemon::Action::kUnlock);
  EXPECT_EQ(observed_pool->calls[1].second.pid, 41054u);
  EXPECT_EQ(observed_pool->calls[1].second.transfer_buffer_count, 0u);
  EXPECT_EQ(observed_pool->calls[1].second.transfer_chunk_bytes, 0u);
  EXPECT_TRUE(observed_pool->calls[1].second.device_map.empty());
  EXPECT_TRUE(observed_pool->calls[1].second.storage_dir.empty());
  EXPECT_TRUE(observed_pool->calls[1].second.selected_devices.empty());
  EXPECT_TRUE(observed_pool->calls[1].second.pinned_storage_files.empty());
  EXPECT_EQ(observed_pool->calls[1].second.expected_start_time_ticks,
            observed_pool->calls[0].second.targets[1]
                .expected_start_time_ticks);
  EXPECT_EQ(observed_pool->calls[1].second.expected_cgroup,
            observed_pool->calls[0].second.targets[1].expected_cgroup);
  EXPECT_EQ(observed_pool->calls[1].second.job_file,
            observed_pool->calls[0].second.targets[1].job_file);
  EXPECT_EQ(observed_pool->calls[1].second.expected_job_file_device,
            observed_pool->calls[0].second.targets[1]
                .expected_job_file_device);
  EXPECT_EQ(observed_pool->calls[1].second.expected_job_file_inode,
            observed_pool->calls[0].second.targets[1]
                .expected_job_file_inode);
  EXPECT_EQ(observed_pool->calls[2].first, 0u);
  EXPECT_EQ(observed_pool->calls[2].second.action,
            cuda_daemon::Action::kUnlock);
  EXPECT_EQ(observed_pool->calls[2].second.pid, 41053u);
  EXPECT_EQ(observed_pool->calls[2].second.transfer_buffer_count, 0u);
  EXPECT_EQ(observed_pool->calls[2].second.transfer_chunk_bytes, 0u);
  EXPECT_TRUE(observed_pool->calls[2].second.device_map.empty());
  EXPECT_TRUE(observed_pool->calls[2].second.storage_dir.empty());
  EXPECT_TRUE(observed_pool->calls[2].second.selected_devices.empty());
  EXPECT_TRUE(observed_pool->calls[2].second.pinned_storage_files.empty());
  EXPECT_EQ(observed_pool->calls[2].second.expected_start_time_ticks,
            observed_pool->calls[0].second.targets[0]
                .expected_start_time_ticks);
  EXPECT_EQ(observed_pool->calls[2].second.expected_cgroup,
            observed_pool->calls[0].second.targets[0].expected_cgroup);
  EXPECT_EQ(observed_pool->calls[2].second.job_file,
            observed_pool->calls[0].second.targets[0].job_file);
  EXPECT_EQ(observed_pool->calls[2].second.expected_job_file_device,
            observed_pool->calls[0].second.targets[0]
                .expected_job_file_device);
  EXPECT_EQ(observed_pool->calls[2].second.expected_job_file_inode,
            observed_pool->calls[0].second.targets[0]
                .expected_job_file_inode);
  EXPECT_EQ(observed_pool->retained_workers.load(), 1u);
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageRejectsDispatchGroupDriftBeforeWorkerCall)
{
  CreateIdentity(41066, 166);
  CreateIdentity(41067, 167);
  std::ofstream(root_ / "41066" / "root" / "snapshot-control" /
                "cuda-checkpoint-job") << "launch-job-a";
  std::ofstream(root_ / "41067" / "root" / "snapshot-control" /
                "cuda-checkpoint-job") << "launch-job-b";

  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false,
      std::move(pool));
  auto request = CustomStorageRequest(41066, 166);
  request.set_uses_job_file(true);
  auto* second = request.add_targets();
  second->set_host_pid(41067);
  second->set_namespace_pid(41067);
  second->set_start_time_ticks(167);
  second->set_cgroup("0::/pagebroker-test\n");
  second->add_selected_devices(
      "GPU-00000000-0000-0000-0000-000000000001");

  auto admission = engine->BeginRestore(request.storage_backend(), 2, 1);
  ASSERT_NE(admission.admission, nullptr);
  const auto restored = engine->Restore(request, root_, *admission.admission);

  EXPECT_FALSE(restored.succeeded);
  EXPECT_EQ(restored.failure_code, Failure::INVALID_REQUEST);
  EXPECT_FALSE(restored.target_may_be_mutated);
  EXPECT_EQ(restored.error,
            "CustomStorage restore dispatch-group count does not match "
            "admission");
  std::lock_guard lock(observed_pool->mutex_);
  EXPECT_EQ(observed_pool->acquired_weights, (std::vector<size_t>{1}));
  EXPECT_TRUE(observed_pool->calls.empty());
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageBatchesOverlapAcrossIndependentRestoreRequests)
{
  for (const auto& [pid, start_time] : {
           std::pair<uint32_t, uint64_t>{41055, 155}, {41056, 156},
           {41057, 157}, {41058, 158}}) {
    CreateIdentity(pid, start_time);
  }
  const auto job = [&](uint32_t pid) {
    return root_ / std::to_string(pid) / "root" / "snapshot-control" /
        "cuda-checkpoint-job";
  };
  std::ofstream(job(41055)) << "launch-job-a";
  fs::create_hard_link(job(41055), job(41056));
  std::ofstream(job(41057)) << "launch-job-b";
  fs::create_hard_link(job(41057), job(41058));

  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(2);
  auto* observed_pool = pool.get();
  std::mutex overlap_mutex;
  std::condition_variable overlap_changed;
  size_t batch_entries = 0;
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestoreBatch) {
      std::unique_lock lock(overlap_mutex);
      ++batch_entries;
      overlap_changed.notify_all();
      if (!overlap_changed.wait_for(lock, 2s,
                                    [&] { return batch_entries == 2; })) {
        return CudaWorkerRpcResult{
            .status = CudaWorkerRpcStatus::kTimeout,
            .error = "independent restore batches did not overlap"};
      }
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, true, 2, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false,
      std::move(pool));
  const auto make_request = [&](uint32_t first_pid, uint64_t first_start,
                                uint32_t second_pid, uint64_t second_start) {
    auto request = CustomStorageRequest(first_pid, first_start);
    request.set_uses_job_file(true);
    auto* second = request.add_targets();
    second->set_host_pid(second_pid);
    second->set_namespace_pid(second_pid);
    second->set_start_time_ticks(second_start);
    second->set_cgroup("0::/pagebroker-test\n");
    second->add_selected_devices(
        "GPU-00000000-0000-0000-0000-000000000001");
    return request;
  };
  const auto first_request = make_request(41055, 155, 41056, 156);
  const auto second_request = make_request(41057, 157, 41058, 158);
  auto first_admission =
      engine->BeginRestore(first_request.storage_backend(), 2, 1);
  auto second_admission =
      engine->BeginRestore(second_request.storage_backend(), 2, 1);
  ASSERT_NE(first_admission.admission, nullptr);
  ASSERT_NE(second_admission.admission, nullptr);

  CudaOperationResult first_result;
  CudaOperationResult second_result;
  std::thread first([&] {
    first_result = engine->Restore(
        first_request, root_, *first_admission.admission);
  });
  std::thread second([&] {
    second_result = engine->Restore(
        second_request, root_, *second_admission.admission);
  });
  first.join();
  second.join();

  EXPECT_TRUE(first_result.succeeded) << first_result.error;
  EXPECT_TRUE(second_result.succeeded) << second_result.error;
  EXPECT_EQ(batch_entries, 2u);
  EXPECT_EQ(observed_pool->acquired_weights,
            (std::vector<size_t>{1, 1}));
  EXPECT_EQ(observed_pool->retained_workers.load(), 2u);
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageRejectsOversizedBatchDescriptorSetBeforeDispatch)
{
  CreateIdentity(41059, 159);
  CreateIdentity(41060, 160);
  const fs::path first_job = root_ / "41059" / "root" /
      "snapshot-control" / "cuda-checkpoint-job";
  const fs::path second_job = root_ / "41060" / "root" /
      "snapshot-control" / "cuda-checkpoint-job";
  std::ofstream(first_job) << "shared-launch-job";
  fs::create_hard_link(first_job, second_job);

  DirectRestoreProcesses processes;
  for (const uint32_t pid : {41059u, 41060u}) {
    DirectRestoreProcess process{
        .namespace_pid = pid,
        .directory_name = "process-nspid-" + std::to_string(pid)};
    process.carriers.resize(33);
    processes.push_back(std::move(process));
  }
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(2);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false,
      std::move(pool));
  auto request = CustomStorageRequest(41059, 159);
  request.set_uses_job_file(true);
  auto* second = request.add_targets();
  second->set_host_pid(41060);
  second->set_namespace_pid(41060);
  second->set_start_time_ticks(160);
  second->set_cgroup("0::/pagebroker-test\n");
  second->add_selected_devices(
      "GPU-00000000-0000-0000-0000-000000000001");
  auto admission = engine->BeginRestore(request.storage_backend(), 2, 1);
  ASSERT_NE(admission.admission, nullptr);

  const auto restored = engine->Restore(
      request, root_, *admission.admission, &processes);
  EXPECT_FALSE(restored.succeeded);
  EXPECT_FALSE(restored.target_may_be_mutated);
  EXPECT_EQ(restored.failure_code, Failure::INVALID_REQUEST);
  EXPECT_NE(restored.error.find("64-carrier"), std::string::npos);
  EXPECT_TRUE(observed_pool->calls.empty());
}

TEST_F(ServiceCudaEngineTest, SuccessfulWorkerOutputHasBoundedTargetIdentity)
{
  CreateIdentity(41044, 144);
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  pool->execute = [](size_t, const cuda_daemon::Request& operation) {
    return CudaWorkerRpcResult{
        .status = CudaWorkerRpcStatus::kOk,
        .response = {.output = operation.action == cuda_daemon::Action::kRestore
                                   ? "worker metric\n"
                                   : ""}};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41044, 144);
  auto admission = engine->BeginRestore(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);
  testing::internal::CaptureStdout();
  const auto restored = engine->Restore(request, root_, *admission.admission);
  const std::string output = testing::internal::GetCapturedStdout();
  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_NE(output.find("\"event\":\"pagebroker_cuda_worker_output\""),
            std::string::npos);
  EXPECT_NE(output.find("\"target_index\":0"), std::string::npos);
  EXPECT_NE(output.find("\"pid\":41044"), std::string::npos);
  EXPECT_NE(output.find("\"start_time_ticks\":144"), std::string::npos);
  EXPECT_NE(output.find("worker metric\\n"), std::string::npos);
}

TEST_F(ServiceCudaEngineTest, UnknownWorkerOutcomePoisonsPoolBeforeLeaseRelease)
{
  CreateIdentity(41042, 142);
  auto service = std::make_unique<FakeOperationService>();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  observed_pool->execute = [](size_t, const cuda_daemon::Request& request) {
    if (request.action == cuda_daemon::Action::kRestore) {
      return CudaWorkerRpcResult{
          .status = CudaWorkerRpcStatus::kTimeout,
          .error = "injected live timeout",
          .unknown_outcome = true};
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 2, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41042, 142);
  auto admission = engine->BeginRestore(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);

  const auto restored =
      engine->Restore(request, root_, *admission.admission);
  EXPECT_FALSE(restored.succeeded);
  EXPECT_TRUE(restored.fatal);
  EXPECT_EQ(restored.error, "injected live timeout");
  ASSERT_EQ(observed_pool->poisoned_workers.size(), 1U);
  EXPECT_EQ(observed_pool->poisoned_workers.front(), 0U);
  admission.admission.reset();

  const auto refused = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_EQ(refused.admission, nullptr);
  EXPECT_TRUE(refused.operation.fatal);
}

TEST_F(ServiceCudaEngineTest,
       MonitorDetectedPoolFailureClosesPreviouslyAdmittedDispatch)
{
  CreateIdentity(41043, 143);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 2, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(41043, 143);
  auto admission = engine->BeginRestore(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);

  {
    std::lock_guard lock(observed_pool->mutex_);
    observed_pool->fail_stop = true;
  }
  const auto restored =
      engine->Restore(request, root_, *admission.admission);

  EXPECT_FALSE(restored.succeeded);
  EXPECT_TRUE(restored.fatal);
  EXPECT_FALSE(restored.target_may_be_mutated);
  EXPECT_EQ(observed_service->execute_calls.load(), 0U);
  {
    std::lock_guard lock(observed_pool->mutex_);
    EXPECT_TRUE(observed_pool->calls.empty());
  }
  EXPECT_TRUE(engine->ShutdownRequired());
}

TEST_F(ServiceCudaEngineTest,
       MixedModePostDispatchRegularWorkerLossRequiresDaemonRestart)
{
  CreateIdentity(41048, 148);
  CreateIdentity(41049, 149);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  observed_pool->execute = [](size_t, const cuda_daemon::Request& request) {
    if (request.action == cuda_daemon::Action::kRestore) {
      return CudaWorkerRpcResult{
          .status = CudaWorkerRpcStatus::kTimeout,
          .error = "injected post-dispatch worker loss",
          .unknown_outcome = true};
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  auto engine = CreateCudaEngineForTesting(
      30s, true, 2, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));

  const auto custom =
      RestoreWithAdmission(*engine, CustomStorageRequest(41048, 148));
  ASSERT_TRUE(custom.succeeded) << custom.error;
  EXPECT_EQ(observed_service->restore_calls.load(), 1U);

  const auto regular = RestoreWithAdmission(*engine, Request(41049, 149));
  EXPECT_FALSE(regular.succeeded);
  EXPECT_TRUE(regular.target_may_be_mutated);
  EXPECT_TRUE(regular.fatal);
  EXPECT_EQ(regular.error, "injected post-dispatch worker loss");
  EXPECT_TRUE(engine->ShutdownRequired());
  ASSERT_EQ(observed_pool->poisoned_workers.size(), 1U);

  const auto custom_during_exit = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE, 1);
  EXPECT_EQ(custom_during_exit.admission, nullptr);
  EXPECT_TRUE(custom_during_exit.operation.fatal);
}

TEST_F(ServiceCudaEngineTest,
       CustomStorageOnlyRejectsRegularWithoutConstructingWorkerPool)
{
  CreateIdentity(41044, 144);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 2, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, nullptr, false);

  EXPECT_THROW(
      engine->BeginRestore(v1::CUDA_STORAGE_BACKEND_REGULAR, 1),
      std::invalid_argument);
  EXPECT_THROW(
      engine->BeginCheckpoint(v1::CUDA_STORAGE_BACKEND_REGULAR, 1),
      std::invalid_argument);

  const auto restored =
      RestoreWithAdmission(*engine, CustomStorageRequest(41044, 144));
  EXPECT_TRUE(restored.succeeded) << restored.error;
  EXPECT_EQ(observed_service->restore_calls.load(), 1U);

  fs::remove_all(root_ / "41044");
  std::string error;
  EXPECT_TRUE(engine->ReapExited(&error)) << error;
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
}

TEST_F(ServiceCudaEngineTest,
       MixedModeWorkerFailureDrainsRegularTargetsThenRequiresDaemonRestart)
{
  CreateIdentity(41045, 145);
  CreateIdentity(41046, 146);
  CreateIdentity(41047, 147);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto terminator = std::make_unique<FakeTargetTerminator>();
  auto* observed_terminator = terminator.get();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 2, std::move(service), std::move(terminator), nullptr,
      std::move(pool));

  const auto custom =
      RestoreWithAdmission(*engine, CustomStorageRequest(41045, 145));
  ASSERT_TRUE(custom.succeeded) << custom.error;
  const auto regular = RestoreWithAdmission(*engine, Request(41046, 146));
  ASSERT_TRUE(regular.succeeded) << regular.error;

  {
    std::lock_guard lock(observed_pool->mutex_);
    observed_pool->fail_stop = true;
  }
  fs::remove_all(root_ / "41046");
  std::string error;
  EXPECT_TRUE(engine->ReapExited(&error)) << error;
  EXPECT_TRUE(engine->ShutdownRequired());
  ASSERT_EQ(observed_terminator->calls.size(), 1U);
  EXPECT_EQ(observed_terminator->calls.front(),
            (std::vector<uint32_t>{41046}));
  EXPECT_EQ(observed_pool->retained_workers.load(), 0U);
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 1U);
  EXPECT_EQ(observed_service->terminate_retained_calls.load(), 0U);
  EXPECT_EQ(observed_service->release_all_calls.load(), 0U);

  const auto regular_refused =
      engine->BeginRestore(v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_EQ(regular_refused.admission, nullptr);
  EXPECT_TRUE(regular_refused.operation.fatal);
  EXPECT_FALSE(regular_refused.operation.target_may_be_mutated);
  EXPECT_EQ(regular_refused.operation.failure_code, Failure::CUDA_ERROR);

  const auto custom_after_failure = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE, 1);
  EXPECT_EQ(custom_after_failure.admission, nullptr);
  EXPECT_TRUE(custom_after_failure.operation.fatal);
  EXPECT_EQ(observed_service->restore_calls.load(), 1U);
  EXPECT_EQ(observed_terminator->calls.size(), 1U);

  fs::remove_all(root_ / "41045");
  fs::remove_all(root_ / "41047");
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
}

TEST_F(ServiceCudaEngineTest, DuplicateActiveTargetFailsBusyBeforeMutation)
{
  CreateIdentity(41003, 103);
  auto [engine, service] = Engine(2);
  service->block_restore = true;
  CudaOperationResult first_result;
  std::thread first([&] { first_result = RestoreWithAdmission(*engine, Request(41003, 103)); });
  const bool entered = service->WaitForRestore();
  if (!entered) {
    service->ReleaseRestore();
    first.join();
  }
  ASSERT_TRUE(entered);

  const auto before = service->execute_calls.load();
  const auto duplicate = RestoreWithAdmission(*engine, Request(41003, 103));
  EXPECT_EQ(duplicate.failure_code, Failure::BUSY);
  EXPECT_FALSE(duplicate.target_may_be_mutated);
  EXPECT_EQ(service->execute_calls.load(), before);

  service->ReleaseRestore();
  first.join();
  EXPECT_TRUE(first_result.succeeded);
}

TEST_F(ServiceCudaEngineTest, RestorePinFailureFailsBeforeNativeMutation)
{
  constexpr uint32_t pid = 41050;
  CreateIdentity(pid, 150);
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto terminator = std::make_unique<FakeTargetTerminator>();
  auto* observed_terminator = terminator.get();
  observed_terminator->pin_succeeds = false;
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service), std::move(terminator), nullptr,
      std::move(pool));

  const auto result = RestoreWithAdmission(*engine, Request(pid, 150));

  EXPECT_FALSE(result.succeeded);
  EXPECT_FALSE(result.target_may_be_mutated);
  EXPECT_NE(result.error.find("injected target pin failure"),
            std::string::npos);
  EXPECT_EQ(observed_terminator->pin_calls.load(), 1U);
  EXPECT_EQ(observed_service->execute_calls.load(), 0U);
  EXPECT_TRUE(observed_pool->calls.empty());
}

TEST_F(ServiceCudaEngineTest, DirectRestorePassesExactPinnedCarrierDescriptors)
{
  constexpr uint32_t pid = 41030;
  constexpr uint64_t start_time = 130;
  CreateIdentity(pid, start_time);
  const fs::path carrier_path = root_ / "carrier.bin";
  std::ofstream(carrier_path) << "carrier";
  const int carrier_fd = open(
      carrier_path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  ASSERT_GE(carrier_fd, 0);
  struct stat status{};
  ASSERT_EQ(fstat(carrier_fd, &status), 0);
  DirectRestoreProcesses processes;
  DirectRestoreProcess process{
      .namespace_pid = pid,
      .directory_name = "process-nspid-" + std::to_string(pid),
      .directory = FileDescriptor(open(root_.c_str(), O_RDONLY | O_DIRECTORY |
                                                        O_CLOEXEC)),
      .manifest = FileDescriptor(open(carrier_path.c_str(), O_RDONLY |
                                                               O_CLOEXEC))};
  process.carriers.push_back({
      .filename = "device-0000.bin",
      .descriptor = FileDescriptor(carrier_fd),
      .size = static_cast<uintmax_t>(status.st_size),
      .device = static_cast<uint64_t>(status.st_dev),
      .inode = static_cast<uint64_t>(status.st_ino)});
  processes.push_back(std::move(process));

  auto service = std::make_unique<FakeOperationService>();
  auto* observed = service.get();
  bool saw_exact = false;
  observed->execute = [&](const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestore) {
      EXPECT_EQ(operation.pinned_storage_files.size(), 1u);
      if (!operation.pinned_storage_files.empty()) {
        const auto& file = operation.pinned_storage_files.front();
        saw_exact = file.filename == "device-0000.bin" &&
                    file.descriptor_fd == carrier_fd &&
                    file.size == static_cast<uint64_t>(status.st_size) &&
                    file.device == static_cast<uint64_t>(status.st_dev) &&
                    file.inode == static_cast<uint64_t>(status.st_ino);
      }
    }
    return cuda_daemon::Response{};
  };
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, true, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), nullptr, std::move(pool));
  auto request = Request(pid, start_time);
  request.set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  request.mutable_targets(0)->add_selected_devices(
      "GPU-00000000-0000-0000-0000-000000000000");
  auto admission = engine->BeginRestore(
      request.storage_backend(), static_cast<size_t>(request.targets_size()));
  ASSERT_NE(admission.admission, nullptr);
  EXPECT_TRUE(engine->Restore(request, root_, *admission.admission,
                              &processes).succeeded);
  EXPECT_TRUE(saw_exact);
  EXPECT_TRUE(observed_pool->acquired_weights.empty());
}

TEST_F(ServiceCudaEngineTest, RestoreFailsBusyWhileCheckpointOwnsOperationGate)
{
  CreateIdentity(41005, 105);
  CreateIdentity(41006, 106);
  auto [engine, service] = Engine(2);
  service->block_lock = true;
  auto checkpoint_admission = engine->BeginCheckpoint(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  ASSERT_NE(checkpoint_admission.admission, nullptr);
  CudaOperationResult checkpoint_result;
  std::thread checkpoint([&] {
    checkpoint_result = engine->Checkpoint(
        CheckpointRequest(41005, 105), root_,
        *checkpoint_admission.admission);
  });
  const bool entered = service->WaitForLock();
  if (!entered) {
    service->ReleaseLock();
    checkpoint.join();
  }
  ASSERT_TRUE(entered);

  const auto before = service->execute_calls.load();
  const auto restore = RestoreWithAdmission(*engine, Request(41006, 106));
  EXPECT_EQ(restore.failure_code, Failure::BUSY);
  EXPECT_FALSE(restore.target_may_be_mutated);
  EXPECT_EQ(service->execute_calls.load(), before);

  service->ReleaseLock();
  checkpoint.join();
  EXPECT_TRUE(checkpoint_result.succeeded);
}

TEST_F(ServiceCudaEngineTest, CheckpointAdmissionCanBeReleasedByAnotherThread)
{
  auto [engine, service] = Engine(1);
  (void)service;
  CheckpointAdmissionResult checkpoint;
  std::thread acquire([&] {
    checkpoint = engine->BeginCheckpoint(
        v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  });
  acquire.join();
  ASSERT_NE(checkpoint.admission, nullptr);

  std::thread release([&] { checkpoint.admission.reset(); });
  release.join();

  const auto restore = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_NE(restore.admission, nullptr);
  EXPECT_TRUE(restore.operation.succeeded) << restore.operation.error;
}

TEST_F(ServiceCudaEngineTest, RestoreAdmissionCanBeReleasedByAnotherThread)
{
  auto [engine, service] = Engine(1);
  (void)service;
  RestoreAdmissionResult restore;
  std::thread acquire([&] {
    restore = engine->BeginRestore(
        v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  });
  acquire.join();
  ASSERT_NE(restore.admission, nullptr);

  std::thread release([&] { restore.admission.reset(); });
  release.join();

  const auto checkpoint = engine->BeginCheckpoint(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_NE(checkpoint.admission, nullptr);
  EXPECT_TRUE(checkpoint.operation.succeeded) << checkpoint.operation.error;
}

TEST_F(ServiceCudaEngineTest,
       CheckpointWaitsForAllRestoreAdmissionsReleasedAcrossThreads)
{
  auto [engine, service] = Engine(2);
  (void)service;
  auto first = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  auto second = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  ASSERT_NE(first.admission, nullptr);
  ASSERT_NE(second.admission, nullptr);

  std::thread release_first([&] { first.admission.reset(); });
  release_first.join();

  const auto blocked = engine->BeginCheckpoint(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_EQ(blocked.admission, nullptr);
  EXPECT_EQ(blocked.operation.failure_code, Failure::BUSY);
  EXPECT_FALSE(blocked.operation.target_may_be_mutated);

  std::thread release_second([&] { second.admission.reset(); });
  release_second.join();

  const auto admitted = engine->BeginCheckpoint(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_NE(admitted.admission, nullptr);
  EXPECT_TRUE(admitted.operation.succeeded) << admitted.operation.error;
}

TEST_F(ServiceCudaEngineTest, ShutdownCancelsActiveRestoreAndRejectsNewAdmission)
{
  constexpr uint32_t pid = 41004;
  CreateIdentity(pid, 104);
  auto [engine, service, terminator] = EngineWithTerminator(1);
  service->block_restore = true;
  service->restore_response = {
      .cuda_status = CUDA_ERROR_OPERATING_SYSTEM,
      .flags = cuda_daemon::kResponseFatal,
      .error = "cancelled for shutdown",
  };
  service->on_begin_shutdown = [&] { fs::remove_all(root_ / std::to_string(pid)); };
  CudaOperationResult active_result;
  std::thread active([&] { active_result = RestoreWithAdmission(*engine, Request(pid, 104)); });
  const bool entered = service->WaitForRestore();
  if (!entered) {
    service->ReleaseRestore();
    active.join();
  }
  ASSERT_TRUE(entered);

  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  active.join();
  EXPECT_TRUE(active_result.fatal);
  EXPECT_TRUE(active_result.target_may_be_mutated);
  EXPECT_EQ(service->begin_shutdown_calls.load(), 1);
  ASSERT_EQ(terminator->calls.size(), 1u);
  EXPECT_EQ(terminator->calls.front(), (std::vector<uint32_t>{pid}));

  const auto before = service->execute_calls.load();
  const auto rejected = RestoreWithAdmission(*engine, Request(pid, 104));
  EXPECT_TRUE(rejected.fatal);
  EXPECT_FALSE(rejected.target_may_be_mutated);
  EXPECT_EQ(service->execute_calls.load(), before);

  EXPECT_TRUE(engine->Shutdown(&error)) << error;
  EXPECT_EQ(service->terminate_retained_calls.load(), 1);
  EXPECT_EQ(service->release_all_calls.load(), 1);
}

TEST_F(ServiceCudaEngineTest,
       BeginShutdownStopsBlockedRegularWorkerAfterTargetTermination)
{
  constexpr uint32_t pid = 41048;
  CreateIdentity(pid, 148);
  auto service = std::make_unique<FakeOperationService>();
  auto terminator = std::make_unique<FakeTargetTerminator>();
  auto* observed_terminator = terminator.get();
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  std::mutex blocked_mutex;
  std::condition_variable blocked_changed;
  bool entered = false;
  bool released = false;
  std::vector<std::string> shutdown_events;
  observed_terminator->on_terminate = [&] {
    shutdown_events.push_back("terminate-target");
  };
  observed_pool->execute = [&](size_t, const cuda_daemon::Request& request) {
    if (request.action == cuda_daemon::Action::kRestore) {
      std::unique_lock lock(blocked_mutex);
      entered = true;
      blocked_changed.notify_all();
      blocked_changed.wait(lock, [&] { return released; });
    }
    return CudaWorkerRpcResult{.status = CudaWorkerRpcStatus::kOk};
  };
  observed_pool->on_shutdown = [&] {
    shutdown_events.push_back("shutdown-workers");
    {
      std::lock_guard lock(blocked_mutex);
      released = true;
    }
    blocked_changed.notify_all();
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service), std::move(terminator), nullptr,
      std::move(pool));

  CudaOperationResult restore_result;
  std::thread restore([&] {
    restore_result = RestoreWithAdmission(*engine, Request(pid, 148));
  });
  {
    std::unique_lock lock(blocked_mutex);
    ASSERT_TRUE(blocked_changed.wait_for(lock, 2s, [&] { return entered; }));
  }

  const auto started = std::chrono::steady_clock::now();
  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  EXPECT_LT(std::chrono::steady_clock::now() - started, 1s);
  restore.join();
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 1U);
  ASSERT_FALSE(observed_terminator->calls.empty());
  EXPECT_EQ(observed_terminator->calls.front(),
            (std::vector<uint32_t>{pid}));
  ASSERT_GE(shutdown_events.size(), 2U);
  EXPECT_EQ(shutdown_events[0], "terminate-target");
  EXPECT_EQ(shutdown_events[1], "shutdown-workers");

  EXPECT_TRUE(engine->Shutdown(&error)) << error;
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 1U);
}

TEST_F(ServiceCudaEngineTest,
       BeginShutdownRetainsWorkersWhenTargetTerminationIsInconclusive)
{
  constexpr uint32_t pid = 41049;
  CreateIdentity(pid, 149);
  auto service = std::make_unique<FakeOperationService>();
  auto terminator = std::make_unique<FakeTargetTerminator>();
  auto* observed_terminator = terminator.get();
  observed_terminator->succeeds = false;
  auto pool = std::make_unique<FakeRestoreWorkerPool>(1);
  auto* observed_pool = pool.get();
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service), std::move(terminator), nullptr,
      std::move(pool));
  ASSERT_TRUE(RestoreWithAdmission(*engine, Request(pid, 149)).succeeded);
  ASSERT_EQ(observed_pool->retained_workers.load(), 1U);

  std::string error;
  EXPECT_FALSE(engine->BeginShutdown(&error));
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 0U);
  EXPECT_EQ(observed_pool->retained_workers.load(), 1U);

  observed_terminator->succeeds = true;
  fs::remove_all(root_ / std::to_string(pid));
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  EXPECT_EQ(observed_pool->shutdown_calls.load(), 1U);
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
}

TEST_F(ServiceCudaEngineTest, PartialRegularRestoreRetainsEverySiblingForShutdown)
{
  CreateIdentity(41007, 107);
  CreateIdentity(41008, 108);
  auto [engine, service, terminator] = EngineWithTerminator(1);
  service->execute = [&](const cuda_daemon::Request& operation) {
    if (operation.pid == 41008) {
      return cuda_daemon::Response{
          .cuda_status = CUDA_ERROR_INVALID_VALUE,
          .error = "second rank restore failed",
      };
    }
    return cuda_daemon::Response{};
  };
  auto request = Request(41007, 107);
  auto* second = request.add_targets();
  second->set_host_pid(41008);
  second->set_namespace_pid(41008);
  second->set_start_time_ticks(108);
  second->set_cgroup("0::/pagebroker-test\n");

  const auto result = RestoreWithAdmission(*engine, request);
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.target_may_be_mutated);
  EXPECT_TRUE(result.fatal);

  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  ASSERT_EQ(terminator->calls.size(), 1u);
  EXPECT_EQ(terminator->calls.front(), (std::vector<uint32_t>{41007, 41008}));
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
}

TEST_F(ServiceCudaEngineTest,
       RetainedCheckpointSourceIsNeverTerminatedDuringShutdown)
{
  CreateIdentity(41009, 109);
  auto [engine, service, terminator] = EngineWithTerminator(1);
  service->execute = [](const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kCheckpoint) {
      return cuda_daemon::Response{
          .cuda_status = CUDA_ERROR_INVALID_VALUE,
          .error = "checkpoint failed after lock",
      };
    }
    return cuda_daemon::Response{};
  };
  auto request = CheckpointRequest(41009, 109);
  auto admission = engine->BeginCheckpoint(request.storage_backend(), 1);
  ASSERT_NE(admission.admission, nullptr);

  const auto result = engine->Checkpoint(request, root_, *admission.admission);
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.target_may_be_mutated);
  EXPECT_TRUE(result.fatal);

  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  EXPECT_TRUE(terminator->calls.empty());
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
  EXPECT_TRUE(terminator->calls.empty());
}

TEST_F(ServiceCudaEngineTest,
       ActiveCheckpointSourceIsNeverTerminatedDuringShutdown)
{
  constexpr uint32_t pid = 41031;
  CreateIdentity(pid, 131);
  auto [engine, service, terminator] = EngineWithTerminator(1);
  service->block_lock = true;
  auto admission = engine->BeginCheckpoint(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  ASSERT_NE(admission.admission, nullptr);
  CudaOperationResult checkpoint_result;
  std::thread checkpoint([&] {
    checkpoint_result = engine->Checkpoint(
        CheckpointRequest(pid, 131), root_, *admission.admission);
  });
  const bool entered = service->WaitForLock();
  if (!entered) {
    service->ReleaseLock();
    checkpoint.join();
  }
  ASSERT_TRUE(entered);

  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  EXPECT_TRUE(terminator->calls.empty());
  checkpoint.join();
  EXPECT_TRUE(checkpoint_result.succeeded) << checkpoint_result.error;
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
  EXPECT_TRUE(terminator->calls.empty());
}

TEST_F(ServiceCudaEngineTest, PartialUnlockRetainsEveryRestoredSiblingForShutdown)
{
  CreateIdentity(41010, 110);
  CreateIdentity(41011, 111);
  auto [engine, service, terminator] = EngineWithTerminator(1);
  size_t unlocks = 0;
  service->execute = [&](const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kUnlock && ++unlocks == 2) {
      return cuda_daemon::Response{
          .cuda_status = CUDA_ERROR_INVALID_VALUE,
          .error = "second unlock failed",
      };
    }
    return cuda_daemon::Response{};
  };
  auto request = Request(41010, 110);
  auto* second = request.add_targets();
  second->set_host_pid(41011);
  second->set_namespace_pid(41011);
  second->set_start_time_ticks(111);
  second->set_cgroup("0::/pagebroker-test\n");

  const auto result = RestoreWithAdmission(*engine, request);
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.target_may_be_mutated);
  EXPECT_TRUE(result.fatal);
  EXPECT_EQ(unlocks, 2u);

  std::string error;
  EXPECT_TRUE(engine->BeginShutdown(&error)) << error;
  ASSERT_EQ(terminator->calls.size(), 1u);
  EXPECT_EQ(terminator->calls.front(), (std::vector<uint32_t>{41010, 41011}));
  EXPECT_TRUE(engine->Shutdown(&error)) << error;
}

TEST_F(ServiceCudaEngineTest, SuccessfulRestoreRemainsReservedUntilReaped)
{
  constexpr uint32_t pid = 41012;
  constexpr uint64_t start_time = 112;
  CreateIdentity(pid, start_time);
  auto [engine, service] = Engine(2);

  const auto first = RestoreWithAdmission(*engine, Request(pid, start_time));
  ASSERT_TRUE(first.succeeded) << first.error;
  const auto before_duplicate = service->execute_calls.load();

  const auto duplicate = RestoreWithAdmission(*engine, Request(pid, start_time));
  EXPECT_FALSE(duplicate.succeeded);
  EXPECT_EQ(duplicate.failure_code, Failure::BUSY);
  EXPECT_FALSE(duplicate.target_may_be_mutated);
  EXPECT_EQ(service->execute_calls.load(), before_duplicate);

  fs::remove_all(root_ / std::to_string(pid));
  std::string error;
  ASSERT_TRUE(engine->ReapExited(&error)) << error;
  CreateIdentity(pid, start_time);

  const auto after_reap = RestoreWithAdmission(*engine, Request(pid, start_time));
  EXPECT_TRUE(after_reap.succeeded) << after_reap.error;
}

TEST_F(ServiceCudaEngineTest, CuinterposeRestoreRunsAfterNativeUnlock)
{
  constexpr uint32_t pid = 41013;
  CreateIdentity(pid, 113);
  std::vector<std::string> events;
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  observed_service->execute = [&](const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestore)
      events.push_back("native-restore");
    else if (operation.action == cuda_daemon::Action::kUnlock)
      events.push_back("native-unlock");
    return cuda_daemon::Response{};
  };
  auto coordinator = std::make_unique<FakeCuinterposeCoordinator>(&events);
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), std::move(coordinator));
  auto request = Request(pid, 113);
  request.set_uses_cuinterpose(true);
  auto* state = request.mutable_cuinterpose_state();
  state->set_protocol_version(CUINTERPOSE_VERSION);
  state->set_size_bytes(123);
  state->set_sha256("test-state");
  state->set_participant_count(1);

  const auto result = RestoreWithAdmission(*engine, request);

  ASSERT_TRUE(result.succeeded) << result.error;
  EXPECT_EQ(events, (std::vector<std::string>{
                        "native-restore", "native-unlock", "cuinterpose-restore"}));
}

TEST_F(ServiceCudaEngineTest, CuinterposeFailureAfterUnlockCleansUpTarget)
{
  constexpr uint32_t pid = 41014;
  CreateIdentity(pid, 114);
  std::vector<std::string> events;
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  observed_service->execute = [&](const cuda_daemon::Request& operation) {
    if (operation.action == cuda_daemon::Action::kRestore)
      events.push_back("native-restore");
    else if (operation.action == cuda_daemon::Action::kUnlock)
      events.push_back("native-unlock");
    return cuda_daemon::Response{};
  };
  auto terminator = std::make_unique<FakeTargetTerminator>();
  auto* observed_terminator = terminator.get();
  auto coordinator = std::make_unique<FakeCuinterposeCoordinator>(&events);
  coordinator->restore_result = {
      .succeeded = false,
      .dispatched = true,
      .error = "cuinterpose restore failed",
  };
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service), std::move(terminator),
      std::move(coordinator));
  auto request = Request(pid, 114);
  request.set_uses_cuinterpose(true);
  auto* state = request.mutable_cuinterpose_state();
  state->set_protocol_version(CUINTERPOSE_VERSION);
  state->set_size_bytes(123);
  state->set_sha256("test-state");
  state->set_participant_count(1);

  const auto result = RestoreWithAdmission(*engine, request);

  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.target_may_be_mutated);
  EXPECT_EQ(result.error, "cuinterpose restore failed");
  EXPECT_EQ(events, (std::vector<std::string>{
                        "native-restore", "native-unlock", "cuinterpose-restore"}));
  ASSERT_EQ(observed_terminator->calls.size(), 1u);
  EXPECT_EQ(observed_terminator->calls.front(), (std::vector<uint32_t>{pid}));
}

TEST_F(ServiceCudaEngineTest, CuinterposeRestoreRejectsOldProtocolMetadataBeforeNativeCuda)
{
  constexpr uint32_t pid = 41072;
  CreateIdentity(pid, 172);
  std::vector<std::string> events;
  auto service = std::make_unique<FakeOperationService>();
  auto* observed_service = service.get();
  auto coordinator = std::make_unique<FakeCuinterposeCoordinator>(&events);
  auto engine = CreateCudaEngineForTesting(
      30s, false, 1, std::move(service),
      std::make_unique<FakeTargetTerminator>(), std::move(coordinator));
  auto request = Request(pid, 172);
  request.set_uses_cuinterpose(true);
  auto* state = request.mutable_cuinterpose_state();
  state->set_protocol_version(CUINTERPOSE_VERSION - 1);
  state->set_size_bytes(123);
  state->set_sha256("test-state");
  state->set_participant_count(1);

  const auto result = RestoreWithAdmission(*engine, request);

  EXPECT_FALSE(result.succeeded);
  EXPECT_FALSE(result.target_may_be_mutated);
  EXPECT_EQ(result.error, "cuinterpose state metadata mismatch");
  EXPECT_EQ(observed_service->execute_calls.load(), 0u);
  EXPECT_TRUE(events.empty());
}

TEST_F(ServiceCudaEngineTest, ReapExitedDoesNotDiscardIdentityError)
{
  auto [engine, service] = Engine(1);
  service->reap_error = "target identity is inconclusive";
  std::string error;
  EXPECT_FALSE(engine->ReapExited(&error));
  EXPECT_EQ(error, service->reap_error);
  EXPECT_EQ(service->reap_calls.load(), 1);
  EXPECT_FALSE(engine->ShutdownRequired());

  auto admitted = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_NE(admitted.admission, nullptr);
}

TEST_F(ServiceCudaEngineTest, ReapReleaseFailureRequiresFailStop)
{
  auto [engine, service] = Engine(1);
  service->reap_status = CUDA_ERROR_OPERATING_SYSTEM;
  std::string error;

  EXPECT_FALSE(engine->ReapExited(&error));
  EXPECT_TRUE(engine->ShutdownRequired());
  EXPECT_EQ(service->reap_calls.load(), 1);

  const auto refused = engine->BeginRestore(
      v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  EXPECT_EQ(refused.admission, nullptr);
  EXPECT_TRUE(refused.operation.fatal);
  EXPECT_EQ(refused.operation.error, "PageBroker CUDA engine is shutting down");
}

TEST(CudaEngineFactoryTest, CustomStorageRequiresNormalizedAbsoluteRoot)
{
  EXPECT_THROW(
      CreateCudaEngine(30s, "relative/../staging", false, true, 1, 0, 1),
      std::invalid_argument);
}

}  // namespace
