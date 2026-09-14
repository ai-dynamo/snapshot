// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>

#include <atomic>
#include <condition_variable>
#include <cerrno>
#include <cstring>
#include <filesystem>
#include <fcntl.h>
#include <fstream>
#include <functional>
#include <future>
#include <mutex>
#include <optional>
#include <stdexcept>
#include <string>
#include <thread>

#include "broker.hpp"
#include "daemon.hpp"
#include "posix_copy_engine.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

constexpr uintmax_t kTestMaxStagingBytes = 1 << 20;

TEST(DaemonLoggingNameTest, NamesDirectRestoreRequestAndResponse)
{
  Request request;
  request.mutable_direct_restore();
  EXPECT_STREQ(RequestCommandName(request.command_case()), "direct_restore");

  Response response;
  response.mutable_staged_restore_directory()->set_image_directory("/staged");
  EXPECT_STREQ(ResponseResultName(request.command_case(), response),
               "direct_restore_staged");

  request.clear_direct_restore();
  request.mutable_staged_restore();
  EXPECT_STREQ(ResponseResultName(request.command_case(), response),
               "staged_restore");

  request.clear_staged_restore();
  request.mutable_reference_regular_restore();
  EXPECT_STREQ(RequestCommandName(request.command_case()),
               "reference_regular_restore");
  EXPECT_STREQ(ResponseResultName(request.command_case(), response),
               "regular_restore_referenced");
}

TEST(DaemonMaintenanceScheduleTest, PollsCudaWithoutAcceleratingTransactionTTL)
{
  using Clock = DaemonMaintenanceSchedule::Clock;
  const auto start = Clock::time_point{};
  DaemonMaintenanceSchedule schedule(start);
  EXPECT_TRUE(schedule.TransactionsDue(start));
  EXPECT_TRUE(schedule.CudaDue(start));
  EXPECT_FALSE(schedule.TransactionsDue(start + std::chrono::seconds(1)));
  EXPECT_TRUE(schedule.CudaDue(start + std::chrono::seconds(1)));
  EXPECT_FALSE(schedule.TransactionsDue(start + std::chrono::seconds(119)));
  EXPECT_TRUE(schedule.CudaDue(start + std::chrono::seconds(119)));
  EXPECT_TRUE(schedule.TransactionsDue(start + std::chrono::minutes(2)));
}

TEST(DaemonReadinessTest,
     RequiresCompletedInitializationAndLiveListenerAndWithdrawsOnShutdown)
{
  const fs::path root =
      fs::temp_directory_path() / "pagebroker-daemon-readiness-test";
  fs::remove_all(root);
  fs::create_directories(root);
  const fs::path socket_path = root / "pagebroker.sock";

  FileDescriptor listener(socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0));
  ASSERT_GE(listener.get(), 0);
  sockaddr_un address {};
  address.sun_family = AF_UNIX;
  ASSERT_LT(socket_path.string().size(), sizeof(address.sun_path));
  std::strcpy(address.sun_path, socket_path.c_str());
  ASSERT_EQ(bind(listener.get(), reinterpret_cast<sockaddr*>(&address),
                 sizeof(address)),
            0);
  ASSERT_EQ(listen(listener.get(), 4), 0);

  // A listening socket alone is not Ready. RunDaemon publishes only after
  // CUDA initialization and every configured worker pool finishes prewarm.
  EXPECT_EQ(ProbeDaemonReady(socket_path), ExitCode::FAILURE);
  std::error_code error;
  ASSERT_TRUE(PublishDaemonReadiness(socket_path, &error)) << error.message();
  EXPECT_EQ(ProbeDaemonReady(socket_path), ExitCode::SUCCESS);

  // Shutdown/fail-stop withdraws readiness before potentially long CUDA
  // target termination and context-release work.
  WithdrawDaemonReadiness(socket_path);
  EXPECT_EQ(ProbeDaemonReady(socket_path), ExitCode::FAILURE);

  // A crash can leave the marker behind; the live socket check prevents the
  // stale file from making a replacement Pod appear Ready.
  listener = FileDescriptor(-1);
  fs::remove(socket_path);
  ASSERT_TRUE(PublishDaemonReadiness(socket_path, &error)) << error.message();
  EXPECT_EQ(ProbeDaemonReady(socket_path), ExitCode::FAILURE);
  WithdrawDaemonReadiness(socket_path);
  fs::remove_all(root);
}

void WriteCustomStorage(const fs::path& process,
                        size_t carrier_bytes,
                        char value = 'x')
{
  fs::create_directories(process);
  std::ofstream(process / "manifest.txt")
      << "version 3\ndevice_count 1\ndevice 0 "
         "GPU-00000000-0000-0000-0000-000000000000 "
      << carrier_bytes << " device-0000.bin " << std::string(64, '0')
      << "\n";
  std::ofstream(process / "device-0000.bin")
      << std::string(carrier_bytes, value);
}

class FakeCudaEngine final : public CudaEngine {
 public:
  class RestoreLease final : public RestoreAdmission {};
  class CheckpointLease final : public CheckpointAdmission {};

  CheckpointAdmissionResult BeginCheckpoint(
      CudaStorageBackend, size_t target_count) override
  {
    ++begin_checkpoint_calls;
    last_begin_checkpoint_target_count = target_count;
    if (!begin_checkpoint_result.succeeded)
      return {.operation = begin_checkpoint_result};
    return {.admission = std::make_unique<CheckpointLease>(),
            .operation = begin_checkpoint_result};
  }

  CudaOperationResult Checkpoint(
      const CudaCheckpointRequest&, const fs::path& staging_directory,
      CheckpointAdmission&) override
  {
    ++checkpoint_calls;
    if (checkpoint_fn)
      return checkpoint_fn(staging_directory);
    last_staging_directory = staging_directory;
    return checkpoint_result;
  }

  RestoreAdmissionResult BeginRestore(
      CudaStorageBackend, size_t target_count,
      size_t expected_dispatch_group_count) override
  {
    ++begin_restore_calls;
    last_begin_restore_target_count = target_count;
    last_begin_restore_dispatch_group_count = expected_dispatch_group_count;
    if (fail_stop_requested.load())
      return {.operation = {.fatal = true,
                            .error = "CUDA engine is fail-stopped"}};
    if (!begin_restore_result.succeeded)
      return {.operation = begin_restore_result};
    return {.admission = std::make_unique<RestoreLease>(), .operation = begin_restore_result};
  }

  CudaOperationResult Restore(
      const CudaRestoreRequest&, const fs::path& staging_directory,
      RestoreAdmission&,
      const DirectRestoreProcesses* direct_processes) override
  {
    if (fail_stop_requested.load())
      return {.target_may_be_mutated = false,
              .fatal = true,
              .error = "CUDA engine is fail-stopped"};
    ++restore_calls;
    last_direct_carrier_fds.clear();
    if (direct_processes != nullptr) {
      for (const auto& process : *direct_processes) {
        for (const auto& carrier : process.carriers)
          last_direct_carrier_fds.push_back(carrier.descriptor.get());
      }
    }
    if (restore_fn)
      return restore_fn(staging_directory);
    last_staging_directory = staging_directory;
    return restore_result;
  }

  bool Shutdown(std::string* error) override
  {
    ++shutdown_calls;
    if (shutdown_succeeds)
      return true;
    if (error != nullptr)
      *error = "shutdown failed";
    return false;
  }

  bool ShutdownRequired() const override { return shutdown_required; }

  void FailStop() noexcept override { fail_stop_requested.store(true); }

  bool ReapExited(std::string* error) override
  {
    ++reap_exited_calls;
    if (reap_exited_succeeds) {
      if (error != nullptr)
        error->clear();
      return true;
    }
    if (error != nullptr)
      *error = "reap exited failed";
    return false;
  }

  bool BeginShutdown(std::string* error) override
  {
    const size_t call = ++begin_shutdown_calls;
    if (begin_shutdown_fn)
      return begin_shutdown_fn(call, error);
    if (begin_shutdown_succeeds) {
      if (error != nullptr)
        error->clear();
      return true;
    }
    if (error != nullptr)
      *error = "begin shutdown failed";
    return false;
  }

  CudaOperationResult checkpoint_result{.succeeded = true, .target_count = 1};
  CudaOperationResult begin_checkpoint_result{.succeeded = true};
  CudaOperationResult begin_restore_result{.succeeded = true};
  CudaOperationResult restore_result{.succeeded = true, .target_count = 1};
  std::function<CudaOperationResult(const fs::path&)> checkpoint_fn;
  std::function<CudaOperationResult(const fs::path&)> restore_fn;
  std::function<bool(size_t, std::string*)> begin_shutdown_fn;
  std::atomic<size_t> checkpoint_calls{0};
  std::atomic<size_t> begin_checkpoint_calls{0};
  std::atomic<size_t> last_begin_checkpoint_target_count{0};
  std::atomic<size_t> begin_restore_calls{0};
  std::atomic<size_t> last_begin_restore_target_count{0};
  std::atomic<size_t> last_begin_restore_dispatch_group_count{0};
  std::atomic<size_t> restore_calls{0};
  std::atomic<size_t> shutdown_calls{0};
  std::atomic<size_t> begin_shutdown_calls{0};
  std::atomic<size_t> reap_exited_calls{0};
  std::atomic<bool> fail_stop_requested{false};
  bool begin_shutdown_succeeds = true;
  bool reap_exited_succeeds = true;
  bool shutdown_succeeds = true;
  bool shutdown_required = false;
  fs::path last_staging_directory;
  std::vector<int> last_direct_carrier_fds;
};

class FakeChangingTransferEngine final : public TransferEngine {
 public:
  explicit FakeChangingTransferEngine(uintmax_t copied_bytes)
      : copied_bytes_(copied_bytes) {}
  TransferEngineType type() const override { return TransferEngineType::POSIX_COPY; }
  uintmax_t RestoreSize(const StorageBackend&) const override { return admitted_bytes; }
  uintmax_t StageRestore(
      const StorageBackend&, const Path& destination,
      uintmax_t) const override
  {
    fs::create_directories(destination);
    std::ofstream(destination / "image") << "changed";
    return copied_bytes_;
  }
  void ValidateCheckpointDestination(const StorageBackend&) const override {}
  bool CheckpointDestinationConflicts(const StorageBackend&) const override { return false; }
  void PublishCheckpoint(const Path&, const StorageBackend&) const override
  {
    throw std::logic_error("unexpected checkpoint publish");
  }
  uintmax_t CopyDirectory(const Path&, const Path&) const override
  {
    throw std::logic_error("unexpected directory copy");
  }

  static constexpr uintmax_t admitted_bytes = 8;

 private:
  uintmax_t copied_bytes_;
};

class FakeBlockingTransferEngine final : public TransferEngine {
 public:
  TransferEngineType type() const override
  {
    return TransferEngineType::POSIX_COPY;
  }

