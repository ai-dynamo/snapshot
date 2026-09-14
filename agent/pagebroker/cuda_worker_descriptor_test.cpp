// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "cuda_worker_client.hpp"

#include <cuda.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#include <array>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <stdexcept>
#include <thread>
#include <vector>

#include <gtest/gtest.h>

#include "daemon_server.h"
#include "cuda_operation.h"
#include "storage_manifest.h"

namespace snapshot::pagebroker {
namespace {
namespace fs = std::filesystem;
namespace protocol = cuda_checkpoint_daemon;
namespace server = cuda_checkpoint_server;
namespace storage = cuda_checkpoint_storage;

class TemporaryDirectory {
public:
  TemporaryDirectory() {
    std::error_code create_error;
    fs::create_directories("/checkpoints", create_error);
    if (create_error)
      throw std::runtime_error("create /checkpoints failed");
    std::array<char, 96> path{};
    std::snprintf(path.data(), path.size(),
                  "/checkpoints/pb-worker-fd-XXXXXX");
    char *created = mkdtemp(path.data());
    if (created == nullptr)
      throw std::runtime_error("mkdtemp failed");
    path_ = created;
  }
  ~TemporaryDirectory() {
    std::error_code ignored;
    fs::remove_all(path_, ignored);
  }
  const fs::path &path() const { return path_; }

private:
  fs::path path_;
};

protocol::Request RestoreRequest(const fs::path &storage_dir, int descriptor,
                                 uint64_t carrier_size) {
  protocol::Request request{
      .action = protocol::Action::kRestore,
      .backend = protocol::Backend::kPosix,
      .pid = 123,
      .transfer_buffer_count = 1,
      .transfer_chunk_bytes = 64ULL * 1024ULL * 1024ULL,
      .expected_start_time_ticks = 456,
      .device_map = "GPU-00000000-0000-0000-0000-000000000000="
                    "GPU-00000000-0000-0000-0000-000000000000",
      .storage_dir = storage_dir.string(),
      .expected_cgroup = "0::/test\n",
      .selected_devices =
          "GPU-00000000-0000-0000-0000-000000000000",
  };
  request.pinned_storage_files.push_back({
      .filename = "device-0000.bin",
      .descriptor_fd = descriptor,
      .size = carrier_size,
  });
  return request;
}

class WorkerSocket {
public:
  explicit WorkerSocket(const fs::path &path) : path_(path) {
    fd_ = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
    if (fd_ < 0)
      throw std::runtime_error("socket failed");
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    const std::string value = path_.string();
    std::memcpy(address.sun_path, value.c_str(), value.size() + 1);
    if (bind(fd_, reinterpret_cast<sockaddr *>(&address), sizeof(address)) !=
            0 ||
        listen(fd_, 1) != 0)
      throw std::runtime_error("bind/listen failed");
  }
  ~WorkerSocket() {
    if (fd_ >= 0)
      close(fd_);
    std::error_code ignored;
    fs::remove(path_, ignored);
  }
  int fd() const { return fd_; }
  const fs::path &path() const { return path_; }

private:
  fs::path path_;
  int fd_ = -1;
};

void Reply(int client, int32_t status, const std::string &error) {
  protocol::Response response{.cuda_status = status, .error = error};
  std::vector<unsigned char> encoded;
  std::string encode_error;
  ASSERT_TRUE(protocol::EncodeResponse(response, &encoded, &encode_error));
  ASSERT_EQ(send(client, encoded.data(), encoded.size(), MSG_NOSIGNAL),
            static_cast<ssize_t>(encoded.size()));
}

class WatchdogService final : public cuda_checkpoint_operation::Service {
public:
  explicit WatchdogService(int events)
      : Service(std::chrono::seconds(1)), events_(events) {}

  CUresult BeginShutdown(const std::string &, std::string *error) override {
    error->clear();
    const char event = 'A';
    (void)write(events_, &event, sizeof(event));
    return CUDA_SUCCESS;
  }

