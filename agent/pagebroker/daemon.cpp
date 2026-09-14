// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon.hpp"

#include <arpa/inet.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/un.h>

#include <algorithm>
#include <chrono>
#include <cstring>
#include <filesystem>
#include <future>
#include <iostream>
#include <string>
#include <string_view>
#include <syncstream>
#include <system_error>
#include <thread>
#include <utility>
#include <vector>

#include "broker.hpp"
#include "cuda_engine.hpp"
#include "daemon_connection.hpp"
#include "file_descriptor.hpp"

namespace fs = std::filesystem;
using snapshot::pagebroker::Broker;
using snapshot::pagebroker::Failure;
using snapshot::pagebroker::Request;
using snapshot::pagebroker::Response;

namespace {
constexpr uint32_t kMaxMessageSize = 64 << 10;  // 64 KB
constexpr timeval kConnectionTimeout{30, 0};
constexpr rlim_t kRequiredFileDescriptors = 4096;
constexpr int kShutdownPollTimeoutMs = 1000;
constexpr auto kAcceptRetryInitialDelay = std::chrono::milliseconds(10);
constexpr auto kAcceptRetryMaxDelay = std::chrono::milliseconds(1000);
constexpr auto kCudaShutdownRetryDelay = std::chrono::milliseconds(100);
volatile sig_atomic_t shutting_down;

void
Stop(int)
{
  shutting_down = 1;
}

void
LogError(std::string_view operation, const std::error_code& error)
{
  std::cerr << operation << ": " << error.message() << '\n';
}

ExitCode
Fail(std::string_view operation, const std::error_code& error)
{
  LogError(operation, error);
  return ExitCode::FAILURE;
}

bool
RaiseFileDescriptorLimit()
{
  rlimit limit {};
  if (getrlimit(RLIMIT_NOFILE, &limit) < 0) {
    LogError("get file descriptor limit", {errno, std::generic_category()});
    return false;
  }
  if (limit.rlim_cur >= kRequiredFileDescriptors)
    return true;
  if (limit.rlim_max < kRequiredFileDescriptors) {
    std::cerr << "file descriptor hard limit must be at least " << kRequiredFileDescriptors << '\n';
    return false;
  }
  limit.rlim_cur = kRequiredFileDescriptors;
  if (setrlimit(RLIMIT_NOFILE, &limit) < 0) {
    LogError("set file descriptor limit", {errno, std::generic_category()});
    return false;
  }
  return true;
}

bool
IsResourceExhaustion(int error)
{
  return error == EMFILE || error == ENFILE || error == ENOBUFS || error == ENOMEM;
}

std::error_code
InstallSignalHandler(int signal)
{
  struct sigaction action {};
  action.sa_handler = Stop;
  sigemptyset(&action.sa_mask);
  if (sigaction(signal, &action, nullptr) < 0)
    return {errno, std::generic_category()};
  return {};
}

std::error_code
InstallSignalHandlers()
{
  if (const auto error = InstallSignalHandler(SIGINT); error)
    return error;
  return InstallSignalHandler(SIGTERM);
}

std::error_code
PrepareDirectories(const fs::path& socket_path, const fs::path& staging_directory)
{
  std::error_code error;
  fs::remove(DaemonReadinessPath(socket_path), error);
  if (error)
    return error;
  fs::create_directories(socket_path.parent_path(), error);
  if (error)
    return error;
  fs::create_directories(staging_directory, error);
  return error;
}

class ReadinessWithdrawal final {
 public:
  explicit ReadinessWithdrawal(fs::path socket_path)
      : socket_path_(std::move(socket_path)) {}
  ReadinessWithdrawal(const ReadinessWithdrawal&) = delete;
  ReadinessWithdrawal& operator=(const ReadinessWithdrawal&) = delete;
  ~ReadinessWithdrawal() { WithdrawDaemonReadiness(socket_path_); }

