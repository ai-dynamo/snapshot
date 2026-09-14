// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "cuda_worker_pool.hpp"

#include <gtest/gtest.h>

#include <signal.h>
#include <unistd.h>

#include <algorithm>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <string>
#include <thread>

namespace snapshot::pagebroker {
namespace {
namespace protocol = cuda_checkpoint_daemon;
namespace fs = std::filesystem;

std::string fake_worker;

class TemporaryDirectory {
public:
  TemporaryDirectory() {
    char pattern[] = "/tmp/pagebroker-cuda-worker-test-XXXXXX";
    const char *created = mkdtemp(pattern);
    EXPECT_NE(created, nullptr);
    if (created != nullptr)
      path_ = created;
  }
  ~TemporaryDirectory() { fs::remove_all(path_); }
  const std::string &path() const { return path_; }

private:
  std::string path_;
};

CudaWorkerPoolConfig Config(const std::string &directory,
                            size_t worker_count) {
  return {.worker_count = worker_count,
          .worker_binary = fake_worker,
          .private_socket_directory = directory,
          .process_root = "/proc",
          .max_operation_seconds = 1,
          .rpc_timeout = std::chrono::milliseconds(80),
          .health_rpc_timeout = std::chrono::milliseconds(40),
          .startup_timeout = std::chrono::seconds(3),
          .health_interval = std::chrono::milliseconds(50)};
}

protocol::Request MutatingRequest(std::string behavior) {
  return {.action = protocol::Action::kRestore,
          .backend = protocol::Backend::kRegular,
          .pid = 123,
          .expected_start_time_ticks = 1,
          .device_map = std::move(behavior),
          .expected_cgroup = "0::/test\n"};
}

TEST(CudaWorkerClientTest, ClassifiesBoundedProtocolFailuresWithoutReplay) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 1));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  const auto client = lease->workers().front().client;

  const auto malformed = client->Call(MutatingRequest("malformed"));
  EXPECT_EQ(malformed.status, CudaWorkerRpcStatus::kProtocolError);
  EXPECT_TRUE(malformed.unknown_outcome);
  const auto bad_header = client->Call(MutatingRequest("bad-header"));
  EXPECT_EQ(bad_header.status, CudaWorkerRpcStatus::kProtocolError);
  EXPECT_TRUE(bad_header.unknown_outcome);
  const auto oversized = client->Call(MutatingRequest("oversized"));
  EXPECT_EQ(oversized.status, CudaWorkerRpcStatus::kProtocolError);
  EXPECT_TRUE(oversized.unknown_outcome);
  const auto disconnected = client->Call(MutatingRequest("disconnect"));
  EXPECT_EQ(disconnected.status, CudaWorkerRpcStatus::kDisconnected);
  EXPECT_TRUE(disconnected.unknown_outcome);
  const auto fatal = client->Call(MutatingRequest("fatal"));
  EXPECT_EQ(fatal.status, CudaWorkerRpcStatus::kFatalResponse);
  EXPECT_TRUE(fatal.unknown_outcome);
  const auto timeout = client->Call(MutatingRequest("timeout"));
  EXPECT_EQ(timeout.status, CudaWorkerRpcStatus::kTimeout);
  EXPECT_TRUE(timeout.unknown_outcome);

  protocol::Request invalid_request;
  invalid_request.expected_cgroup.assign(protocol::kMaxCgroupSize + 1, 'x');
  const auto invalid = client->Call(invalid_request);
  EXPECT_EQ(invalid.status, CudaWorkerRpcStatus::kInvalidRequest);
  EXPECT_FALSE(invalid.unknown_outcome);
  lease->Release();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, PropagatesConfiguredCustomStorageRoot) {
  TemporaryDirectory directory;
  auto config = Config(directory.path(), 1);
  config.storage_root = directory.path();
  CudaWorkerPool pool(std::move(config));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  protocol::Request request{
      .action = protocol::Action::kRestore,
      .backend = protocol::Backend::kPosix,
      .pid = 123,
      .transfer_buffer_count = 1,
      .transfer_chunk_bytes = 64ULL * 1024ULL * 1024ULL,
      .expected_start_time_ticks = 1,
      .storage_dir =
          (fs::path(directory.path()) / "restore" / "process-nspid-123")
              .string(),
      .expected_cgroup = "0::/test\n",
      .selected_devices =
          "GPU-00000000-0000-0000-0000-000000000000",
  };
  const auto result = lease->Call(0, request);
  EXPECT_TRUE(result) << result.error;
  EXPECT_EQ(result.response.cuda_status, 0);
  lease->Release();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, PrewarmsHealthAndLeasesFourOrEightAtomically) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 8));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  ASSERT_TRUE(pool.ready());
  const auto snapshot = pool.Snapshot();
  ASSERT_EQ(snapshot.size(), 8U);
  for (const auto &worker : snapshot) {
    EXPECT_TRUE(worker.alive);
    EXPECT_TRUE(worker.healthy);
    EXPECT_FALSE(worker.incarnation.empty());
  }

  auto first = pool.TryAcquire(4);
  auto second = pool.TryAcquire(4);
  ASSERT_TRUE(first.has_value());
  ASSERT_TRUE(second.has_value());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  first->Release();
  auto replacement = pool.TryAcquire(4);
  EXPECT_TRUE(replacement.has_value());
  EXPECT_FALSE(pool.TryAcquire(8).has_value());
  replacement->Release();
  second->Release();
  auto all = pool.TryAcquire(8);
  ASSERT_TRUE(all.has_value());
  EXPECT_EQ(all->workers().size(), 8U);
  all->Release();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, AssignedChildLossRequiresFailStopAndNeverRespawns) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 4));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  const pid_t lost_pid = lease->workers().front().pid;
  ASSERT_EQ(kill(lost_pid, SIGKILL), 0);

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  ASSERT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.ready());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  lease->Release();

  const auto snapshot = pool.Snapshot();
  ASSERT_EQ(snapshot.size(), 4U);
  size_t lost = 0;
  for (const auto &worker : snapshot) {
    if (worker.pid == lost_pid) {
      ++lost;
      EXPECT_FALSE(worker.alive);
      EXPECT_TRUE(worker.unknown_outcome);
    }
  }
  EXPECT_EQ(lost, 1U);
  std::this_thread::sleep_for(std::chrono::milliseconds(100));
  EXPECT_EQ(pool.Snapshot().size(), 4U);
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, UnexpectedIdleChildLossRestartsWholePoolGeneration) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 4));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  const auto workers = pool.Snapshot();
  ASSERT_EQ(workers.size(), 4U);
  ASSERT_EQ(kill(workers.front().pid, SIGKILL), 0);

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  EXPECT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.ready());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  const auto after = pool.Snapshot();
  EXPECT_EQ(after.size(), 4U);
  const auto lost = std::find_if(
      after.begin(), after.end(), [&](const auto& worker) {
        return worker.pid == workers.front().pid;
      });
  ASSERT_NE(lost, after.end());
  EXPECT_FALSE(lost->alive);
  EXPECT_FALSE(lost->unknown_outcome);
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest,
     MonitorFailStopWinsDispatchBoundaryForAnExistingLease) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 2));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  const pid_t leased_pid = lease->workers().front().pid;
  const auto snapshot = pool.Snapshot();
  const auto idle = std::find_if(
      snapshot.begin(), snapshot.end(),
      [leased_pid](const auto& worker) { return worker.pid != leased_pid; });
  ASSERT_NE(idle, snapshot.end());
  ASSERT_EQ(kill(idle->pid, SIGKILL), 0);

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  ASSERT_TRUE(pool.fail_stop_required());

  const auto rejected = lease->Call(0, MutatingRequest("fatal"));
  EXPECT_EQ(rejected.status, CudaWorkerRpcStatus::kPoolFailStopped);
  EXPECT_EQ(rejected.error, "CUDA worker pool fail-stopped before dispatch");
  EXPECT_FALSE(rejected.unknown_outcome);
  lease->Release();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest,
     TransientHealthTimeoutSuspendsAdmissionThenRecoversWithoutFailStop) {
  TemporaryDirectory directory;
  auto config = Config(directory.path(), 2);
  config.health_interval = std::chrono::milliseconds(500);
  CudaWorkerPool pool(std::move(config));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  const auto first_socket = fs::path(directory.path()) /
                            "worker-0.sock.hang-health-once";
  std::ofstream(first_socket) << "hang";

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(5);
  bool observed_quarantine = false;
  while (std::chrono::steady_clock::now() < deadline) {
    const auto workers = pool.Snapshot();
    if (!workers.front().healthy) {
      observed_quarantine = true;
      EXPECT_FALSE(pool.fail_stop_required());
      EXPECT_FALSE(pool.TryAcquire(1).has_value());
      break;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  ASSERT_TRUE(observed_quarantine);

  const auto recovery_deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(5);
  while (!pool.ready() && std::chrono::steady_clock::now() < recovery_deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  EXPECT_TRUE(pool.ready());
  EXPECT_FALSE(pool.fail_stop_required());
  auto lease = pool.TryAcquire(2);
  while (!lease && std::chrono::steady_clock::now() < recovery_deadline) {
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
    lease = pool.TryAcquire(2);
  }
  EXPECT_TRUE(lease.has_value());
  if (lease)
    lease->Release();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, PersistentHealthTimeoutRequiresFailStop) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 2));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  const auto first_socket =
      fs::path(directory.path()) / "worker-0.sock.hang-health";
  std::ofstream(first_socket) << "hang";

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  EXPECT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  fs::remove(first_socket);
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, IncarnationMismatchRequiresImmediateFailStop) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 2));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  const auto marker =
      fs::path(directory.path()) / "worker-0.sock.wrong-incarnation";
  std::ofstream(marker) << "wrong";

  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  EXPECT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  fs::remove(marker);
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, RetainedContextOwnerLossRequiresFailStopAfterLeaseRelease) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 2));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  const pid_t retained_pid = lease->workers().front().pid;
  auto retention = lease->Retain(0);
  ASSERT_NE(retention, nullptr);
  lease->Release();

  ASSERT_EQ(kill(retained_pid, SIGKILL), 0);
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (!pool.fail_stop_required() &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  EXPECT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  retention.reset();
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, PoisonedMutatingRpcLeaseIsNeverReused) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 2));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  auto lease = pool.TryAcquire(1);
  ASSERT_TRUE(lease.has_value());
  lease->Poison(0);
  lease->Release();

  EXPECT_TRUE(pool.fail_stop_required());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
}

TEST(CudaWorkerPoolTest, GracefulShutdownReapsEveryChild) {
  TemporaryDirectory directory;
  CudaWorkerPool pool(Config(directory.path(), 4));
  std::string error;
  ASSERT_TRUE(pool.Start(&error)) << error;
  EXPECT_FALSE(pool.TryAcquire(8).has_value());
  auto all_four = pool.TryAcquire(4);
  ASSERT_TRUE(all_four.has_value());
  EXPECT_FALSE(pool.TryAcquire(1).has_value());
  ASSERT_TRUE(pool.Shutdown(std::chrono::seconds(3), &error)) << error;
  for (const auto &worker : pool.Snapshot()) {
    EXPECT_FALSE(worker.alive);
    EXPECT_EQ(kill(worker.pid, 0), -1);
    EXPECT_EQ(errno, ESRCH);
  }
}

} // namespace
} // namespace snapshot::pagebroker

int main(int argc, char **argv) {
  if (argc != 2)
    return 2;
  snapshot::pagebroker::fake_worker =
      std::filesystem::absolute(argv[1]).lexically_normal().string();
  testing::InitGoogleTest(&argc, argv);
  return RUN_ALL_TESTS();
}