  CUresult TerminateRetainedTargets(const std::string &,
                                    std::string *error) override {
    error->clear();
    const char event = 'R';
    (void)write(events_, &event, sizeof(event));
    return CUDA_SUCCESS;
  }

private:
  int events_;
};

TEST(CudaWorkerDescriptors, TransfersAnUnlinkedCarrierByDescriptor) {
  TemporaryDirectory temporary;
  const fs::path storage_dir = temporary.path() / "storage";
  ASSERT_TRUE(fs::create_directory(storage_dir));
  const std::string payload = "carrier";
  const fs::path carrier_path = temporary.path() / "carrier";
  std::ofstream(carrier_path) << payload;
  const int carrier =
      open(carrier_path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  ASSERT_GE(carrier, 0);
  ASSERT_TRUE(fs::remove(carrier_path));
  std::string manifest_error;
  ASSERT_TRUE(storage::WriteManifest(
      storage_dir,
      {{.source_uuid = "GPU-00000000-0000-0000-0000-000000000000",
        .size = payload.size(),
        .filename = "device-0000.bin",
        .sha256 = std::string(64, '0')}},
      &manifest_error));
  WorkerSocket worker(temporary.path() / "worker.sock");
  std::thread server_thread([&] {
    const int client = accept4(worker.fd(), nullptr, nullptr, SOCK_CLOEXEC);
    ASSERT_GE(client, 0);
    std::vector<unsigned char> packet(protocol::kMaxRequestSize + 1);
    protocol::Request received;
    std::vector<int> descriptors;
    std::string error;
    ASSERT_TRUE(server::ReceiveWorkerRequest(client, &packet, &received,
                                             &descriptors, &error,
                                             temporary.path().string()))
        << error;
    ASSERT_EQ(received.pinned_storage_files.size(), 1u);
    std::array<char, 16> contents{};
    ASSERT_EQ(pread(received.pinned_storage_files[0].descriptor_fd,
                    contents.data(), contents.size(), 0),
              static_cast<ssize_t>(payload.size()));
    EXPECT_EQ(std::string(contents.data(), payload.size()), payload);
    for (const int fd : descriptors)
      close(fd);
    Reply(client, 0, "");
    close(client);
  });
  CudaWorkerClient client(worker.path().string(), std::chrono::seconds(5),
                          std::chrono::seconds(1));
  const auto result =
      client.Call(RestoreRequest(storage_dir, carrier, payload.size()));
  EXPECT_TRUE(result);
  EXPECT_EQ(result.response.cuda_status, 0);
  server_thread.join();
  close(carrier);
}

TEST(CudaWorkerDescriptors, TransfersBatchCarriersInTargetOrder) {
  TemporaryDirectory temporary;
  const fs::path first_storage = temporary.path() / "storage-1";
  const fs::path second_storage = temporary.path() / "storage-2";
  ASSERT_TRUE(fs::create_directory(first_storage));
  ASSERT_TRUE(fs::create_directory(second_storage));
  const std::array<std::string, 2> payloads{"first-carrier",
                                             "second-carrier"};
  std::array<int, 2> carriers{-1, -1};
  for (size_t index = 0; index < carriers.size(); ++index) {
    const fs::path carrier_path =
        temporary.path() / ("carrier-" + std::to_string(index));
    std::ofstream(carrier_path) << payloads[index];
    carriers[index] =
        open(carrier_path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    ASSERT_GE(carriers[index], 0);
    ASSERT_TRUE(fs::remove(carrier_path));
    std::string manifest_error;
    ASSERT_TRUE(storage::WriteManifest(
        index == 0 ? first_storage : second_storage,
        {{.source_uuid = "GPU-00000000-0000-0000-0000-000000000000",
          .size = payloads[index].size(),
          .filename = "device-0000.bin",
          .sha256 = std::string(64, '0')}},
        &manifest_error));
  }
  auto first = RestoreRequest(first_storage, carriers[0], payloads[0].size());
  auto second =
      RestoreRequest(second_storage, carriers[1], payloads[1].size());
  second.pid = 124;
  second.expected_start_time_ticks = 457;
  protocol::Request batch{
      .action = protocol::Action::kRestoreBatch,
      .backend = protocol::Backend::kPosix,
      .pid = 2,
      .targets = {first, second},
  };

  WorkerSocket worker(temporary.path() / "worker.sock");
  std::thread server_thread([&] {
    const int client = accept4(worker.fd(), nullptr, nullptr, SOCK_CLOEXEC);
    ASSERT_GE(client, 0);
    std::vector<unsigned char> packet(protocol::kMaxRequestSize + 1);
    protocol::Request received;
    std::vector<int> descriptors;
    std::string error;
    ASSERT_TRUE(server::ReceiveWorkerRequest(client, &packet, &received,
                                             &descriptors, &error,
                                             temporary.path().string()))
        << error;
    ASSERT_EQ(received.targets.size(), 2u);
    for (size_t index = 0; index < received.targets.size(); ++index) {
      ASSERT_EQ(received.targets[index].pinned_storage_files.size(), 1u);
      std::array<char, 32> contents{};
      ASSERT_EQ(
          pread(received.targets[index].pinned_storage_files[0].descriptor_fd,
                contents.data(), contents.size(), 0),
          static_cast<ssize_t>(payloads[index].size()));
      EXPECT_EQ(std::string(contents.data(), payloads[index].size()),
                payloads[index]);
    }
    for (const int fd : descriptors)
      close(fd);
    Reply(client, 0, "");
    close(client);
  });
  CudaWorkerClient client(worker.path().string(), std::chrono::seconds(5),
                          std::chrono::seconds(1));
  const auto result = client.Call(batch);
  EXPECT_TRUE(result);
  EXPECT_EQ(result.response.cuda_status, 0);
  server_thread.join();
  for (const int carrier : carriers)
    close(carrier);
}

TEST(CudaWorkerDescriptors, RejectsCarrierWhoseSizeDoesNotMatchManifest) {
  TemporaryDirectory temporary;
  const fs::path storage_dir = temporary.path() / "storage";
  ASSERT_TRUE(fs::create_directory(storage_dir));
  const fs::path carrier_path = temporary.path() / "carrier";
  std::ofstream(carrier_path) << "short";
  const int carrier = open(carrier_path.c_str(), O_RDONLY | O_CLOEXEC);
  ASSERT_GE(carrier, 0);
  std::string manifest_error;
  ASSERT_TRUE(storage::WriteManifest(
      storage_dir,
      {{.source_uuid = "GPU-00000000-0000-0000-0000-000000000000",
        .size = 99,
        .filename = "device-0000.bin",
        .sha256 = std::string(64, '0')}},
      &manifest_error));
  WorkerSocket worker(temporary.path() / "worker.sock");
  std::thread server_thread([&] {
    const int client = accept4(worker.fd(), nullptr, nullptr, SOCK_CLOEXEC);
    ASSERT_GE(client, 0);
    std::vector<unsigned char> packet(protocol::kMaxRequestSize + 1);
    protocol::Request received;
    std::vector<int> descriptors;
    std::string error;
    EXPECT_FALSE(server::ReceiveWorkerRequest(client, &packet, &received,
                                              &descriptors, &error,
                                              temporary.path().string()));
    EXPECT_NE(error.find("does not match"), std::string::npos);
    for (const int fd : descriptors)
      close(fd);
    Reply(client, CUDA_ERROR_INVALID_VALUE, error);
    close(client);
  });
  CudaWorkerClient client(worker.path().string(), std::chrono::seconds(5),
                          std::chrono::seconds(1));
  const auto result = client.Call(RestoreRequest(storage_dir, carrier, 5));
  EXPECT_TRUE(result);
  EXPECT_EQ(result.response.cuda_status, CUDA_ERROR_INVALID_VALUE);
  server_thread.join();
  close(carrier);
}

TEST(CudaWorkerShutdown,
     SignalWatchdogExitsBlockedWorkerOnlyAfterTargetTermination) {
  int events[2];
  ASSERT_EQ(pipe(events), 0);
  const pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    close(events[0]);
    protocol::ShutdownSignalOwner signal_owner;
    std::string error;
    if (!signal_owner.Start(&error))
      _exit(90);
    WatchdogService service(events[1]);
    std::jthread watchdog([&](std::stop_token stop) {
      server::RunShutdownWatchdog(
          stop, &signal_owner, &service, "/proc",
          std::chrono::milliseconds(100), [&] {
            const char event = 'E';
            (void)write(events[1], &event, sizeof(event));
            _exit(42);
          });
    });
    const char ready = 'B';
    if (write(events[1], &ready, sizeof(ready)) != sizeof(ready))
      _exit(91);
    // Model an operation thread stuck inside a CUDA driver call. The signal
    // owner consumes SIGTERM on its separate thread, so this thread never
    // returns through the ordinary daemon cleanup path.
    for (;;)
      pause();
  }

  close(events[1]);
  char event = 0;
  ASSERT_EQ(read(events[0], &event, sizeof(event)), sizeof(event));
  ASSERT_EQ(event, 'B');
  const int event_flags = fcntl(events[0], F_GETFL);
  ASSERT_GE(event_flags, 0);
  ASSERT_EQ(fcntl(events[0], F_SETFL, event_flags | O_NONBLOCK), 0);
  ASSERT_EQ(kill(child, SIGTERM), 0);

  int status = 0;
  pid_t waited = 0;
  std::string observed(1, event);
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(3);
  while (std::chrono::steady_clock::now() < deadline) {
    char next = 0;
    const ssize_t bytes = read(events[0], &next, sizeof(next));
    if (bytes == sizeof(next))
      observed.push_back(next);
    waited = waitpid(child, &status, WNOHANG);
    if (waited == child)
      break;
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
  if (waited != child) {
    (void)kill(child, SIGKILL);
    (void)waitpid(child, &status, 0);
  } else {
    char remaining[16];
    for (;;) {
      const ssize_t bytes = read(events[0], remaining, sizeof(remaining));
      if (bytes > 0) {
        observed.append(remaining, static_cast<size_t>(bytes));
        continue;
      }
      if (bytes < 0 && errno == EINTR)
        continue;
      break;
    }
  }
  close(events[0]);

  ASSERT_EQ(waited, child) << "blocked worker exceeded bounded shutdown";
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 42);
  const size_t exit_event = observed.find('E');
  ASSERT_NE(exit_event, std::string::npos);
  EXPECT_NE(observed.substr(0, exit_event).find('A'), std::string::npos);
  EXPECT_NE(observed.substr(0, exit_event).find('R'), std::string::npos);
}

} // namespace
} // namespace snapshot::pagebroker
