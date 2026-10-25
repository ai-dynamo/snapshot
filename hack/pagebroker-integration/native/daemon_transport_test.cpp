// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <array>
#include <algorithm>
#include <cstdlib>
#include <iterator>
#include <cstring>
#include <fcntl.h>
#include <arpa/inet.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <sys/resource.h>
#include <poll.h>
#include <span>
#include <csignal>
#include <chrono>
#include <condition_variable>
#include <mutex>

#include <fstream>
#include <iterator>
#include <memory>
#include <optional>
#include <string>
#include <thread>
#include <sys/stat.h>
#include <unistd.h>

#include "broker.hpp"
#include "daemon.hpp"
#include "gpu/storage_manifest.hpp"
#include "tests/request_builder.hpp"
#include "tests/temporary_directory.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {
int restore_started_fd = -1;
}

// GNU ld wraps the C++ filesystem staging symbol only in the test executable.
void RealStageRestore(const CheckpointInventory&, const fs::path&, Cancellation)
    asm("__real__ZN8snapshot10pagebroker18filesystem_storage12StageRestoreERKNS0_19CheckpointInventoryE"
        "RKNSt10filesystem7__cxx114pathENS0_12CancellationE");
void WrappedStageRestore(const CheckpointInventory&, const fs::path&, Cancellation)
    asm("__wrap__ZN8snapshot10pagebroker18filesystem_storage12StageRestoreERKNS0_19CheckpointInventoryE"
        "RKNSt10filesystem7__cxx114pathENS0_12CancellationE");

void WrappedStageRestore(const CheckpointInventory& inventory, const fs::path& destination, Cancellation cancellation)
{
  RealStageRestore(inventory, destination, cancellation);
  if (restore_started_fd < 0)
    return;
  // The real copy is complete, but the request still owns staging and is active.
  // Only transaction cancellation releases it; closing the listener is insufficient.
  const char ready = 'R';
  ssize_t sent;
  do {
    sent = write(restore_started_fd, &ready, 1);
  } while (sent < 0 && errno == EINTR);
  if (sent != 1)
    throw std::runtime_error("signal active restore to shutdown test");
  std::mutex mutex;
  std::unique_lock lock(mutex);
  std::condition_variable_any changed;
  changed.wait_for(lock, cancellation.Token(), std::chrono::seconds{10}, [] { return false; });
  cancellation.ThrowIfCancelled();
  throw std::runtime_error("shutdown test timed out waiting for transfer cancellation");
}

namespace {
using test::Configure;

// Exercise the shipped listener and framing with real SCM_RIGHTS descriptors.
class DaemonTransportTest : public ::testing::Test, public test::RequestBuilder {
 protected:
  void SetUp() override
  {
    root_ = temporary_.path();
    fs::create_directories(root_ / "storage");
    socket_path_ = root_ / "control.sock";
    server_ = fork();
    ASSERT_GE(server_, 0);
    if (server_ == 0) {
      ConfigureChild();
      DaemonOptions options;
      options.socket_path = socket_path_;
      options.staging_directory = root_ / "staging";
      options.storage_root = root_ / "storage";
      options.enable_gpu = false;
      std::_Exit(static_cast<int>(RunDaemon(options)));
    }
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
    while (!fs::exists(socket_path_) && std::chrono::steady_clock::now() < deadline) {
      std::this_thread::sleep_for(std::chrono::milliseconds{10});
    }
    ASSERT_TRUE(fs::exists(socket_path_));
  }

  void TearDown() override
  {
    if (server_ > 0) {
      kill(server_, SIGTERM);
      while (waitpid(server_, nullptr, 0) < 0 && errno == EINTR) {}
    }
    // The temporary directory is removed after the daemon has exited.
  }

  virtual void ConfigureChild() {}