 private:
  fs::path socket_path_;
};

std::error_code
ConfigureConnection(int connection)
{
  if (setsockopt(connection, SOL_SOCKET, SO_RCVTIMEO, &kConnectionTimeout, sizeof(kConnectionTimeout)) < 0 ||
      setsockopt(connection, SOL_SOCKET, SO_SNDTIMEO, &kConnectionTimeout, sizeof(kConnectionTimeout)) < 0)
    return {errno, std::generic_category()};
  return {};
}

std::error_code
ConfigureListener(int listener)
{
  const int flags = fcntl(listener, F_GETFL);
  if (flags < 0 || fcntl(listener, F_SETFL, flags | O_NONBLOCK) < 0)
    return {errno, std::generic_category()};
  return {};
}

std::pair<FileDescriptor, std::error_code>
CreateListener(const fs::path& socket_path)
{
  std::error_code error;
  fs::remove(socket_path, error);
  if (error)
    return std::make_pair(FileDescriptor(-1), error);

  FileDescriptor listener(socket(AF_UNIX, SOCK_STREAM, 0));
  if (listener.get() < 0)
    return std::make_pair(std::move(listener), std::error_code(errno, std::generic_category()));
  sockaddr_un address{};
  address.sun_family = AF_UNIX;
  std::strcpy(address.sun_path, socket_path.c_str());
  if (bind(listener.get(), reinterpret_cast<const sockaddr*>(&address), sizeof(address)) < 0 ||
      listen(listener.get(), SOMAXCONN) < 0)
    return std::make_pair(std::move(listener), std::error_code(errno, std::generic_category()));
  if (error = ConfigureListener(listener.get()); error)
    return std::make_pair(std::move(listener), error);
  return std::make_pair(std::move(listener), std::error_code{});
}

bool
ReadAll(int fd, void* buffer, size_t size)
{
  auto* bytes = static_cast<char*>(buffer);
  while (size > 0) {
    ssize_t read;
    do {
      read = recv(fd, bytes, size, 0);
    } while (read < 0 && errno == EINTR);
    if (read <= 0)
      return false;
    bytes += read;
    size -= read;
  }
  return true;
}

bool
WriteAll(int fd, const void* buffer, size_t size)
{
  const auto* bytes = static_cast<const char*>(buffer);
  while (size > 0) {
    ssize_t written;
    do {
      written = send(fd, bytes, size, MSG_NOSIGNAL);
    } while (written < 0 && errno == EINTR);
    if (written <= 0)
      return false;
    bytes += written;
    size -= written;
  }
  return true;
}

Response
InvalidRequest()
{
  Response response;
  response.set_request_id("");
  response.set_transaction_id("");
  response.mutable_failure()->set_code(Failure::INVALID_REQUEST);
  response.mutable_failure()->set_message("invalid request");
  return response;
}

}  // namespace

const char*
RequestCommandName(Request::CommandCase command)
{
  switch (command) {
    case Request::kStagedRestore:
      return "staged_restore";
    case Request::kDirectRestore:
      return "direct_restore";
    case Request::kReferenceRegularRestore:
      return "reference_regular_restore";
    case Request::kPrepareStagedCheckpoint:
      return "prepare_staged_checkpoint";
    case Request::kCommit:
      return "commit";
    case Request::kAbort:
      return "abort";
    case Request::kCudaCheckpoint:
      return "cuda_checkpoint";
    case Request::kCudaRestore:
      return "cuda_restore";
    case Request::kBeginRestore:
      return "begin_restore";
    case Request::kActivateRestore:
      return "activate_restore";
    case Request::kBeginCheckpoint:
      return "begin_checkpoint";
    default:
      return "invalid";
  }
}

const char*
ResponseResultName(Request::CommandCase command, const Response& response)
{
  switch (response.result_case()) {
    case Response::kStagedRestoreDirectory:
      if (command == Request::kDirectRestore)
        return "direct_restore_staged";
      if (command == Request::kReferenceRegularRestore)
        return "regular_restore_referenced";
      return "staged_restore";
    case Response::kStagedCheckpointDirectory:
      return "staged_checkpoint";
    case Response::kDirectRestoreReady:
      return "direct_restore_ready";
    case Response::kCommitComplete:
      return "committed";
    case Response::kAbortComplete:
      return "aborted";
    case Response::kFailure:
      return "failed";
    case Response::kCudaOperationComplete:
      return "cuda_complete";
    case Response::kRestoreAdmissionGranted:
      return "restore_admitted";
    case Response::kCheckpointAdmissionGranted:
      return "checkpoint_admitted";
    case Response::kRestoreActivationGranted:
      return "restore_activated";
    default:
      return "invalid";
  }
}

namespace {

void
HandleConnection(int connection, Broker& broker)
{
  uint32_t size = 0;
  if (!ReadAll(connection, &size, sizeof(size)))
    return;
  size = ntohl(size);

  Response response;
  if (size > kMaxMessageSize) {
    response = InvalidRequest();
  } else {
    std::string message(size, '\0');
    Request request;
    if (!ReadAll(connection, message.data(), size) || !request.ParseFromString(message) || !request.IsInitialized()) {
      response = InvalidRequest();
    } else {
      const auto request_start = std::chrono::steady_clock::now();
      response = broker.HandleRequest(request);
      const auto duration =
          std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - request_start);
      std::osyncstream(std::cerr) << "transaction=" << request.transaction_id()
                                  << " command=" << RequestCommandName(request.command_case())
                                  << " result=" << ResponseResultName(request.command_case(), response)
                                  << " duration_ms=" << duration.count()
                                  << (response.has_failure() ? " error=" + response.failure().message() : "") << '\n';
    }
  }