  uintmax_t RestoreSize(const StorageBackend&) const override
  {
    return kBytes;
  }

  uintmax_t StageRestore(
      const StorageBackend&, const Path& destination,
      uintmax_t) const override
  {
    {
      std::unique_lock lock(mutex_);
      stage_entered_ = true;
      stage_entered_cv_.notify_all();
      release_cv_.wait(lock, [this] { return released_; });
    }
    fs::create_directories(destination);
    std::ofstream(destination / "image") << "image";
    return kBytes;
  }

  void ValidateCheckpointDestination(const StorageBackend&) const override {}
  bool CheckpointDestinationConflicts(const StorageBackend&) const override
  {
    return false;
  }
  void PublishCheckpoint(const Path&, const StorageBackend&) const override
  {
    throw std::logic_error("unexpected checkpoint publish");
  }
  uintmax_t CopyDirectory(const Path&, const Path&) const override
  {
    throw std::logic_error("unexpected directory copy");
  }

  bool WaitUntilStageEntered(std::chrono::seconds timeout) const
  {
    std::unique_lock lock(mutex_);
    return stage_entered_cv_.wait_for(
        lock, timeout, [this] { return stage_entered_; });
  }

  void Release()
  {
    {
      std::lock_guard lock(mutex_);
      released_ = true;
    }
    release_cv_.notify_all();
  }

 private:
  static constexpr uintmax_t kBytes = 5;
  mutable std::mutex mutex_;
  mutable std::condition_variable stage_entered_cv_;
  mutable std::condition_variable release_cv_;
  mutable bool stage_entered_ = false;
  mutable bool released_ = false;
};

class BrokerTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / "pagebroker-daemon-tests" /
            ::testing::UnitTest::GetInstance()->current_test_info()->name();
    fs::remove_all(root_);
    source_ = root_ / "storage" / "source";
    fs::create_directories(source_);
    std::ofstream(source_ / "image") << "image";
    broker_.emplace(root_ / "tmpfs", root_ / "storage", kTestMaxStagingBytes);
  }

  void TearDown() override { fs::remove_all(root_); }

  Request RequestFor(const std::string& id)
  {
    Request request;
    request.set_request_id("request-" + id + "-" + std::to_string(++request_number_));
    request.set_transaction_id(id);
    return request;
  }

  void Configure(StorageBackend* storage, IOEngine* engine, const fs::path& directory)
  {
    storage->mutable_filesystem()->set_directory(directory.string());
    engine->mutable_posix_copy();
  }

  void ConfigureRestoreIdentity(
      RestoreIdentity* identity,
      const std::string& container_id =
          "containerd://0123456789abcdef")
  {
    identity->set_pod_uid("pod-uid");
    identity->set_destination_container("main");
    identity->set_content_uid("content-uid");
    identity->set_source_container("main");
    identity->set_container_id(container_id);
  }

  Broker& broker() { return *broker_; }

  FakeCudaEngine& EnableCuda(
      std::optional<size_t> direct_restore_descriptor_budget = std::nullopt,
      StageReadyGate::FailureForTesting stage_ready_failure_for_testing = {})
  {
    broker_.reset();
    auto engine = std::make_unique<FakeCudaEngine>();
    cuda_engine_ = engine.get();
    broker_.emplace(root_ / "tmpfs", root_ / "storage", kTestMaxStagingBytes,
                    std::move(engine), nullptr,
                    direct_restore_descriptor_budget,
                    std::move(stage_ready_failure_for_testing));
    return *cuda_engine_;
  }

  Response StageRestore(const std::string& id)
  {
    auto restore = RequestFor(id);
    Configure(
        restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
        source_);
    return broker().HandleRequest(restore);
  }

  Response ReferenceRegularRestore(
      const std::string& id,
      const std::string& container_id = "containerd://0123456789abcdef")
  {
    auto restore = RequestFor(id);
    auto* operation = restore.mutable_reference_regular_restore();
    Configure(
        operation->mutable_source(), operation->mutable_io_engine(), source_);
    operation->set_pod_uid("pod-uid");
    operation->set_destination_container("main");
    operation->set_content_uid("content-uid");
    operation->set_source_container("main");
    operation->set_container_id(container_id);
    return broker().HandleRequest(restore);
  }

  Response DirectRestore(const std::string& id, uint32_t namespace_pid)
  {
    auto restore = RequestFor(id);
    Configure(
        restore.mutable_direct_restore()->mutable_source(),
        restore.mutable_direct_restore()->mutable_io_engine(), source_);
    restore.mutable_direct_restore()->add_cuda_namespace_pids(namespace_pid);
    ConfigureRestoreIdentity(
        restore.mutable_direct_restore()->mutable_identity());
    return broker().HandleRequest(restore);
  }

  Response StageCheckpoint(const std::string& id)
  {
    auto checkpoint = RequestFor(id);
    Configure(
        checkpoint.mutable_prepare_staged_checkpoint()->mutable_destination(),
        checkpoint.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
        root_ / "storage" / ("published-" + id));
    return broker().HandleRequest(checkpoint);
  }

  Response BeginRestore(
      const std::string& id,
      CudaStorageBackend backend = v1::CUDA_STORAGE_BACKEND_REGULAR,
      uint32_t target_count = 1)
  {
    auto request = RequestFor(id);
    request.mutable_begin_restore()->set_storage_backend(backend);
    request.mutable_begin_restore()->set_target_count(target_count);
    return broker().HandleRequest(request);
  }

  Response ActivateRestore(
      const std::string& id,
      const std::string& destination_container = "main",
      const std::string& container_id = "containerd://0123456789abcdef")
  {
    auto request = RequestFor(id);
    auto* identity = request.mutable_activate_restore()->mutable_identity();
    ConfigureRestoreIdentity(identity, container_id);
    identity->set_destination_container(destination_container);
    return broker().HandleRequest(request);
  }

  Response BeginCheckpoint(
      const std::string& id,
      CudaStorageBackend backend = v1::CUDA_STORAGE_BACKEND_REGULAR,
      uint32_t target_count = 1)
  {
    auto request = RequestFor(id);
    request.mutable_begin_checkpoint()->set_storage_backend(backend);
    request.mutable_begin_checkpoint()->set_target_count(target_count);
    return broker().HandleRequest(request);
  }

  void ConfigureCudaRestore(Request* request)
  {
    request->mutable_cuda_restore()->set_storage_backend(v1::CUDA_STORAGE_BACKEND_REGULAR);
    request->mutable_cuda_restore()->add_targets();
  }

  fs::path root_;
  fs::path source_;
  std::optional<Broker> broker_;
  FakeCudaEngine* cuda_engine_ = nullptr;
  unsigned request_number_ = 0;
};

TEST_F(BrokerTest, RejectsCudaWhenEngineIsDisabled)
{
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, ShutdownWithoutCudaIsSuccessful)
{
  std::string error = "stale error";
  EXPECT_TRUE(broker().ShutdownCuda(&error));
  EXPECT_TRUE(error.empty());
}

TEST_F(BrokerTest, ShutsDownCudaEngine)
{
  auto& cuda = EnableCuda();
  std::string error;
  EXPECT_TRUE(broker().ShutdownCuda(&error));
  EXPECT_EQ(cuda.shutdown_calls.load(), 1);
  EXPECT_TRUE(error.empty());
}

TEST_F(BrokerTest, BeginsCudaShutdownBeforeFinalCleanup)
{
  auto& cuda = EnableCuda();
  std::string error = "stale";
  EXPECT_TRUE(broker().BeginShutdownCuda(&error));
  EXPECT_EQ(cuda.begin_shutdown_calls.load(), 1);
  EXPECT_TRUE(error.empty());
  EXPECT_EQ(cuda.shutdown_calls.load(), 0);
}

TEST_F(BrokerTest, ReapsExitedCudaTargets)
{
  auto& cuda = EnableCuda();
  std::string error = "stale";
  EXPECT_TRUE(broker().ReapExitedCuda(&error));
  EXPECT_EQ(cuda.reap_exited_calls.load(), 1);
  EXPECT_TRUE(error.empty());
}

TEST_F(BrokerTest, PropagatesCudaReapFailure)
{
  auto& cuda = EnableCuda();
  cuda.reap_exited_succeeds = false;
  std::string error;
  EXPECT_FALSE(broker().ReapExitedCuda(&error));
  EXPECT_EQ(cuda.reap_exited_calls.load(), 1);
  EXPECT_EQ(error, "reap exited failed");
  EXPECT_FALSE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, BackendLocalCudaFailureDoesNotCloseDaemonAdmission)
{
  auto& cuda = EnableCuda();
  cuda.reap_exited_succeeds = false;
  std::string error;
  EXPECT_FALSE(broker().ReapExitedCuda(&error));
  EXPECT_FALSE(broker().ShutdownRequested());

  ASSERT_TRUE(StageCheckpoint("checkpoint-after-backend-failure")
                  .has_staged_checkpoint_directory());
  EXPECT_TRUE(BeginCheckpoint("checkpoint-after-backend-failure")
                  .has_checkpoint_admission_granted());
}

TEST_F(BrokerTest, WorkerOwnershipLossRequestsFailClosedShutdown)
{
  auto& cuda = EnableCuda();
  cuda.shutdown_required = true;
  std::string error;
  EXPECT_TRUE(broker().ReapExitedCuda(&error));
  EXPECT_TRUE(broker().ShutdownRequested());
  EXPECT_TRUE(cuda.fail_stop_requested.load());
}

TEST_F(BrokerTest, PropagatesCudaBeginShutdownFailure)
{
  auto& cuda = EnableCuda();
  cuda.begin_shutdown_succeeds = false;
  std::string error;
  EXPECT_FALSE(broker().BeginShutdownCuda(&error));
  EXPECT_EQ(cuda.begin_shutdown_calls.load(), 1);
  EXPECT_EQ(error, "begin shutdown failed");
}

TEST_F(BrokerTest, PropagatesCudaShutdownFailure)
{
  auto& cuda = EnableCuda();
  cuda.shutdown_succeeds = false;
  std::string error;
  EXPECT_FALSE(broker().ShutdownCuda(&error));
  EXPECT_EQ(cuda.shutdown_calls.load(), 1);
  EXPECT_EQ(error, "shutdown failed");
}

TEST_F(BrokerTest, CompletesCudaRestoreThenCommitsTransaction)
{
  auto& cuda = EnableCuda();
  cuda.restore_result.target_count = 4;
  const auto staged = StageRestore("restore");
  ASSERT_TRUE(staged.has_staged_restore_directory()) << staged.DebugString();
  ASSERT_TRUE(BeginRestore("restore").has_restore_admission_granted());

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_cuda_operation_complete());
  EXPECT_EQ(response.cuda_operation_complete().target_count(), 4);
  EXPECT_EQ(cuda.restore_calls.load(), 1);
  EXPECT_EQ(cuda.last_staging_directory, staged.staged_restore_directory().image_directory());

  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::hours(24 * 365));

  const auto duplicate = broker().HandleRequest(request);
  ASSERT_TRUE(duplicate.has_failure());
  EXPECT_EQ(duplicate.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 1);

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
}