  FileDescriptor Connect()
  {
    FileDescriptor connection(socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0));
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    if (socket_path_.string().size() >= sizeof(address.sun_path)) {
      throw std::runtime_error("test socket path is too long");
    }
    std::strcpy(address.sun_path, socket_path_.c_str());
    if (connect(connection.get(), reinterpret_cast<sockaddr*>(&address), sizeof(address))) {
      throw std::system_error(errno, std::generic_category(), "connect test daemon");
    }
    constexpr timeval timeout{5, 0};
    if (setsockopt(connection.get(), SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout))) {
      throw std::runtime_error("set test socket timeout");
    }
    return connection;
  }

  void Send(int connection, const void* bytes, size_t size, std::span<const int> files = {})
  {
    iovec data{const_cast<void*>(bytes), size};
    alignas(cmsghdr) std::array<char, CMSG_SPACE(253 * sizeof(int))> control{};
    msghdr header{};
    header.msg_iov = &data;
    header.msg_iovlen = 1;
    if (!files.empty()) {
      if (files.size() > 253) {
        throw std::runtime_error("too many test descriptors");
      }
      header.msg_control = control.data();
      header.msg_controllen = CMSG_SPACE(files.size() * sizeof(int));
      auto* rights = CMSG_FIRSTHDR(&header);
      rights->cmsg_level = SOL_SOCKET;
      rights->cmsg_type = SCM_RIGHTS;
      rights->cmsg_len = CMSG_LEN(files.size() * sizeof(int));
      std::copy(files.begin(), files.end(), reinterpret_cast<int*>(CMSG_DATA(rights)));
    }
    if (sendmsg(connection, &header, MSG_NOSIGNAL) != static_cast<ssize_t>(size)) {
      throw std::runtime_error("send test request");
    }
  }

  Response Receive(int connection, FileDescriptor* broker_process = nullptr)
  {
    uint32_t size = 0;
    alignas(cmsghdr) std::array<char, CMSG_SPACE(sizeof(int))> control{};
    iovec data{&size, sizeof(size)};
    msghdr header{};
    header.msg_iov = &data;
    header.msg_iovlen = 1;
    header.msg_control = control.data();
    header.msg_controllen = control.size();
    const auto received = recvmsg(connection, &header, MSG_CMSG_CLOEXEC);
    if (received <= 0) {
      throw std::runtime_error("read test response size");
    }
    if (auto* rights = CMSG_FIRSTHDR(&header)) {
      if (rights->cmsg_level != SOL_SOCKET || rights->cmsg_type != SCM_RIGHTS ||
          rights->cmsg_len != CMSG_LEN(sizeof(int))) {
        throw std::runtime_error("invalid response descriptor");
      }
      int descriptor;
      std::memcpy(&descriptor, CMSG_DATA(rights), sizeof(descriptor));
      FileDescriptor process(descriptor);
      if (!broker_process) {
        throw std::runtime_error("unexpected response descriptor");
      }
      *broker_process = std::move(process);
    }
    if (header.msg_flags & MSG_CTRUNC ||
        (received < static_cast<ssize_t>(sizeof(size)) &&
         recv(connection, reinterpret_cast<char*>(&size) + received, sizeof(size) - received, MSG_WAITALL) !=
             static_cast<ssize_t>(sizeof(size)) - received)) {
      throw std::runtime_error("truncated response header");
    }
    std::string body(ntohl(size), '\0');
    Response response;
    if (recv(connection, body.data(), body.size(), MSG_WAITALL) != static_cast<ssize_t>(body.size()) ||
        !response.ParseFromString(body)) {
      throw std::runtime_error("read test response");
    }
    char extra;
    if (recv(connection, &extra, 1, 0) != 0) {
      throw std::runtime_error("test connection did not close after response");
    }
    return response;
  }

  Response Exchange(const Request& request, std::initializer_list<int> files = {})
  {
    auto connection = Connect();
    const auto message = request.SerializeAsString();
    const uint32_t size = htonl(message.size());
    std::vector<int> descriptors(files);
    FileDescriptor broker_process(static_cast<int>(syscall(SYS_pidfd_open, server_, 0)));
    if (request.has_checkpoint_gpu() || request.has_restore_gpu()) {
      descriptors.insert(descriptors.begin(), broker_process.get());
    }
    Send(connection.get(), &size, sizeof(size), descriptors);
    Send(connection.get(), message.data(), message.size());
    return Receive(connection.get());
  }

  auto OpenFiles() const
  {
    return std::distance(fs::directory_iterator("/proc/" + std::to_string(server_) + "/fd"),
                         fs::directory_iterator());
  }

  // The broker holds its own pidfd from startup, so wait for a received one.
  bool WaitForPidfd() const
  {
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
    do {
      int pidfds = 0;
      for (const auto& entry : fs::directory_iterator("/proc/" + std::to_string(server_) + "/fd")) {
        std::error_code error;
        if (fs::read_symlink(entry, error) == "anon_inode:[pidfd]" && ++pidfds > 1) {
          return true;
        }
      }
      std::this_thread::yield();
    } while (std::chrono::steady_clock::now() < deadline);
    return false;
  }

  test::TemporaryDirectory temporary_;
  pid_t server_ = -1;
  fs::path root_;
  fs::path socket_path_;
};

