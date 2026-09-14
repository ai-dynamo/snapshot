// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <stdexcept>
#include <string_view>
#include <thread>

#include "content_digest.h"
#include "cuinterpose_coordinator.hpp"
#include "protocol.h"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

class TemporaryDirectory {
public:
  TemporaryDirectory() {
    char pattern[] = "/tmp/pagebroker-cuinterpose-XXXXXX";
    char *created = mkdtemp(pattern);
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

class UnixListener {
public:
  explicit UnixListener(const fs::path &path) {
    fd_ = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (fd_ < 0)
      throw std::runtime_error("socket failed");
    struct sockaddr_un address {};
    address.sun_family = AF_UNIX;
    const std::string value = path.string();
    if (value.size() >= sizeof(address.sun_path))
      throw std::runtime_error("socket path is too long");
    std::snprintf(address.sun_path, sizeof(address.sun_path), "%s",
                  value.c_str());
    if (bind(fd_, reinterpret_cast<struct sockaddr *>(&address),
             sizeof(address)) != 0)
      throw std::runtime_error("bind failed");
  }
  ~UnixListener() {
    if (fd_ >= 0)
      close(fd_);
  }

private:
  int fd_ = -1;
};

fs::path WriteScript(const fs::path &directory, std::string_view body) {
  const fs::path script = directory / "coordinator";
  std::ofstream output(script);
  output << "#!/bin/sh\n" << body;
  output.close();
  if (chmod(script.c_str(), 0700) != 0)
    throw std::runtime_error("chmod failed");
  return script;
}

TEST(CuinterposeCoordinatorTest, EnforcesAllOrNoneEndpointContract) {
  TemporaryDirectory temporary;
  const fs::path proc = temporary.path() / "proc";
  const fs::path control = proc / "123/root/snapshot-control";
  fs::create_directories(control);
  CuinterposeCoordinator coordinator("/bin/true", proc,
                                     std::chrono::seconds(1));
  const std::vector<CuinterposeTarget> targets{
      {.host_pid = 123, .namespace_pid = 7}};
  std::string error;
  EXPECT_TRUE(coordinator.ValidateEndpoints(targets, false, &error));
  EXPECT_FALSE(coordinator.ValidateEndpoints(targets, true, &error));

  UnixListener listener(control / "cuinterpose-7.sock");
  EXPECT_TRUE(coordinator.ValidateEndpoints(targets, true, &error));
  EXPECT_FALSE(coordinator.ValidateEndpoints(targets, false, &error));
}

TEST(CuinterposeCoordinatorTest, RejectsSymlinkAndHashesBoundedState) {
  TemporaryDirectory temporary;
  const fs::path staging = temporary.path() / "staging";
  fs::create_directories(staging);
  CuinterposeCoordinator coordinator("/bin/true", temporary.path(),
                                     std::chrono::seconds(1));
  std::string error;
  CuinterposeStateMetadata metadata;
  ASSERT_NO_THROW(fs::create_symlink("missing", staging / "cuinterpose.state"));
  EXPECT_FALSE(coordinator.ReadState(staging, 2, &metadata, &error));
  fs::remove(staging / "cuinterpose.state");

  std::ofstream(staging / "cuinterpose.state")
      << CUINTERPOSE_STATE_HEADER << '\n';
  ASSERT_TRUE(coordinator.ReadState(staging, 2, &metadata, &error)) << error;
  EXPECT_EQ(metadata.protocol_version, CUINTERPOSE_VERSION);
  EXPECT_EQ(metadata.participant_count, 2u);
  EXPECT_EQ(metadata.size_bytes, 21u);
  EXPECT_TRUE(cuda_checkpoint_storage::IsSHA256Hex(metadata.sha256));
}

TEST(CuinterposeCoordinatorTest, ValidatesSavedTopologyInCoordinator) {
  TemporaryDirectory temporary;
  const fs::path staging = temporary.path() / "staging";
  fs::create_directories(staging);
  std::ofstream(staging / "cuinterpose.state")
      << CUINTERPOSE_STATE_HEADER << '\n'
      << "participant 0123456789abcdef0123456789abcdef 0\n";
  CuinterposeCoordinator coordinator(
      fs::absolute("build/cuinterpose/cuinterpose-coordinator"),
      temporary.path(), std::chrono::seconds(1));
  std::atomic<bool> shutting_down{false};
  const auto result = coordinator.ValidateState(staging, shutting_down);
  EXPECT_TRUE(result.succeeded) << result.error;
}

TEST(CuinterposeCoordinatorTest, BoundsCoordinatorRuntime) {
  TemporaryDirectory temporary;
  const fs::path script = WriteScript(temporary.path(), "sleep 10\n");
  CuinterposeCoordinator coordinator(script, temporary.path(),
                                     std::chrono::seconds(1));
  std::atomic<bool> shutting_down{false};
  const auto start = std::chrono::steady_clock::now();
  const auto result = coordinator.Prepare(
      {{.host_pid = 1, .namespace_pid = 1}}, temporary.path(), shutting_down,
      false);
  const auto elapsed = std::chrono::steady_clock::now() - start;
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.dispatched);
  EXPECT_NE(result.error.find("timed out"), std::string::npos);
  EXPECT_LT(elapsed, std::chrono::seconds(3));
}

TEST(CuinterposeCoordinatorTest, DispatchesCustomStoragePrepareMode) {
  TemporaryDirectory temporary;
  const fs::path script = WriteScript(
      temporary.path(), "[ \"$1\" = \"--prepare-custom-storage\" ]\n");
  CuinterposeCoordinator coordinator(script, temporary.path(),
                                     std::chrono::seconds(1));
  std::atomic<bool> shutting_down{false};
  const auto result = coordinator.Prepare(
      {{.host_pid = 1, .namespace_pid = 1}}, temporary.path(), shutting_down,
      true);
  EXPECT_TRUE(result.succeeded) << result.error;
  EXPECT_TRUE(result.dispatched);
}

TEST(CuinterposeCoordinatorTest, BoundsOutputDrainAfterCoordinatorExit) {
  TemporaryDirectory temporary;
  const fs::path script =
      WriteScript(temporary.path(), "sleep 10 &\nexit 0\n");
  CuinterposeCoordinator coordinator(script, temporary.path(),
                                     std::chrono::seconds(1));
  std::atomic<bool> shutting_down{false};
  const auto start = std::chrono::steady_clock::now();
  const auto result = coordinator.Prepare(
      {{.host_pid = 1, .namespace_pid = 1}}, temporary.path(), shutting_down,
      false);
  const auto elapsed = std::chrono::steady_clock::now() - start;
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.dispatched);
  EXPECT_NE(result.error.find("timed out"), std::string::npos);
  EXPECT_LT(elapsed, std::chrono::seconds(3));
}

TEST(CuinterposeCoordinatorTest, CancelsCoordinatorDuringShutdown) {
  TemporaryDirectory temporary;
  const fs::path script = WriteScript(temporary.path(), "sleep 10\n");
  CuinterposeCoordinator coordinator(script, temporary.path(),
                                     std::chrono::seconds(30));
  std::atomic<bool> shutting_down{false};
  std::thread cancel([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(100));
    shutting_down.store(true, std::memory_order_release);
  });
  const auto result = coordinator.Prepare(
      {{.host_pid = 1, .namespace_pid = 1}}, temporary.path(), shutting_down,
      false);
  cancel.join();
  EXPECT_FALSE(result.succeeded);
  EXPECT_TRUE(result.dispatched);
  EXPECT_NE(result.error.find("cancelled"), std::string::npos);
}

} // namespace