TEST_F(BrokerTest,
       RegularRestoreReferenceSurvivesSourceDeleteAndCommitCleansOnlyReference)
{
  const auto referenced = ReferenceRegularRestore("referenced");
  ASSERT_TRUE(referenced.has_staged_restore_directory())
      << referenced.DebugString();
  const fs::path reference =
      referenced.staged_restore_directory().image_directory();
  struct stat source_stat{};
  struct stat reference_stat{};
  ASSERT_EQ(stat((source_ / "image").c_str(), &source_stat), 0);
  ASSERT_EQ(stat((reference / "image").c_str(), &reference_stat), 0);
  EXPECT_EQ(source_stat.st_dev, reference_stat.st_dev);
  EXPECT_EQ(source_stat.st_ino, reference_stat.st_ino);

  fs::remove_all(source_);
  EXPECT_TRUE(fs::exists(reference / "image"));
  auto commit = RequestFor("referenced");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_FALSE(fs::exists(reference));
  EXPECT_FALSE(fs::exists(source_));
}

TEST_F(BrokerTest, RegularRestoreReferenceRejectsInvalidDestinationIdentity)
{
  const auto request_for = [&](const std::string& id) {
    auto request = RequestFor(id);
    auto* operation = request.mutable_reference_regular_restore();
    Configure(operation->mutable_source(), operation->mutable_io_engine(),
              source_);
    operation->set_pod_uid("pod-uid");
    operation->set_destination_container("main");
    operation->set_content_uid("content-uid");
    operation->set_source_container("main");
    operation->set_container_id("containerd://0123456789abcdef");
    return request;
  };

  auto missing = request_for("missing-identity");
  missing.mutable_reference_regular_restore()->clear_pod_uid();
  EXPECT_EQ(broker().HandleRequest(missing).failure().code(),
            Failure::INVALID_REQUEST);

  auto missing_container = request_for("missing-container-id");
  missing_container.mutable_reference_regular_restore()->clear_container_id();
  EXPECT_EQ(broker().HandleRequest(missing_container).failure().code(),
            Failure::INVALID_REQUEST);

  auto empty = request_for("empty-identity");
  empty.mutable_reference_regular_restore()->set_destination_container("");
  EXPECT_EQ(broker().HandleRequest(empty).failure().code(),
            Failure::INVALID_REQUEST);

  auto too_long = request_for("long-identity");
  too_long.mutable_reference_regular_restore()->set_content_uid(
      std::string(254, 'x'));
  EXPECT_EQ(broker().HandleRequest(too_long).failure().code(),
            Failure::INVALID_REQUEST);

  auto control = request_for("control-identity");
  control.mutable_reference_regular_restore()->set_source_container(
      "main\nworker");
  EXPECT_EQ(broker().HandleRequest(control).failure().code(),
            Failure::INVALID_REQUEST);

  auto invalid_utf8 = request_for("utf8-identity");
  invalid_utf8.mutable_reference_regular_restore()->set_pod_uid("\xff");
  EXPECT_EQ(broker().HandleRequest(invalid_utf8).failure().code(),
            Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, RegularRestoreReferenceAbortAndExpiryCleanExactTree)
{
  const auto aborted = ReferenceRegularRestore("aborted-reference");
  ASSERT_TRUE(aborted.has_staged_restore_directory());
  const fs::path aborted_path =
      aborted.staged_restore_directory().image_directory();
  auto abort = RequestFor("aborted-reference");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(aborted_path));
  EXPECT_TRUE(fs::exists(source_ / "image"));

  const auto expired = ReferenceRegularRestore("expired-reference");
  ASSERT_TRUE(expired.has_staged_restore_directory());
  const fs::path expired_path =
      expired.staged_restore_directory().image_directory();
  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::hours(24));
  EXPECT_FALSE(fs::exists(expired_path));
  EXPECT_TRUE(fs::exists(source_ / "image"));
}

TEST_F(BrokerTest,
       RegularRestoreReferenceRejectsCustomStorageBeforeAdmission)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(ReferenceRegularRestore("regular-reference")
                  .has_staged_restore_directory());
  const auto response = BeginRestore(
      "regular-reference",
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.begin_restore_calls.load(), 0);
}

TEST_F(BrokerTest, DirectRestoreRetainsCarrierDescriptorUntilCommit)
{
  const fs::path process =
      source_ / "cuda-custom-storage" / "process-nspid-42";
  WriteCustomStorage(process, 4096);
  auto& cuda = EnableCuda();

  const auto staged = DirectRestore("direct", 42);
  ASSERT_TRUE(staged.has_staged_restore_directory()) << staged.DebugString();
  EXPECT_STREQ(ResponseResultName(Request::kDirectRestore, staged),
               "direct_restore_staged");
  const fs::path staging =
      staged.staged_restore_directory().image_directory();
  EXPECT_TRUE(fs::exists(staging / "image"));
  EXPECT_TRUE(fs::exists(staging / "cuda-custom-storage" /
                         "process-nspid-42" / "manifest.txt"));
  EXPECT_FALSE(fs::exists(staging / "cuda-custom-storage" /
                          "process-nspid-42" / "device-0000.bin"));
  ASSERT_TRUE(BeginRestore(
                  "direct",
                  v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE)
                  .has_restore_admission_granted());

  auto restore = RequestFor("direct");
  restore.mutable_cuda_restore()->set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  restore.mutable_cuda_restore()->add_targets()->set_namespace_pid(42);
  ASSERT_TRUE(broker().HandleRequest(restore).has_cuda_operation_complete());
  ASSERT_EQ(cuda.last_direct_carrier_fds.size(), 1u);
  const int carrier_fd = cuda.last_direct_carrier_fds.front();
  EXPECT_NE(fcntl(carrier_fd, F_GETFD), -1);

  auto commit = RequestFor("direct");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  errno = 0;
  EXPECT_EQ(fcntl(carrier_fd, F_GETFD), -1);
  EXPECT_EQ(errno, EBADF);
}

TEST_F(BrokerTest, ActivationPublishesBackendAndAbortClosesGate)
{
  const fs::path process =
      source_ / "cuda-custom-storage" / "process-nspid-42";
  WriteCustomStorage(process, 7);
  EnableCuda();
  ASSERT_TRUE(DirectRestore("activated", 42).has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore(
                  "activated",
                  v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE)
                  .has_restore_admission_granted());
  for (int field = 0; field < 5; ++field) {
    auto mismatch = RequestFor("activated");
    auto* identity = mismatch.mutable_activate_restore()->mutable_identity();
    ConfigureRestoreIdentity(identity);
    switch (field) {
      case 0: identity->set_pod_uid("other-pod"); break;
      case 1: identity->set_destination_container("other-container"); break;
      case 2: identity->set_content_uid("other-content"); break;
      case 3: identity->set_source_container("other-source"); break;
      case 4: identity->set_container_id("containerd://replacement"); break;
    }
    const auto rejected = broker().HandleRequest(mismatch);
    ASSERT_TRUE(rejected.has_failure()) << field << ": " << rejected.DebugString();
    EXPECT_EQ(rejected.failure().code(), Failure::TRANSACTION_CONFLICT)
        << field;
  }
  const auto activated = ActivateRestore("activated");
  ASSERT_TRUE(activated.has_restore_activation_granted())
      << activated.DebugString();
  EXPECT_TRUE(ActivateRestore("activated").has_restore_activation_granted());
  EXPECT_STREQ(ResponseResultName(Request::kActivateRestore, activated),
               "restore_activated");

  const auto marker_name =
      StageReadyGate::OperationId("pod-uid", "main") + ".json";
  fs::path marker;
  const auto owners = root_ / "storage" / ".pagebroker-stage-ready" /
                      "v1" / "owners";
  for (const auto& owner : fs::directory_iterator(owners)) {
    const auto candidate = owner.path() / "markers" / marker_name;
    if (fs::exists(candidate))
      marker = candidate;
  }
  ASSERT_FALSE(marker.empty());
  const auto read_marker = [&] {
    std::ifstream stream(marker);
    return std::string(std::istreambuf_iterator<char>(stream),
                       std::istreambuf_iterator<char>());
  };
  EXPECT_NE(read_marker().find("\"backend\":\"posix-custom-storage\""),
            std::string::npos);
  EXPECT_NE(read_marker().find("\"state\":\"ready\""),
            std::string::npos);

  auto abort = RequestFor("activated");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_NE(read_marker().find("\"state\":\"aborted\""),
            std::string::npos);
}

TEST_F(BrokerTest, DirectRestoreRejectsMissingIdentityBeforeStaging)
{
  auto request = RequestFor("missing-direct-identity");
  Configure(request.mutable_direct_restore()->mutable_source(),
            request.mutable_direct_restore()->mutable_io_engine(), source_);
  request.mutable_direct_restore()->add_cuda_namespace_pids(42);

  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_FALSE(fs::exists(root_ / "tmpfs" / "restore" /
                          "missing-direct-identity"));
}

TEST_F(BrokerTest, CommitClosesActivatedStageReadyGate)
{
  EnableCuda();
  ASSERT_TRUE(ReferenceRegularRestore("activated-commit")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("activated-commit").has_restore_admission_granted());
  ASSERT_TRUE(ActivateRestore("activated-commit").has_restore_activation_granted());

  const auto marker_name =
      StageReadyGate::OperationId("pod-uid", "main") + ".json";
  fs::path marker;
  const auto owners = root_ / "storage" / ".pagebroker-stage-ready" /
                      "v1" / "owners";
  for (const auto& owner : fs::directory_iterator(owners)) {
    const auto candidate = owner.path() / "markers" / marker_name;
    if (fs::exists(candidate))
      marker = candidate;
  }
  ASSERT_FALSE(marker.empty());

  auto restore = RequestFor("activated-commit");
  ConfigureCudaRestore(&restore);
  ASSERT_TRUE(broker().HandleRequest(restore).has_cuda_operation_complete());

  auto commit = RequestFor("activated-commit");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  std::ifstream stream(marker);
  const std::string contents{std::istreambuf_iterator<char>(stream),
                             std::istreambuf_iterator<char>()};
  EXPECT_NE(contents.find("\"state\":\"committed\""),
            std::string::npos);
}