class DaemonActiveTransferTest : public DaemonTransportTest {
 protected:
  void SetUp() override
  {
    int ready[2];
    ASSERT_EQ(pipe2(ready, O_CLOEXEC), 0);
    ready_read_ = FileDescriptor(ready[0]);
    ready_write_ = FileDescriptor(ready[1]);
    ASSERT_NO_FATAL_FAILURE(DaemonTransportTest::SetUp());
    ready_write_ = FileDescriptor(-1);
  }

  void ConfigureChild() override
  {
    ready_read_ = FileDescriptor(-1);
    restore_started_fd = ready_write_.get();
  }

  void TearDown() override
  {
    // Successful tests reap explicitly. Failed assertions must not leave a child
    // waiting for transfer cancellation or block the ordinary fixture teardown.
    if (server_ > 0) {
      kill(server_, SIGKILL);
      while (waitpid(server_, nullptr, 0) < 0 && errno == EINTR) {}
      server_ = -1;
    }
  }

  FileDescriptor ready_read_{-1};
  FileDescriptor ready_write_{-1};
};

TEST_F(DaemonActiveTransferTest, SigtermCancelsActiveRestoreBeforeWaitingForHandlers)
{
  const auto source = root_ / "storage/source";
  fs::create_directory(source);
  std::ofstream(source / "image") << "capture";
  auto request = RequestFor("active");
  Configure(request.mutable_staged_restore()->mutable_source(), request.mutable_staged_restore()->mutable_io_engine(), source);
  auto connection = Connect();
  const auto message = request.SerializeAsString();
  const uint32_t size = htonl(message.size());
  Send(connection.get(), &size, sizeof(size));
  Send(connection.get(), message.data(), message.size());

  pollfd ready{ready_read_.get(), POLLIN, 0};
  ASSERT_EQ(poll(&ready, 1, 5000), 1) << "restore did not reach the staging boundary";
  char signal = 0;
  ASSERT_EQ(read(ready_read_.get(), &signal, 1), 1);
  ASSERT_EQ(signal, 'R');
  const auto staging = root_ / "staging/restore/active";
  ASSERT_TRUE(fs::exists(staging / "image"));
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, server_, 0)));
  ASSERT_GE(process.get(), 0);
  ASSERT_EQ(kill(server_, SIGTERM), 0);

  // Receive has a five-second socket timeout; process exit is independently bounded.
  const auto response = Receive(connection.get());
  ASSERT_TRUE(response.has_failure()) << response.DebugString();
  EXPECT_EQ(response.failure().code(), Failure::INTERNAL_ERROR);
  EXPECT_EQ(response.failure().message(), "operation cancelled");
  pollfd exited{process.get(), POLLIN, 0};
  ASSERT_EQ(poll(&exited, 1, 5000), 1) << "daemon did not exit after cancellation";
  int status = 0;
  ASSERT_EQ(waitpid(server_, &status, WNOHANG), server_);
  server_ = -1;
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), static_cast<int>(ExitCode::SUCCESS));
  EXPECT_FALSE(fs::exists(staging));
  std::string contents;
  std::ifstream(source / "image") >> contents;
  EXPECT_EQ(contents, "capture");
}