  std::string message = response.SerializeAsString();
  size = htonl(message.size());
  WriteAll(connection, &size, sizeof(size));
  WriteAll(connection, message.data(), message.size());
}

void
ServeConnection(int connection, Broker& broker)
{
  FileDescriptor descriptor(connection);
  if (const auto error = ConfigureConnection(descriptor.get()); error) {
    LogError("set connection timeout", error);
    return;
  }
  HandleConnection(descriptor.get(), broker);
}

void
WaitForHandler(std::future<void>& handler)
{
  try {
    handler.get();
  }
  catch (const std::exception& error) {
    std::cerr << "connection handler: " << error.what() << '\n';
  }
  catch (...) {
    std::cerr << "connection handler: unknown exception\n";
  }
}

void
ReapHandlers(std::vector<std::future<void>>& handlers)
{
  for (auto handler = handlers.begin(); handler != handlers.end();) {
    if (handler->wait_for(std::chrono::seconds(0)) != std::future_status::ready) {
      ++handler;
      continue;
    }
    WaitForHandler(*handler);
    handler = handlers.erase(handler);
  }
}

void
WaitForHandlers(std::vector<std::future<void>>& handlers)
{
  for (auto& handler : handlers) WaitForHandler(handler);
}

}  // namespace

fs::path
DaemonReadinessPath(const fs::path& socket_path)
{
  return socket_path.string() + ".ready";
}

bool
PublishDaemonReadiness(const fs::path& socket_path, std::error_code* error)
{
  const fs::path readiness_path = DaemonReadinessPath(socket_path);
  FileDescriptor marker(open(readiness_path.c_str(), O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600));
  if (marker.get() < 0) {
    if (error != nullptr)
      *error = {errno, std::generic_category()};
    return false;
  }
  if (error != nullptr)
    error->clear();
  return true;
}

void
WithdrawDaemonReadiness(const fs::path& socket_path)
{
  std::error_code ignored;
  fs::remove(DaemonReadinessPath(socket_path), ignored);
}

ExitCode
ProbeDaemonReady(const fs::path& socket_path)
{
  if (socket_path.string().size() >= sizeof(sockaddr_un::sun_path))
    return ExitCode::INVALID_ARGUMENTS;
  struct stat marker {};
  if (lstat(DaemonReadinessPath(socket_path).c_str(), &marker) != 0 ||
      !S_ISREG(marker.st_mode))
    return ExitCode::FAILURE;

  FileDescriptor connection(socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0));
  if (connection.get() < 0)
    return ExitCode::FAILURE;
  sockaddr_un address {};
  address.sun_family = AF_UNIX;
  std::strcpy(address.sun_path, socket_path.c_str());
  if (connect(connection.get(), reinterpret_cast<sockaddr*>(&address),
              sizeof(address)) != 0)
    return ExitCode::FAILURE;
  // Closing without a request is intentionally silent in ServeConnection and
  // cannot mutate broker state or trigger CUDA fail-stop.
  return ExitCode::SUCCESS;
}

void
ShutdownAndWaitForHandlers(Broker& broker,
                           std::vector<std::future<void>>& handlers,
                           const fs::path& readiness_socket_path)
{
  if (!readiness_socket_path.empty())
    WithdrawDaemonReadiness(readiness_socket_path);
  for (;;) {
    std::string shutdown_error;
    if (broker.BeginShutdownCuda(&shutdown_error))
      break;
    std::cerr << "begin PageBroker CUDA shutdown: " << shutdown_error << '\n';
    // BeginShutdownCuda performs bounded identity checks. Retry them before
    // joining handlers: a restore handler blocked in a worker RPC cannot
    // unwind until a conclusive pass safely signals that worker. Persistent
    // identity uncertainty remains fail-closed until kubelet enforces the Pod
    // deadline; PageBroker never exits or signals a context owner on a guess.
    ReapHandlers(handlers);
    std::this_thread::sleep_for(kCudaShutdownRetryDelay);
  }
  WaitForHandlers(handlers);
}