TEST_F(BrokerTest, StageMarkerMaintenanceFailureIsNonfatalAndRetried)
{
  bool fail_unlink = true;
  EnableCuda(
      std::nullopt,
      [&](const std::string& operation) {
        if (operation == "reap_marker_unlink" && fail_unlink) {
          fail_unlink = false;
          return EIO;
        }
        return 0;
      });
  ASSERT_TRUE(ReferenceRegularRestore("maintenance")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("maintenance").has_restore_admission_granted());
  ASSERT_TRUE(
      ActivateRestore("maintenance").has_restore_activation_granted());
  auto restore = RequestFor("maintenance");
  ConfigureCudaRestore(&restore);
  ASSERT_TRUE(broker().HandleRequest(restore).has_cuda_operation_complete());
  auto commit = RequestFor("maintenance");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());

  const auto marker_name =
      StageReadyGate::OperationId("pod-uid", "main") + ".json";
  fs::path marker;
  const auto owners = root_ / "storage" / ".pagebroker-stage-ready" /
                      "v1" / "owners";
  for (const auto& owner : fs::directory_iterator(owners)) {
    const auto candidate = owner.path() / "markers" / marker_name;
    if (fs::exists(candidate))
      marker = candidate;
  }
  ASSERT_FALSE(marker.empty());
  const auto old = std::chrono::system_clock::now() - std::chrono::hours(2);
  const auto old_time = std::chrono::system_clock::to_time_t(old);
  const timespec times[2] = {{old_time, 0}, {old_time, 0}};
  ASSERT_EQ(utimensat(AT_FDCWD, marker.c_str(), times, 0), 0);

  std::string error;
  EXPECT_FALSE(broker().ReapRetainedStageReadyMarkers(
      std::chrono::system_clock::now(), &error));
  EXPECT_NE(error.find("reap_marker_unlink"), std::string::npos);
  EXPECT_TRUE(fs::exists(marker));
  // The daemon remains usable after maintenance reports the error, and the
  // next scheduled pass retries the exact retained marker.
  EXPECT_TRUE(StageRestore("after-maintenance-failure")
                  .has_staged_restore_directory());
  EXPECT_TRUE(broker().ReapRetainedStageReadyMarkers(
      std::chrono::system_clock::now(), &error));
  EXPECT_TRUE(error.empty());
  EXPECT_FALSE(fs::exists(marker));
}

TEST_F(BrokerTest, RegularActivationRejectsEveryMismatchedIdentityField)
{
  EnableCuda();
  ASSERT_TRUE(ReferenceRegularRestore("regular-identity")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("regular-identity").has_restore_admission_granted());

  for (int field = 0; field < 5; ++field) {
    auto mismatch = RequestFor("regular-identity");
    auto* identity = mismatch.mutable_activate_restore()->mutable_identity();
    ConfigureRestoreIdentity(identity);
    switch (field) {
      case 0: identity->set_pod_uid("other-pod"); break;
      case 1: identity->set_destination_container("other-container"); break;
      case 2: identity->set_content_uid("other-content"); break;
      case 3: identity->set_source_container("other-source"); break;
      case 4: identity->set_container_id("containerd://replacement"); break;
    }
    const auto rejected = broker().HandleRequest(mismatch);
    ASSERT_TRUE(rejected.has_failure()) << field << ": " << rejected.DebugString();
    EXPECT_EQ(rejected.failure().code(), Failure::TRANSACTION_CONFLICT)
        << field;
  }
  EXPECT_TRUE(ActivateRestore("regular-identity")
                  .has_restore_activation_granted());
}

TEST_F(BrokerTest, RegularActivationIgnoresUnknownProtobufIdentityFields)
{
  EnableCuda();
  ASSERT_TRUE(ReferenceRegularRestore("semantic-identity")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("semantic-identity")
                  .has_restore_admission_granted());

  auto request = RequestFor("semantic-identity");
  auto* identity = request.mutable_activate_restore()->mutable_identity();
  identity->set_pod_uid("pod-uid");
  identity->set_destination_container("main");
  identity->set_content_uid("content-uid");
  identity->set_source_container("main");
  identity->set_container_id("containerd://0123456789abcdef");
  identity->GetReflection()->MutableUnknownFields(identity)->AddVarint(99, 1);

  EXPECT_TRUE(
      broker().HandleRequest(request).has_restore_activation_granted());
}

TEST_F(BrokerTest, CommittedOperationAllowsNewContainerIncarnation)
{
  EnableCuda();
  ASSERT_TRUE(ReferenceRegularRestore("first-incarnation")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("first-incarnation")
                  .has_restore_admission_granted());
  ASSERT_TRUE(ActivateRestore("first-incarnation")
                  .has_restore_activation_granted());
  auto cuda_restore = RequestFor("first-incarnation");
  ConfigureCudaRestore(&cuda_restore);
  ASSERT_TRUE(broker().HandleRequest(cuda_restore)
                  .has_cuda_operation_complete());
  auto commit = RequestFor("first-incarnation");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());

  constexpr char replacement[] = "containerd://replacement";
  ASSERT_TRUE(ReferenceRegularRestore("second-incarnation", replacement)
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("second-incarnation")
                  .has_restore_admission_granted());
  ASSERT_TRUE(ActivateRestore("second-incarnation", "main", replacement)
                  .has_restore_activation_granted());
}

TEST_F(BrokerTest, AbortTerminalizesVisibleReadyAfterUncertainActivation)
{
  bool fail_publication_sync = true;
  EnableCuda(
      std::nullopt,
      [&](const std::string& operation) {
        if (operation == "publish_marker_directory_fsync" &&
            fail_publication_sync) {
          fail_publication_sync = false;
          return EIO;
        }
        return 0;
      });
  ASSERT_TRUE(ReferenceRegularRestore("uncertain-activation")
                  .has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("uncertain-activation")
                  .has_restore_admission_granted());
  const auto activated = ActivateRestore("uncertain-activation");
  ASSERT_TRUE(activated.has_failure());
  EXPECT_EQ(activated.failure().code(), Failure::STORAGE_ERROR);

  const auto marker_name =
      StageReadyGate::OperationId("pod-uid", "main") + ".json";
  fs::path marker;
  const auto owners = root_ / "storage" / ".pagebroker-stage-ready" /
                      "v1" / "owners";
  for (const auto& owner : fs::directory_iterator(owners)) {
    const auto candidate = owner.path() / "markers" / marker_name;
    if (fs::exists(candidate))
      marker = candidate;
  }
  ASSERT_FALSE(marker.empty());
  {
    std::ifstream stream(marker);
    const std::string contents{std::istreambuf_iterator<char>(stream),
                               std::istreambuf_iterator<char>()};
    EXPECT_NE(contents.find("\"state\":\"ready\""), std::string::npos);
  }

  auto abort = RequestFor("uncertain-activation");
  abort.mutable_abort();
  ASSERT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  std::ifstream stream(marker);
  const std::string contents{std::istreambuf_iterator<char>(stream),
                             std::istreambuf_iterator<char>()};
  EXPECT_NE(contents.find("\"state\":\"aborted\""), std::string::npos);
}

TEST_F(BrokerTest, DirectRestoreRejectsCUDAIdentityOutsideCarrierSet)
{
  const fs::path process =
      source_ / "cuda-custom-storage" / "process-nspid-42";
  WriteCustomStorage(process, 7);
  auto& cuda = EnableCuda();
  const auto staged = DirectRestore("direct", 42);
  ASSERT_TRUE(staged.has_staged_restore_directory()) << staged.DebugString();
  ASSERT_TRUE(BeginRestore(
                  "direct",
                  v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE)
                  .has_restore_admission_granted());
  auto restore = RequestFor("direct");
  restore.mutable_cuda_restore()->set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  restore.mutable_cuda_restore()->add_targets()->set_namespace_pid(43);
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 0);
  restore.mutable_cuda_restore()->mutable_targets(0)->set_namespace_pid(42);
  EXPECT_TRUE(broker().HandleRequest(restore).has_cuda_operation_complete());
  EXPECT_EQ(cuda.restore_calls.load(), 1);
}

TEST_F(BrokerTest,
       DirectRestoreDescriptorCapacityDefersWithoutDisruptingOtherRequests)
{
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-42", 7);
  EnableCuda(4);
  ASSERT_TRUE(DirectRestore("first-direct", 42)
                  .has_staged_restore_directory());

  const auto saturated = DirectRestore("second-direct", 42);
  ASSERT_TRUE(saturated.has_failure());
  EXPECT_EQ(saturated.failure().code(), Failure::BUSY);
  ASSERT_TRUE(saturated.failure().has_target_may_be_mutated());
  EXPECT_FALSE(saturated.failure().target_may_be_mutated());

  const auto ordinary = StageRestore("ordinary");
  EXPECT_TRUE(ordinary.has_staged_restore_directory())
      << ordinary.DebugString();

  auto abort = RequestFor("first-direct");
  abort.mutable_abort();
  ASSERT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  ASSERT_TRUE(DirectRestore("second-direct", 42)
                  .has_staged_restore_directory());

  auto commit = RequestFor("second-direct");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(DirectRestore("third-direct", 42)
                  .has_staged_restore_directory());
}

TEST_F(BrokerTest, ExpiredDirectRestoreReleasesDescriptorCapacity)
{
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-42", 7);
  EnableCuda(4);
  ASSERT_TRUE(DirectRestore("expired-direct", 42)
                  .has_staged_restore_directory());
  ASSERT_EQ(DirectRestore("after-expiry", 42).failure().code(),
            Failure::BUSY);

  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::minutes(10));

  EXPECT_TRUE(DirectRestore("after-expiry", 42)
                  .has_staged_restore_directory());
}

TEST_F(BrokerTest, DirectRestoreRequiresExactPIDSetBeforeConsumingAdmission)
{
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-42", 7);
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-43", 9);
  auto& cuda = EnableCuda();
  auto stage = RequestFor("direct-set");
  Configure(stage.mutable_direct_restore()->mutable_source(),
            stage.mutable_direct_restore()->mutable_io_engine(), source_);
  stage.mutable_direct_restore()->add_cuda_namespace_pids(42);
  stage.mutable_direct_restore()->add_cuda_namespace_pids(43);
  ConfigureRestoreIdentity(stage.mutable_direct_restore()->mutable_identity());
  ASSERT_TRUE(broker().HandleRequest(stage).has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore(
                  "direct-set",
                  v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE, 2)
                  .has_restore_admission_granted());
  EXPECT_EQ(cuda.last_begin_restore_dispatch_group_count.load(), 2u);

  auto subset = RequestFor("direct-set");
  subset.mutable_cuda_restore()->set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  subset.mutable_cuda_restore()->add_targets()->set_namespace_pid(42);
  EXPECT_EQ(broker().HandleRequest(subset).failure().code(),
            Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 0);

  auto exact = RequestFor("direct-set");
  exact.mutable_cuda_restore()->set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE);
  exact.mutable_cuda_restore()->add_targets()->set_namespace_pid(43);
  exact.mutable_cuda_restore()->add_targets()->set_namespace_pid(42);
  EXPECT_TRUE(broker().HandleRequest(exact).has_cuda_operation_complete());
  EXPECT_EQ(cuda.restore_calls.load(), 1);
}