TEST_F(DaemonTransportTest, PreparationPidfdIsOptInAndRejectsOtherProcess)
{
  auto ordinary = RequestFor("ordinary");
  Configure(ordinary.mutable_prepare_direct_checkpoint()->mutable_destination(),
            ordinary.mutable_prepare_direct_checkpoint()->mutable_io_engine(), root_ / "storage" / "ordinary");
  EXPECT_TRUE(Exchange(ordinary).has_direct_checkpoint_directory());

  auto preparation = RequestFor("prepared");
  preparation.set_request_broker_pidfd(true);
  Configure(preparation.mutable_prepare_direct_checkpoint()->mutable_destination(),
            preparation.mutable_prepare_direct_checkpoint()->mutable_io_engine(), root_ / "storage" / "checkpoint");
  auto connection = Connect();
  const auto message = preparation.SerializeAsString();
  const uint32_t size = htonl(message.size());
  Send(connection.get(), &size, sizeof(size));
  Send(connection.get(), message.data(), message.size());
  FileDescriptor broker_process(-1);
  ASSERT_TRUE(Receive(connection.get(), &broker_process).has_direct_checkpoint_directory());
  ASSERT_GE(broker_process.get(), 0);
  EXPECT_NE(fcntl(broker_process.get(), F_GETFD) & FD_CLOEXEC, 0);
  pollfd original{broker_process.get(), POLLIN, 0};
  EXPECT_EQ(poll(&original, 1, 0), 0);

  auto abort = RequestFor("prepared");
  abort.mutable_abort();
  FileDescriptor other(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  auto rejected = Exchange(abort, {other.get()});
  ASSERT_TRUE(rejected.has_failure());
  EXPECT_EQ(rejected.failure().message(), "broker process descriptor does not match");
  EXPECT_TRUE(Exchange(abort, {broker_process.get()}).has_abort_complete());

  ASSERT_EQ(kill(server_, SIGKILL), 0);
  while (waitpid(server_, nullptr, 0) < 0 && errno == EINTR) {}
  server_ = -1;
  EXPECT_EQ(poll(&original, 1, 0), 1);
  EXPECT_NE(original.revents & POLLIN, 0);
}

TEST_F(DaemonTransportTest, BindsGpuDescriptorsAndPreservesControlRequests)
{
  auto ready = RequestFor("ready");
  ready.mutable_capabilities();
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  const auto initial_files = OpenFiles();
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  FileDescriptor ordinary(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(process.get(), 0);
  ASSERT_GE(ordinary.get(), 0);
  auto request = RequestFor("gpu");
  auto* checkpoint = request.mutable_checkpoint_gpu();
  checkpoint->mutable_context()->add_captured_pids(1);
  checkpoint->mutable_context()->add_visible_devices("GPU-11111111-1111-1111-1111-111111111111");
  auto* target = checkpoint->add_targets();
  target->set_captured_pid(1);
  target->set_target_pid(getpid());

  const auto valid = Exchange(request, {process.get()});
  ASSERT_TRUE(valid.has_failure()) << valid.DebugString();
  // Descriptor validation succeeded. This CPU test deliberately disables CUDA.
  EXPECT_EQ(valid.failure().message(), "CustomStorage is unavailable");
  for (const auto& invalid : {Exchange(request), Exchange(request, {process.get(), process.get()}),
                              Exchange(request, {ordinary.get()})}) {
    EXPECT_EQ(invalid.failure().code(), Failure::INVALID_REQUEST) << invalid.DebugString();
    EXPECT_NE(invalid.failure().message(), valid.failure().message());
  }
  target->set_target_pid(std::max(getpid(), server_) + 1);
  const auto mismatched = Exchange(request, {process.get()});
  EXPECT_EQ(mismatched.failure().message(), "GPU target descriptor does not match host PID");

  const auto child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    pause();
    std::_Exit(0);
  }
  FileDescriptor exited(static_cast<int>(syscall(SYS_pidfd_open, child, 0)));
  kill(child, SIGKILL);
  while (waitpid(child, nullptr, 0) < 0 && errno == EINTR) {}
  ASSERT_GE(exited.get(), 0);
  target->set_target_pid(child);
  const auto dead = Exchange(request, {exited.get()});
  EXPECT_EQ(dead.failure().code(), Failure::INVALID_REQUEST) << dead.DebugString();
  EXPECT_NE(dead.failure().message(), valid.failure().message());

  auto capabilities = RequestFor("control");
  capabilities.mutable_capabilities();
  const auto wrong_control = Exchange(capabilities, {process.get()});
  EXPECT_EQ(wrong_control.failure().code(), Failure::INVALID_REQUEST);
  const auto control = Exchange(capabilities);
  EXPECT_TRUE(control.has_capabilities()) << control.DebugString();
  EXPECT_EQ(OpenFiles(), initial_files) << "request descriptors leaked";
}

TEST_F(DaemonTransportTest, HandlesFragmentedHeadersAndRejectsLateOrTruncatedRights)
{
  auto ready = RequestFor("ready");
  ready.mutable_capabilities();
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  const auto initial_files = OpenFiles();
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  ASSERT_GE(process.get(), 0);
  FileDescriptor broker_process(static_cast<int>(syscall(SYS_pidfd_open, server_, 0)));
  const std::array<int, 2> files{broker_process.get(), process.get()};
  auto request = RequestFor("fragmented");
  auto* checkpoint = request.mutable_checkpoint_gpu();
  checkpoint->mutable_context()->add_captured_pids(1);
  checkpoint->mutable_context()->add_visible_devices("GPU-11111111-1111-1111-1111-111111111111");
  auto* target = checkpoint->add_targets();
  target->set_captured_pid(1);
  target->set_target_pid(getpid());
  const auto message = request.SerializeAsString();
  const uint32_t size = htonl(message.size());
  for (const auto& late : {std::string("none"), std::string("header"), std::string("body")}) {
    SCOPED_TRACE(late);
    auto connection = Connect();
    Send(connection.get(), &size, 1, files);
    // Descriptor adoption proves that the first recvmsg consumed exactly this
    // fragment before sending the rest. No timing assumption or syscall stub.
    ASSERT_TRUE(WaitForPidfd());
    Send(connection.get(), reinterpret_cast<const char*>(&size) + 1, sizeof(size) - 1,
         late == "header" ? std::span<const int>(files) : std::span<const int>{});
    if (late == "header") {
      char byte;
      EXPECT_EQ(recv(connection.get(), &byte, 1, 0), 0);
    } else {
      Send(connection.get(), message.data(), message.size(),
           late == "body" ? std::span<const int>(files) : std::span<const int>{});
      const auto response = Receive(connection.get());
      ASSERT_TRUE(response.has_failure()) << response.DebugString();
      EXPECT_EQ(response.failure().message(), late == "none" ? "CustomStorage is unavailable" : "invalid request");
    }
    ASSERT_TRUE(Exchange(ready).has_capabilities());
    EXPECT_EQ(OpenFiles(), initial_files);
  }

  // The kernel's SCM_RIGHTS send limit equals the receiver's buffer capacity.
  // Lower the running child's descriptor limit to induce real MSG_CTRUNC.
  rlimit original{};
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, nullptr, &original), 0);
  rlimit limited{static_cast<rlim_t>(initial_files + 8), original.rlim_max};
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, &limited, nullptr), 0);
  auto connection = Connect();
  const std::array<int, 32> many = [&] {
    std::array<int, 32> result;
    result.fill(process.get());
    return result;
  }();
  Send(connection.get(), &size, sizeof(size), many);
  const auto truncated = Receive(connection.get());
  EXPECT_EQ(truncated.failure().code(), Failure::INVALID_REQUEST);
  EXPECT_EQ(truncated.failure().message(), "invalid request descriptors");
  ASSERT_EQ(prlimit(server_, RLIMIT_NOFILE, &original, nullptr), 0);
  ASSERT_TRUE(Exchange(ready).has_capabilities());
  EXPECT_EQ(OpenFiles(), initial_files);
}