namespace {

void
Serve(FileDescriptor& listener, Broker& broker, size_t max_concurrent_requests,
      const fs::path& readiness_socket_path)
{
  std::vector<std::future<void>> handlers;
  DaemonMaintenanceSchedule maintenance(std::chrono::steady_clock::now());
  auto accept_retry_delay = kAcceptRetryInitialDelay;
  while (!shutting_down && !broker.ShutdownRequested()) {
    ReapHandlers(handlers);
    const auto now = std::chrono::steady_clock::now();
    if (maintenance.TransactionsDue(now)) {
      broker.ReapExpiredTransactions(now);
      std::string stage_reap_error;
      if (!broker.ReapRetainedStageReadyMarkers(
              std::chrono::system_clock::now(), &stage_reap_error))
        std::cerr << "reap retained PageBroker stage markers: "
                  << stage_reap_error << '\n';
    }
    if (maintenance.CudaDue(now)) {
      std::string cuda_reap_error;
      if (!broker.ReapExitedCuda(&cuda_reap_error))
        std::cerr << "reap exited PageBroker CUDA targets: " << cuda_reap_error << '\n';
    }
    pollfd poll_descriptor{listener.get(), POLLIN, 0};
    const int ready = poll(&poll_descriptor, 1, kShutdownPollTimeoutMs);
    if (ready == 0)
      continue;
    if (ready < 0) {
      if (errno != EINTR)
        LogError("poll", {errno, std::generic_category()});
      continue;
    }
    const int connection = accept(listener.get(), nullptr, nullptr);
    if (connection < 0) {
      const int error = errno;
      if (IsResourceExhaustion(error)) {
        LogError("accept", {error, std::generic_category()});
        std::this_thread::sleep_for(accept_retry_delay);
        accept_retry_delay = std::min(accept_retry_delay * 2, kAcceptRetryMaxDelay);
      } else if (error != EINTR && error != EAGAIN && error != EWOULDBLOCK) {
        LogError("accept", {error, std::generic_category()});
      }
      continue;
    }
    accept_retry_delay = kAcceptRetryInitialDelay;
    if (handlers.size() == max_concurrent_requests) {
      FileDescriptor descriptor(connection);
      std::cerr << "connection limit reached\n";
      if (!snapshot::pagebroker::HandleConnectionLimit(descriptor.get()))
        std::cerr << "connection limit response timed out\n";
      continue;
    }
    try {
      handlers.emplace_back(
          std::async(std::launch::async, [connection, &broker] { ServeConnection(connection, broker); }));
    }
    catch (const std::exception& error) {
      FileDescriptor descriptor(connection);
      std::cerr << "start connection: " << error.what() << '\n';
    }
  }
  ShutdownAndWaitForHandlers(broker, handlers, readiness_socket_path);
}
}  // namespace

ExitCode
RunDaemon(
    const fs::path& socket_path,
    const fs::path& staging_directory,
    const fs::path& storage_root,
    size_t max_concurrent_requests,
    uintmax_t max_staging_bytes,
    bool cuda,
    bool cuda_custom_storage,
    size_t max_concurrent_cuda_restores,
    uint64_t cuda_operation_timeout_seconds,
    size_t cuda_worker_count,
    size_t cuda_custom_storage_worker_count)
{
  shutting_down = 0;
  if (!RaiseFileDescriptorLimit())
    return ExitCode::FAILURE;
  if (const auto error = InstallSignalHandlers(); error)
    return Fail("install signal handlers", error);
  if (const auto error = PrepareDirectories(socket_path, staging_directory); error)
    return Fail("create daemon directories", error);
  std::error_code staging_path_error;
  const fs::path canonical_staging_directory =
      fs::canonical(staging_directory, staging_path_error);
  if (staging_path_error)
    return Fail("resolve staging directory", staging_path_error);
  if (socket_path.string().size() >= sizeof(sockaddr_un::sun_path)) {
    std::cerr << "socket path is too long\n";
    return ExitCode::INVALID_ARGUMENTS;
  }
  auto [listener, error] = CreateListener(socket_path);
  if (error)
    return Fail("create listener", error);

  std::unique_ptr<snapshot::pagebroker::CudaEngine> cuda_engine;
  if (cuda || cuda_custom_storage) {
    try {
      cuda_engine = snapshot::pagebroker::CreateCudaEngine(
          std::chrono::seconds(cuda_operation_timeout_seconds),
          canonical_staging_directory,
          cuda,
          cuda_custom_storage,
          max_concurrent_cuda_restores,
          cuda_worker_count,
          cuda_custom_storage_worker_count);
    }
    catch (const std::exception& error) {
      std::cerr << error.what() << '\n';
      return ExitCode::FAILURE;
    }
  }
  Broker broker(canonical_staging_directory, storage_root, max_staging_bytes,
                std::move(cuda_engine));
  ReadinessWithdrawal readiness_withdrawal(socket_path);
  std::error_code readiness_error;
  if (!PublishDaemonReadiness(socket_path, &readiness_error))
    return Fail("publish daemon readiness", readiness_error);
  Serve(listener, broker, max_concurrent_requests, socket_path);
  std::string shutdown_error;
  if (!broker.ShutdownCuda(&shutdown_error)) {
    std::cerr << shutdown_error << '\n';
    return ExitCode::FAILURE;
  }
  return ExitCode::SUCCESS;
}