TEST_F(BrokerTest,
       CustomStorageAdmissionCountsImmutableLaunchJobAsOneDispatchGroup)
{
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-42", 7);
  WriteCustomStorage(
      source_ / "cuda-custom-storage" / "process-nspid-43", 9);
  std::ofstream(source_ / "cuda-checkpoint-job") << "shared-launch-job";
  auto& cuda = EnableCuda();
  auto stage = RequestFor("direct-launch-job");
  Configure(stage.mutable_direct_restore()->mutable_source(),
            stage.mutable_direct_restore()->mutable_io_engine(), source_);
  stage.mutable_direct_restore()->add_cuda_namespace_pids(42);
  stage.mutable_direct_restore()->add_cuda_namespace_pids(43);
  ConfigureRestoreIdentity(stage.mutable_direct_restore()->mutable_identity());
  ASSERT_TRUE(broker().HandleRequest(stage).has_staged_restore_directory());

  // Admission consumes the PageBroker-owned staged copy, not mutable source
  // storage or restore-time process state.
  fs::remove(source_ / "cuda-checkpoint-job");
  ASSERT_TRUE(BeginRestore(
                  "direct-launch-job",
                  v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE, 2)
                  .has_restore_admission_granted());
  EXPECT_EQ(cuda.last_begin_restore_target_count.load(), 2u);
  EXPECT_EQ(cuda.last_begin_restore_dispatch_group_count.load(), 1u);
}

TEST_F(BrokerTest, RepeatingMatchingRestoreAdmissionIsIdempotent)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  EXPECT_TRUE(BeginRestore("restore").has_restore_admission_granted());
  EXPECT_TRUE(BeginRestore("restore").has_restore_admission_granted());
  EXPECT_EQ(cuda.begin_restore_calls.load(), 1);
  EXPECT_EQ(cuda.last_begin_restore_target_count.load(), 1);
}

TEST_F(BrokerTest, RestoreAdmissionTargetCountIsImmutable)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  EXPECT_TRUE(BeginRestore("restore", v1::CUDA_STORAGE_BACKEND_REGULAR, 2)
                  .has_restore_admission_granted());
  const auto conflict =
      BeginRestore("restore", v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  ASSERT_TRUE(conflict.has_failure());
  EXPECT_EQ(conflict.failure().code(), Failure::TRANSACTION_CONFLICT);

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto restore = broker().HandleRequest(request);
  ASSERT_TRUE(restore.has_failure());
  EXPECT_EQ(restore.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 0);
}

TEST_F(BrokerTest, PropagatesCudaMutationClassificationAndAllowsAbort)
{
  auto& cuda = EnableCuda();
  cuda.restore_result = {
      .succeeded = false,
      .target_may_be_mutated = true,
      .target_count = 2,
      .error = "restore failed",
  };
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("restore").has_restore_admission_granted());

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::CUDA_ERROR);
  EXPECT_TRUE(response.failure().target_may_be_mutated());
  EXPECT_EQ(response.failure().message(), "restore failed");

  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::hours(24 * 365));

  const auto replay = broker().HandleRequest(request);
  ASSERT_TRUE(replay.has_failure());
  EXPECT_EQ(replay.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 1);

  auto abort = RequestFor("restore");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
}

TEST_F(BrokerTest, PropagatesCudaRestoreAdmissionBusyWithoutMutation)
{
  auto& cuda = EnableCuda();
  cuda.begin_restore_result = {
      .succeeded = false,
      .target_may_be_mutated = false,
      .failure_code = Failure::BUSY,
      .target_count = 1,
      .error = "PageBroker CUDA restore capacity is busy",
  };
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());

  const auto response = BeginRestore("restore");
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::BUSY);
  EXPECT_FALSE(response.failure().target_may_be_mutated());
  EXPECT_FALSE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, FailsClosedWhenCudaEngineThrowsUnexpectedly)
{
  auto& cuda = EnableCuda();
  cuda.restore_fn = [](const fs::path&) -> CudaOperationResult {
    throw std::runtime_error("driver adapter exception");
  };
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("restore").has_restore_admission_granted());

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::CUDA_ERROR);
  EXPECT_TRUE(response.failure().target_may_be_mutated());
  EXPECT_NE(response.failure().message().find("driver adapter exception"), std::string::npos);
  EXPECT_TRUE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, FailsClosedWhenCudaValidationExceptionEscapesOperation)
{
  auto& cuda = EnableCuda();
  cuda.restore_fn = [](const fs::path&) -> CudaOperationResult {
    throw std::invalid_argument("late validation exception");
  };
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("restore").has_restore_admission_granted());

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::CUDA_ERROR);
  EXPECT_TRUE(response.failure().has_target_may_be_mutated());
  EXPECT_TRUE(response.failure().target_may_be_mutated());
  EXPECT_NE(response.failure().message().find("late validation exception"), std::string::npos);
  EXPECT_TRUE(cuda.fail_stop_requested.load());
  EXPECT_TRUE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, FailsClosedWhenCudaCheckpointExceptionEscapesOperation)
{
  auto& cuda = EnableCuda();
  cuda.checkpoint_fn = [](const fs::path&) -> CudaOperationResult {
    throw std::invalid_argument("late checkpoint validation exception");
  };
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      root_ / "storage" / "published");
  ASSERT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());
  ASSERT_TRUE(BeginCheckpoint("checkpoint").has_checkpoint_admission_granted());

  auto request = RequestFor("checkpoint");
  request.mutable_cuda_checkpoint()->set_storage_backend(
      v1::CUDA_STORAGE_BACKEND_REGULAR);
  request.mutable_cuda_checkpoint()->add_targets();
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::CUDA_ERROR);
  EXPECT_TRUE(response.failure().has_target_may_be_mutated());
  EXPECT_TRUE(response.failure().target_may_be_mutated());
  EXPECT_NE(
      response.failure().message().find("late checkpoint validation exception"),
      std::string::npos);
  EXPECT_TRUE(cuda.fail_stop_requested.load());
  EXPECT_TRUE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, CheckpointAdmissionIsExactAndIdempotent)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageCheckpoint("checkpoint").has_staged_checkpoint_directory());
  EXPECT_TRUE(BeginCheckpoint(
      "checkpoint", v1::CUDA_STORAGE_BACKEND_REGULAR, 2)
                  .has_checkpoint_admission_granted());
  EXPECT_TRUE(BeginCheckpoint(
      "checkpoint", v1::CUDA_STORAGE_BACKEND_REGULAR, 2)
                  .has_checkpoint_admission_granted());
  EXPECT_EQ(cuda.begin_checkpoint_calls.load(), 1u);
  EXPECT_EQ(cuda.last_begin_checkpoint_target_count.load(), 2u);

  const auto mismatch = BeginCheckpoint(
      "checkpoint", v1::CUDA_STORAGE_BACKEND_REGULAR, 1);
  ASSERT_TRUE(mismatch.has_failure());
  EXPECT_EQ(mismatch.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, CheckpointAdmissionBusyIsProvenNonMutating)
{
  auto& cuda = EnableCuda();
  cuda.begin_checkpoint_result = {
      .succeeded = false,
      .target_may_be_mutated = false,
      .failure_code = Failure::BUSY,
      .error = "checkpoint boundary busy",
  };
  ASSERT_TRUE(StageCheckpoint("checkpoint").has_staged_checkpoint_directory());
  const auto response = BeginCheckpoint("checkpoint");
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::BUSY);
  ASSERT_TRUE(response.failure().has_target_may_be_mutated());
  EXPECT_FALSE(response.failure().target_may_be_mutated());
}

TEST_F(BrokerTest, RequestsShutdownAfterFatalCudaFailure)
{
  auto& cuda = EnableCuda();
  cuda.restore_result = {
      .succeeded = false,
      .target_may_be_mutated = true,
      .fatal = true,
      .target_count = 1,
      .error = "CUDA handle state is indeterminate",
  };
  ASSERT_TRUE(StageRestore("restore").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("restore").has_restore_admission_granted());

  auto request = RequestFor("restore");
  ConfigureCudaRestore(&request);
  const auto response = broker().HandleRequest(request);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::CUDA_ERROR);
  EXPECT_TRUE(response.failure().target_may_be_mutated());
  EXPECT_TRUE(broker().ShutdownRequested());
}

TEST_F(BrokerTest, FatalResultImmediatelyClosesPreviouslyAdmittedCudaDispatch)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("first").has_staged_restore_directory());
  ASSERT_TRUE(StageRestore("second").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("first").has_restore_admission_granted());
  ASSERT_TRUE(BeginRestore("second").has_restore_admission_granted());
  cuda.restore_result = {.succeeded = false,
                         .target_may_be_mutated = true,
                         .fatal = true,
                         .error = "unknown worker outcome"};

  auto first = RequestFor("first");
  ConfigureCudaRestore(&first);
  EXPECT_TRUE(broker().HandleRequest(first).has_failure());
  EXPECT_TRUE(cuda.fail_stop_requested.load());

  auto second = RequestFor("second");
  ConfigureCudaRestore(&second);
  const auto denied = broker().HandleRequest(second);
  ASSERT_TRUE(denied.has_failure());
  EXPECT_TRUE(denied.failure().target_may_be_mutated() == false);
  EXPECT_EQ(denied.failure().message(), "CUDA engine is fail-stopped");
  EXPECT_EQ(cuda.restore_calls.load(), 1U);
}

TEST_F(BrokerTest, RejectsCudaOperationForWrongTransactionKind)
{
  auto& cuda = EnableCuda();
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "published");
  ASSERT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());

  auto restore = RequestFor("checkpoint");
  restore.mutable_begin_restore()->set_storage_backend(v1::CUDA_STORAGE_BACKEND_REGULAR);
  restore.mutable_begin_restore()->set_target_count(1);
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_EQ(cuda.restore_calls.load(), 0);
}

TEST_F(BrokerTest, RunsIndependentCudaRestoreTransactionsConcurrently)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("first").has_staged_restore_directory());
  ASSERT_TRUE(StageRestore("second").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("first").has_restore_admission_granted());
  ASSERT_TRUE(BeginRestore("second").has_restore_admission_granted());

  std::mutex mutex;
  std::condition_variable ready;
  std::condition_variable release;
  size_t active = 0;
  bool released = false;
  cuda.restore_fn = [&](const fs::path&) {
    std::unique_lock lock(mutex);
    ++active;
    ready.notify_all();
    release.wait(lock, [&] { return released; });
    return CudaOperationResult{.succeeded = true, .target_count = 1};
  };

  auto first = RequestFor("first");
  auto second = RequestFor("second");
  ConfigureCudaRestore(&first);
  ConfigureCudaRestore(&second);
  Response first_response;
  Response second_response;
  std::thread first_request([&] { first_response = broker().HandleRequest(first); });
  std::thread second_request([&] { second_response = broker().HandleRequest(second); });
  bool overlapped = false;
  {
    std::unique_lock lock(mutex);
    overlapped = ready.wait_for(lock, std::chrono::seconds(1), [&] { return active == 2; });
    released = true;
  }
  release.notify_all();
  first_request.join();
  second_request.join();
  EXPECT_TRUE(overlapped);
  EXPECT_TRUE(first_response.has_cuda_operation_complete());
  EXPECT_TRUE(second_response.has_cuda_operation_complete());
}