TEST_F(DaemonTransportTest, RejectsGpuAdmissionFromAControlHandlerDuringShutdown)
{
  FileDescriptor process(static_cast<int>(syscall(SYS_pidfd_open, getpid(), 0)));
  ASSERT_GE(process.get(), 0);
  auto request = RequestFor("shutdown");
  request.mutable_checkpoint_gpu();
  const auto message = request.SerializeAsString();
  const uint32_t size = htonl(message.size());
  auto connection = Connect();
  FileDescriptor broker_process(static_cast<int>(syscall(SYS_pidfd_open, server_, 0)));
  const std::array<int, 2> files{broker_process.get(), process.get()};
  Send(connection.get(), &size, 1, files);
  ASSERT_TRUE(WaitForPidfd());
  ASSERT_EQ(kill(server_, SIGTERM), 0);
  // Closing the listener is observable before the blocked control handler
  // finishes its frame. The handler must not admit a new GPU operation now.
  bool stopped = false;
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{5};
  while (std::chrono::steady_clock::now() < deadline) {
    try {
      auto probe = Connect();
    } catch (const std::system_error& error) {
      stopped = error.code().value() == ECONNREFUSED;
      break;
    }
    std::this_thread::yield();
  }
  EXPECT_TRUE(stopped);
  Send(connection.get(), reinterpret_cast<const char*>(&size) + 1, sizeof(size) - 1);
  Send(connection.get(), message.data(), message.size());
  const auto response = Receive(connection.get());
  EXPECT_EQ(response.failure().code(), Failure::UNAVAILABLE);
  EXPECT_EQ(response.failure().message(), "PageBroker is shutting down");
}

}  // namespace