TEST_F(BrokerTest, BusyCudaTransactionDoesNotDelayMaintenanceOrShutdownStart)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("busy").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("busy").has_restore_admission_granted());

  std::mutex mutex;
  std::condition_variable entered;
  std::condition_variable release;
  bool restore_entered = false;
  bool released = false;
  cuda.restore_fn = [&](const fs::path&) {
    std::unique_lock lock(mutex);
    restore_entered = true;
    entered.notify_all();
    release.wait(lock, [&] { return released; });
    return CudaOperationResult{.succeeded = true, .target_count = 1};
  };

  auto request = RequestFor("busy");
  ConfigureCudaRestore(&request);
  Response restore_response;
  std::thread restore([&] {
    restore_response = broker().HandleRequest(request);
  });
  bool started = false;
  {
    std::unique_lock lock(mutex);
    started = entered.wait_for(
        lock, std::chrono::seconds(1), [&] { return restore_entered; });
  }
  if (!started) {
    {
      std::lock_guard lock(mutex);
      released = true;
    }
    release.notify_all();
    restore.join();
  }
  ASSERT_TRUE(started);

  const auto now = DaemonMaintenanceSchedule::Clock::now();
  DaemonMaintenanceSchedule maintenance(now);
  bool shutdown_result = false;
  std::string shutdown_error;
  auto maintenance_and_shutdown = std::async(std::launch::async, [&] {
    if (maintenance.TransactionsDue(now)) {
      broker().ReapExpiredTransactions(
          now + std::chrono::hours(24 * 365));
    }
    shutdown_result = broker().BeginShutdownCuda(&shutdown_error);
  });

  const bool responsive = maintenance_and_shutdown.wait_for(
                              std::chrono::seconds(1)) ==
                          std::future_status::ready;
  const size_t shutdown_calls_while_busy =
      cuda.begin_shutdown_calls.load();
  {
    std::lock_guard lock(mutex);
    released = true;
  }
  release.notify_all();
  restore.join();
  maintenance_and_shutdown.get();

  EXPECT_TRUE(responsive);
  EXPECT_EQ(shutdown_calls_while_busy, 1u);
  EXPECT_TRUE(shutdown_result);
  EXPECT_TRUE(shutdown_error.empty());
  EXPECT_TRUE(restore_response.has_cuda_operation_complete());
}

TEST_F(BrokerTest,
       ShutdownRetriesIdentityBeforeWaitingForBlockedRestoreHandler)
{
  auto& cuda = EnableCuda();
  ASSERT_TRUE(StageRestore("shutdown-retry").has_staged_restore_directory());
  ASSERT_TRUE(BeginRestore("shutdown-retry").has_restore_admission_granted());

  std::mutex mutex;
  std::condition_variable entered;
  std::condition_variable release;
  std::condition_variable shutdown_entered;
  bool restore_entered = false;
  bool released = false;
  bool shutdown_attempted = false;
  bool allow_shutdown = false;
  cuda.restore_fn = [&](const fs::path&) {
    std::unique_lock lock(mutex);
    restore_entered = true;
    entered.notify_all();
    release.wait(lock, [&] { return released; });
    return CudaOperationResult{.succeeded = true, .target_count = 1};
  };

  auto request = RequestFor("shutdown-retry");
  ConfigureCudaRestore(&request);
  Response restore_response;
  std::vector<std::future<void>> handlers;
  handlers.emplace_back(std::async(std::launch::async, [&] {
    restore_response = broker().HandleRequest(request);
  }));
  bool started = false;
  {
    std::unique_lock lock(mutex);
    started = entered.wait_for(
        lock, std::chrono::seconds(1), [&] { return restore_entered; });
  }
  if (!started) {
    {
      std::lock_guard lock(mutex);
      released = true;
    }
    release.notify_all();
    handlers.front().get();
  }
  ASSERT_TRUE(started);

  cuda.begin_shutdown_fn = [&](size_t, std::string* error) {
    {
      std::lock_guard lock(mutex);
      shutdown_attempted = true;
      shutdown_entered.notify_all();
    }
    {
      std::lock_guard lock(mutex);
      if (!allow_shutdown) {
        *error = "restored target identity is temporarily unreadable";
        return false;
      }
    }
    {
      std::lock_guard lock(mutex);
      released = true;
    }
    release.notify_all();
    error->clear();
    return true;
  };

  const fs::path readiness_socket = root_ / "shutdown.sock";
  std::error_code readiness_error;
  ASSERT_TRUE(PublishDaemonReadiness(readiness_socket, &readiness_error))
      << readiness_error.message();

  auto shutdown = std::async(std::launch::async, [&] {
    ShutdownAndWaitForHandlers(broker(), handlers, readiness_socket);
  });
  // Readiness is withdrawn before BeginShutdown retries identity and before
  // the blocked restore handler is joined.
  bool observed_shutdown = false;
  {
    std::unique_lock lock(mutex);
    observed_shutdown = shutdown_entered.wait_for(
        lock, std::chrono::seconds(1), [&] { return shutdown_attempted; });
  }
  EXPECT_TRUE(observed_shutdown);
  EXPECT_FALSE(fs::exists(DaemonReadinessPath(readiness_socket)));
  EXPECT_EQ(shutdown.wait_for(std::chrono::seconds(0)),
            std::future_status::timeout);
  {
    std::lock_guard lock(mutex);
    allow_shutdown = true;
  }
  const auto shutdown_status = shutdown.wait_for(std::chrono::seconds(2));
  if (shutdown_status != std::future_status::ready) {
    {
      std::lock_guard lock(mutex);
      released = true;
    }
    release.notify_all();
  }
  shutdown.get();

  ASSERT_EQ(shutdown_status, std::future_status::ready);
  EXPECT_GE(cuda.begin_shutdown_calls.load(), 2u);
  EXPECT_TRUE(restore_response.has_cuda_operation_complete());
}

TEST_F(BrokerTest, StagesRestoreAndCleansUpOnCommit)
{
  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());
  EXPECT_TRUE(fs::exists(staging_directory / "image"));

  const auto conflict = broker().HandleRequest(restore);
  ASSERT_TRUE(conflict.has_failure());
  EXPECT_EQ(conflict.failure().code(), Failure::TRANSACTION_CONFLICT);

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_FALSE(fs::exists(staging_directory));

  auto abort = RequestFor("restore");
  abort.mutable_abort();
  const auto abort_response = broker().HandleRequest(abort);
  ASSERT_TRUE(abort_response.has_failure());
  EXPECT_EQ(abort_response.failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTest, StagesIndependentRestoresConcurrently)
{
  auto first = RequestFor("first");
  auto second = RequestFor("second");
  Configure(
      first.mutable_staged_restore()->mutable_source(), first.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      second.mutable_staged_restore()->mutable_source(), second.mutable_staged_restore()->mutable_io_engine(), source_);

  Response first_response;
  Response second_response;
  std::thread first_request([&] { first_response = broker().HandleRequest(first); });
  std::thread second_request([&] { second_response = broker().HandleRequest(second); });
  first_request.join();
  second_request.join();

  ASSERT_TRUE(first_response.has_staged_restore_directory());
  ASSERT_TRUE(second_response.has_staged_restore_directory());
  EXPECT_NE(
      first_response.staged_restore_directory().image_directory(),
      second_response.staged_restore_directory().image_directory());
}

TEST_F(BrokerTest, RetainsRestoreCapacityUntilCommit)
{
  broker_.reset();
  const uintmax_t artifact_bytes = fs::file_size(source_ / "image");
  broker_.emplace(root_ / "tmpfs", root_ / "storage", artifact_bytes);

  ASSERT_TRUE(StageRestore("first").has_staged_restore_directory());
  const auto blocked = StageRestore("second");
  ASSERT_TRUE(blocked.has_failure());
  EXPECT_EQ(blocked.failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto commit = RequestFor("first");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(StageRestore("second").has_staged_restore_directory());
}

TEST_F(BrokerTest, RetainsSameIDAcrossConcurrentCapacityFailures)
{
  broker_.reset();
  const uintmax_t artifact_bytes = fs::file_size(source_ / "image");
  broker_.emplace(root_ / "tmpfs", root_ / "storage", artifact_bytes);
  ASSERT_TRUE(StageRestore("held").has_staged_restore_directory());

  auto first_request = RequestFor("waiting");
  auto second_request = RequestFor("waiting");
  Configure(
      first_request.mutable_staged_restore()->mutable_source(),
      first_request.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      second_request.mutable_staged_restore()->mutable_source(),
      second_request.mutable_staged_restore()->mutable_io_engine(), source_);
  Response first_response;
  Response second_response;
  std::thread first([&] { first_response = broker().HandleRequest(first_request); });
  std::thread second([&] { second_response = broker().HandleRequest(second_request); });
  first.join();
  second.join();
  ASSERT_TRUE(first_response.has_failure());
  ASSERT_TRUE(second_response.has_failure());
  EXPECT_EQ(first_response.failure().code(), Failure::INSUFFICIENT_STORAGE);
  EXPECT_EQ(second_response.failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto commit = RequestFor("held");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());

  auto retry_first_request = RequestFor("waiting");
  auto retry_second_request = RequestFor("waiting");
  Configure(
      retry_first_request.mutable_staged_restore()->mutable_source(),
      retry_first_request.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      retry_second_request.mutable_staged_restore()->mutable_source(),
      retry_second_request.mutable_staged_restore()->mutable_io_engine(), source_);
  std::thread retry_first([&] { first_response = broker().HandleRequest(retry_first_request); });
  std::thread retry_second([&] { second_response = broker().HandleRequest(retry_second_request); });
  retry_first.join();
  retry_second.join();
  ASSERT_NE(first_response.has_staged_restore_directory(), second_response.has_staged_restore_directory());
  const auto& rejected = first_response.has_staged_restore_directory() ? second_response : first_response;
  ASSERT_TRUE(rejected.has_failure());
  EXPECT_EQ(rejected.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, ReapsCapacityRejectedNewTransaction)
{
  broker_.reset();
  const uintmax_t artifact_bytes = fs::file_size(source_ / "image");
  broker_.emplace(root_ / "tmpfs", root_ / "storage", artifact_bytes);
  ASSERT_TRUE(StageRestore("held").has_staged_restore_directory());
  ASSERT_EQ(StageRestore("waiting").failure().code(), Failure::INSUFFICIENT_STORAGE);

  broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::hours(3));
  auto commit = RequestFor("held");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);
  EXPECT_TRUE(StageRestore("waiting").has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsRestoreThatGrowsWhileItIsCopied)
{
  broker_.reset();
  broker_.emplace(
      root_ / "tmpfs", root_ / "storage", FakeChangingTransferEngine::admitted_bytes, nullptr,
      std::make_unique<FakeChangingTransferEngine>(FakeChangingTransferEngine::admitted_bytes + 1));

  const fs::path staged_directory = root_ / "tmpfs" / "restore" / "growing";
  const auto response = StageRestore("growing");

  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INSUFFICIENT_STORAGE);
  EXPECT_FALSE(fs::exists(staged_directory));
}

TEST_F(BrokerTest, GrowingPOSIXRestoreNeverWritesPastReservationAndReleasesIt)
{
  broker_.reset();
  bool grow_once = true;
  auto engine = std::make_unique<PosixCopyEngine>(
      root_ / "storage", [&] {
        if (grow_once) {
          std::ofstream(source_ / "image", std::ios::app) << "growth";
          grow_once = false;
        }
      });
  broker_.emplace(root_ / "tmpfs", root_ / "storage", 5, nullptr,
                  std::move(engine));

  const fs::path failed = root_ / "tmpfs" / "restore" / "growing-real";
  const auto response = StageRestore("growing-real");
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INSUFFICIENT_STORAGE);
  EXPECT_FALSE(fs::exists(failed));

  std::ofstream(source_ / "image", std::ios::trunc) << "image";
  EXPECT_TRUE(StageRestore("retry-real").has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsRestoreThatShrinksWhileItIsCopied)
{
  broker_.reset();
  broker_.emplace(
      root_ / "tmpfs", root_ / "storage", FakeChangingTransferEngine::admitted_bytes, nullptr,
      std::make_unique<FakeChangingTransferEngine>(FakeChangingTransferEngine::admitted_bytes - 1));

  const fs::path staged_directory = root_ / "tmpfs" / "restore" / "shrinking";
  const auto response = StageRestore("shrinking");

  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
  EXPECT_FALSE(fs::exists(staged_directory));
}

TEST_F(BrokerTest, AdmitsRestoresWhoseArtifactsFitAggregateCapacity)
{
  broker_.reset();
  const uintmax_t artifact_bytes = fs::file_size(source_ / "image");
  broker_.emplace(root_ / "tmpfs", root_ / "storage", artifact_bytes * 2);

  ASSERT_TRUE(StageRestore("first").has_staged_restore_directory());
  ASSERT_TRUE(StageRestore("second").has_staged_restore_directory());
  const auto blocked = StageRestore("third");
  ASSERT_TRUE(blocked.has_failure());
  EXPECT_EQ(blocked.failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto commit = RequestFor("first");
  commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(StageRestore("third").has_staged_restore_directory());
}

TEST_F(BrokerTest, CheckpointReservesWholeCapacity)
{
  broker_.reset();
  const uintmax_t artifact_bytes = fs::file_size(source_ / "image");
  broker_.emplace(root_ / "tmpfs", root_ / "storage", artifact_bytes);

  auto checkpoint = RequestFor("checkpoint");
  Configure(
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_destination(),
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "published");
  ASSERT_TRUE(broker().HandleRequest(checkpoint).has_staged_checkpoint_directory());

  const auto blocked = StageRestore("restore");
  ASSERT_TRUE(blocked.has_failure());
  EXPECT_EQ(blocked.failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  ASSERT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_TRUE(StageRestore("restore").has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsZeroStagingCapacity)
{
  broker_.reset();
  EXPECT_THROW(Broker(root_ / "tmpfs", root_ / "storage", 0), std::invalid_argument);
}

TEST_F(BrokerTest, RejectsConcurrentRestoreForSameTransaction)
{
  auto first = RequestFor("restore");
  auto second = RequestFor("restore");
  Configure(
      first.mutable_staged_restore()->mutable_source(), first.mutable_staged_restore()->mutable_io_engine(), source_);
  Configure(
      second.mutable_staged_restore()->mutable_source(), second.mutable_staged_restore()->mutable_io_engine(), source_);

  Response first_response;
  Response second_response;
  std::thread first_request([&] { first_response = broker().HandleRequest(first); });
  std::thread second_request([&] { second_response = broker().HandleRequest(second); });
  first_request.join();
  second_request.join();

  ASSERT_NE(first_response.has_staged_restore_directory(), second_response.has_staged_restore_directory());
  const auto& rejected =
      first_response.has_staged_restore_directory() ? second_response : first_response;
  EXPECT_EQ(rejected.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, ReapsExpiredStagedTransactions)
{
  auto restore = RequestFor("expired");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(), source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());

  broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::minutes(9));
  EXPECT_TRUE(fs::exists(staging_directory));

  broker().ReapExpiredTransactions(std::chrono::steady_clock::now() + std::chrono::minutes(10));
  EXPECT_FALSE(fs::exists(staging_directory));

  auto commit = RequestFor("expired");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto retry = RequestFor("expired");
  Configure(retry.mutable_staged_restore()->mutable_source(), retry.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(retry).has_staged_restore_directory());
}

TEST_F(BrokerTest, ExpirationReaperSkipsBusyStagingTransaction)
{
  broker_.reset();
  auto engine = std::make_unique<FakeBlockingTransferEngine>();
  auto* blocking = engine.get();
  broker_.emplace(
      root_ / "tmpfs", root_ / "storage", kTestMaxStagingBytes,
      nullptr, std::move(engine));

  auto restore = RequestFor("busy-staging");
  Configure(
      restore.mutable_staged_restore()->mutable_source(),
      restore.mutable_staged_restore()->mutable_io_engine(), source_);
  Response staged;
  std::thread staging([&] { staged = broker().HandleRequest(restore); });
  const bool started =
      blocking->WaitUntilStageEntered(std::chrono::seconds(1));
  if (!started) {
    blocking->Release();
    staging.join();
  }
  ASSERT_TRUE(started);

  auto reap = std::async(std::launch::async, [&] {
    broker().ReapExpiredTransactions(
        std::chrono::steady_clock::now() + std::chrono::hours(24 * 365));
  });
  const bool responsive =
      reap.wait_for(std::chrono::seconds(1)) == std::future_status::ready;
  blocking->Release();
  staging.join();
  reap.get();

  ASSERT_TRUE(responsive);
  ASSERT_TRUE(staged.has_staged_restore_directory()) << staged.DebugString();
  const fs::path staging_directory(
      staged.staged_restore_directory().image_directory());
  EXPECT_TRUE(fs::exists(staging_directory));

  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::hours(24 * 365));
  EXPECT_FALSE(fs::exists(staging_directory));
  auto commit = RequestFor("busy-staging");
  commit.mutable_commit();
  EXPECT_EQ(
      broker().HandleRequest(commit).failure().code(),
      Failure::TRANSACTION_NOT_FOUND);
}

TEST_F(BrokerTest, RetainsAdmittedRestoreUntilExplicitCleanup)
{
  EnableCuda();
  const auto staged = StageRestore("admitted");
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());
  ASSERT_TRUE(BeginRestore("admitted").has_restore_admission_granted());

  broker().ReapExpiredTransactions(
      std::chrono::steady_clock::now() + std::chrono::hours(24 * 365));
  EXPECT_TRUE(fs::exists(staging_directory));

  auto abort = RequestFor("admitted");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(staging_directory));
}

TEST_F(BrokerTest, CleansStaleStagingOnStart)
{
  broker_.reset();
  const fs::path stale = root_ / "tmpfs" / "restore" / "stale";
  fs::create_directories(stale);
  std::ofstream(stale / "image") << "image";

  broker_.emplace(root_ / "tmpfs", root_ / "storage", kTestMaxStagingBytes);
  EXPECT_FALSE(fs::exists(stale));
}

TEST_F(BrokerTest, RejectsUnsafeTransactionIDs)
{
  for (const auto& id :
       {std::string("../escape"), std::string("nested/name"), std::string("nested\\name"),
        std::string("nul\0suffix", 10)}) {
    auto abort = RequestFor(id);
    abort.mutable_abort();
    const auto response = broker().HandleRequest(abort);
    EXPECT_TRUE(response.has_failure());
    EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
  }
}

TEST_F(BrokerTest, RejectsSymlinkInRestoreSource)
{
  fs::create_symlink(root_ / "storage" / "elsewhere", source_ / "link");
  auto restore = RequestFor("symlink");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
}

TEST_F(BrokerTest, RestoreCopyRemainsPinnedAcrossSourceRenameToSymlink)
{
  const fs::path original = root_ / "storage" / "original-source";
  const fs::path outside = root_ / "outside";
  const fs::path destination = root_ / "descriptor-safe-copy";
  fs::create_directories(outside);
  std::ofstream(outside / "outside-secret") << "must-not-copy";
  StorageBackend source;
  source.mutable_filesystem()->set_directory(source_.string());
  bool swapped = false;
  PosixCopyEngine engine(root_ / "storage", [&] {
    fs::rename(source_, original);
    fs::create_directory_symlink(outside, source_);
    swapped = true;
  });

  const uintmax_t copied = engine.StageRestore(source, destination, 5);

  EXPECT_TRUE(swapped);
  EXPECT_EQ(copied, 5u);
  EXPECT_TRUE(fs::exists(destination / "image"));
  EXPECT_FALSE(fs::exists(destination / "outside-secret"));
}

TEST_F(BrokerTest, InvalidRestoreDoesNotReserveTransaction)
{
  auto invalid = RequestFor("restore");
  Configure(
      invalid.mutable_staged_restore()->mutable_source(), invalid.mutable_staged_restore()->mutable_io_engine(),
      "relative");
  EXPECT_EQ(broker().HandleRequest(invalid).failure().code(), Failure::INVALID_REQUEST);

  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  EXPECT_TRUE(broker().HandleRequest(restore).has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsPathsOutsideStorageRoot)
{
  const fs::path outside = root_ / "outside";
  fs::create_directories(outside);
  std::ofstream(outside / "image") << "image";

  auto restore = RequestFor("outside-restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(), outside);
  EXPECT_EQ(broker().HandleRequest(restore).failure().code(), Failure::INVALID_REQUEST);

  auto checkpoint = RequestFor("outside-checkpoint");
  Configure(
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_destination(),
      checkpoint.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      outside / "checkpoint");
  EXPECT_EQ(broker().HandleRequest(checkpoint).failure().code(), Failure::INVALID_REQUEST);

  fs::create_directory_symlink(outside, root_ / "storage" / "link");
  auto symlinked = RequestFor("symlinked-checkpoint");
  Configure(
      symlinked.mutable_prepare_staged_checkpoint()->mutable_destination(),
      symlinked.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      root_ / "storage" / "link" / "checkpoint");
  EXPECT_EQ(broker().HandleRequest(symlinked).failure().code(), Failure::INVALID_REQUEST);

  auto root_destination = RequestFor("root-destination");
  Configure(
      root_destination.mutable_prepare_staged_checkpoint()->mutable_destination(),
      root_destination.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage");
  EXPECT_EQ(broker().HandleRequest(root_destination).failure().code(), Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, InsufficientStagingDoesNotReserveCapacity)
{
  const fs::path large = root_ / "storage" / "large";
  fs::create_directories(large);
  std::ofstream file(large / "image");
  file.seekp(1LL << 40);
  file.put('\0');
  file.close();

  auto insufficient = RequestFor("restore");
  Configure(
      insufficient.mutable_staged_restore()->mutable_source(), insufficient.mutable_staged_restore()->mutable_io_engine(),
      large);
  EXPECT_EQ(broker().HandleRequest(insufficient).failure().code(), Failure::INSUFFICIENT_STORAGE);

  auto retry = RequestFor("restore");
  Configure(
      retry.mutable_staged_restore()->mutable_source(), retry.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(retry).has_staged_restore_directory());
}

TEST_F(BrokerTest, RejectsInvalidStagedRestore)
{
  auto restore = RequestFor("restore");
  restore.mutable_staged_restore();
  const auto response = broker().HandleRequest(restore);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::INVALID_REQUEST);
}

TEST_F(BrokerTest, UnknownCommitAndAbortDoNotReserveTransactions)
{
  auto commit = RequestFor("unknown-commit");
  commit.mutable_commit();
  EXPECT_EQ(broker().HandleRequest(commit).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto restore = RequestFor("unknown-commit");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  EXPECT_TRUE(broker().HandleRequest(restore).has_staged_restore_directory());
  auto restore_commit = RequestFor("unknown-commit");
  restore_commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(restore_commit).has_commit_complete());

  auto abort = RequestFor("unknown-abort");
  abort.mutable_abort();
  EXPECT_EQ(broker().HandleRequest(abort).failure().code(), Failure::TRANSACTION_NOT_FOUND);

  auto prepare = RequestFor("unknown-abort");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "published");
  EXPECT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());
}

TEST_F(BrokerTest, EvictsOldTerminalTransactionsButRetainsRecentCompletions)
{
  auto oldest = RequestFor("oldest");
  Configure(
      oldest.mutable_prepare_staged_checkpoint()->mutable_destination(),
      oldest.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / "oldest");
  ASSERT_TRUE(broker().HandleRequest(oldest).has_staged_checkpoint_directory());
  auto oldest_commit = RequestFor("oldest");
  oldest_commit.mutable_commit();
  ASSERT_TRUE(broker().HandleRequest(oldest_commit).has_commit_complete());

  for (size_t index = 0; index < 1024; ++index) {
    const auto id = "terminal-" + std::to_string(index);
    auto prepare = RequestFor(id);
    Configure(
        prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
        prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), root_ / "storage" / id);
    ASSERT_TRUE(broker().HandleRequest(prepare).has_staged_checkpoint_directory());
    auto commit = RequestFor(id);
    commit.mutable_commit();
    ASSERT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  }

  auto recent_commit = RequestFor("terminal-1023");
  recent_commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(recent_commit).has_commit_complete());

  auto reuse = RequestFor("oldest");
  Configure(
      reuse.mutable_staged_restore()->mutable_source(), reuse.mutable_staged_restore()->mutable_io_engine(), source_);
  EXPECT_TRUE(broker().HandleRequest(reuse).has_staged_restore_directory());
}

TEST_F(BrokerTest, AbortsFailedCheckpointStaging)
{
  const fs::path destination = root_ / "storage" / "published";
  const fs::path staging_directory = root_ / "tmpfs" / "checkpoint" / "checkpoint";
  fs::create_symlink(root_ / "missing", staging_directory);
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), destination);
  const auto failed = broker().HandleRequest(prepare);
  ASSERT_TRUE(failed.has_failure());
  EXPECT_EQ(failed.failure().code(), Failure::STORAGE_ERROR);

  auto abort = RequestFor("checkpoint");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());

  const auto retry = broker().HandleRequest(prepare);
  ASSERT_TRUE(retry.has_failure());
  EXPECT_EQ(retry.failure().code(), Failure::TRANSACTION_CONFLICT);
}

TEST_F(BrokerTest, PublishesCheckpoint)
{
  const fs::path published = root_ / "storage" / "published";
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  const fs::path staging_directory(output.staged_checkpoint_directory().image_directory());
  std::ofstream(staging_directory / "image") << "image";
  std::ofstream(staging_directory / ".destination") << "image";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_TRUE(fs::exists(published / "image"));
  EXPECT_TRUE(fs::exists(published / ".destination"));
  EXPECT_FALSE(fs::exists(staging_directory));
}

TEST_F(BrokerTest, PublishesCheckpointWithNestedModes)
{
  const fs::path published = root_ / "storage" / "published-modes";
  auto prepare = RequestFor("checkpoint-modes");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  const fs::path staging_directory(output.staged_checkpoint_directory().image_directory());
  const fs::path tools = staging_directory / "cuda-tools";
  fs::create_directory(tools);
  fs::permissions(tools, fs::perms::owner_all | fs::perms::group_read | fs::perms::group_exec);
  const fs::path executable = tools / "cuda-checkpoint";
  std::ofstream(executable) << "tool";
  fs::permissions(executable, fs::perms::owner_all | fs::perms::group_read | fs::perms::group_exec);
  fs::permissions(tools, fs::perms::owner_read | fs::perms::owner_exec);

  auto commit = RequestFor("checkpoint-modes");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  std::string copied_contents;
  std::ifstream(published / "cuda-tools" / "cuda-checkpoint") >> copied_contents;
  EXPECT_EQ(copied_contents, "tool");
  EXPECT_EQ(
      fs::status(published / "cuda-tools" / "cuda-checkpoint").permissions(),
      fs::perms::owner_all | fs::perms::group_read | fs::perms::group_exec);
  EXPECT_EQ(fs::status(published / "cuda-tools").permissions(), fs::perms::owner_read | fs::perms::owner_exec);
}

TEST_F(BrokerTest, RejectsCheckpointWithNestedSymlink)
{
  const fs::path published = root_ / "storage" / "published-symlink";
  auto prepare = RequestFor("checkpoint-symlink");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  const fs::path staging_directory(output.staged_checkpoint_directory().image_directory());
  fs::create_symlink(root_ / "storage", staging_directory / "link");

  auto commit = RequestFor("checkpoint-symlink");
  commit.mutable_commit();
  const auto response = broker().HandleRequest(commit);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
  EXPECT_FALSE(fs::exists(published));
  EXPECT_FALSE(fs::exists(published.string() + ".pagebroker-partial"));
}

TEST_F(BrokerTest, CheckpointPublishRejectsParentReplacedBySymlink)
{
  const fs::path parent = root_ / "storage" / "publish-parent";
  const fs::path held_parent = root_ / "storage" / "held-publish-parent";
  const fs::path outside = root_ / "outside-publish";
  const fs::path published = parent / "checkpoint";
  fs::create_directories(parent);
  fs::create_directories(outside);
  auto prepare = RequestFor("checkpoint-parent-swap");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(),
      published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  const fs::path staging_directory(
      output.staged_checkpoint_directory().image_directory());
  std::ofstream(staging_directory / "image") << "image";

  fs::rename(parent, held_parent);
  fs::create_directory_symlink(outside, parent);
  auto commit = RequestFor("checkpoint-parent-swap");
  commit.mutable_commit();
  const auto response = broker().HandleRequest(commit);

  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::STORAGE_ERROR);
  EXPECT_FALSE(fs::exists(outside / "checkpoint"));
}

TEST_F(BrokerTest, ReplacesExistingCheckpoint)
{
  const fs::path published = root_ / "storage" / "published";
  const fs::path previous = published.string() + ".pagebroker-previous";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";

  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "new") << "new";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_commit_complete());
  EXPECT_FALSE(fs::exists(published / "old"));
  EXPECT_TRUE(fs::exists(published / "new"));
  EXPECT_FALSE(fs::exists(previous));
}

TEST_F(BrokerTest, PreservesExistingCheckpointWhenReplacementFails)
{
  const fs::path published = root_ / "storage" / "published";
  const fs::path previous = published.string() + ".pagebroker-previous";
  fs::create_directories(published);
  std::ofstream(published / "old") << "old";
  std::ofstream(previous) << "previous";

  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), published);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "new") << "new";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  EXPECT_TRUE(broker().HandleRequest(commit).has_failure());
  EXPECT_TRUE(fs::exists(published / "old"));
  EXPECT_TRUE(fs::exists(previous));
}

TEST_F(BrokerTest, PreservesExistingPartialCheckpointDestination)
{
  const fs::path destination = root_ / "storage" / "blocked";
  auto prepare = RequestFor("checkpoint");
  Configure(
      prepare.mutable_prepare_staged_checkpoint()->mutable_destination(),
      prepare.mutable_prepare_staged_checkpoint()->mutable_io_engine(), destination);
  const auto output = broker().HandleRequest(prepare);
  ASSERT_TRUE(output.has_staged_checkpoint_directory());
  std::ofstream(fs::path(output.staged_checkpoint_directory().image_directory()) / "image") << "image";
  const fs::path partial = destination.string() + ".pagebroker-partial";
  std::ofstream(partial) << "keep";

  auto commit = RequestFor("checkpoint");
  commit.mutable_commit();
  const auto response = broker().HandleRequest(commit);
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::TRANSACTION_CONFLICT);
  EXPECT_TRUE(fs::exists(partial));
  std::string partial_contents;
  std::ifstream(partial) >> partial_contents;
  EXPECT_EQ(partial_contents, "keep");
}

TEST_F(BrokerTest, AbortsRestore)
{
  auto restore = RequestFor("restore");
  Configure(
      restore.mutable_staged_restore()->mutable_source(), restore.mutable_staged_restore()->mutable_io_engine(),
      source_);
  const auto staged = broker().HandleRequest(restore);
  ASSERT_TRUE(staged.has_staged_restore_directory());
  const fs::path staging_directory(staged.staged_restore_directory().image_directory());

  auto abort = RequestFor("restore");
  abort.mutable_abort();
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_TRUE(broker().HandleRequest(abort).has_abort_complete());
  EXPECT_FALSE(fs::exists(staging_directory));

  auto commit = RequestFor("restore");
  commit.mutable_commit();
  const auto commit_response = broker().HandleRequest(commit);
  ASSERT_TRUE(commit_response.has_failure());
  EXPECT_EQ(commit_response.failure().code(), Failure::TRANSACTION_NOT_FOUND);
}

}  // namespace
