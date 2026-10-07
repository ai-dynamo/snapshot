// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon.hpp"
#include "fatal_cleanup.hpp"

#include <arpa/inet.h>
#include <sys/resource.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>

#include <algorithm>
#include <array>
#include <atomic>
#include <mutex>
#include <chrono>
#include <charconv>
#include <optional>
#include <stdexcept>
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
#include "file_descriptor.hpp"

namespace fs = std::filesystem;
using snapshot::pagebroker::Broker;
using snapshot::pagebroker::Cancellation;
using snapshot::pagebroker::CancellationPtr;
using snapshot::pagebroker::Failure;
using snapshot::pagebroker::Request;
using snapshot::pagebroker::Response;

namespace {
constexpr uint32_t kMaxMessageSize = 64 << 10;  // 64 KB
constexpr size_t kMaxPassedFiles = 253;  // Linux SCM_RIGHTS per-message limit.
constexpr timeval kConnectionTimeout{30, 0};
constexpr rlim_t kRequiredFileDescriptors = 4096;
constexpr int kShutdownPollTimeoutMs = 1000;
constexpr auto kAcceptRetryInitialDelay = std::chrono::milliseconds(10);
constexpr auto kAcceptRetryMaxDelay = std::chrono::milliseconds(1000);
constexpr auto kTransactionReapInterval = std::chrono::minutes(2);
// Long GPU operations use a separate budget, leaving control handlers available
// for Abort and Commit. The CLI limit applies only to control handlers.
constexpr size_t kMaxGpuHandlers = 128;
constexpr int kGpuWatchPollTimeoutMs = 100;
static_assert(std::atomic<bool>::is_always_lock_free);
std::atomic<bool> shutting_down{false};

void
Stop(int)
{
  shutting_down.store(true, std::memory_order_relaxed);
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
  fs::create_directories(socket_path.parent_path(), error);
  if (error)
    return error;
  fs::create_directories(staging_directory, error);
  return error;
}

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
    iovec data{bytes, size};
    msghdr message{};
    message.msg_iov = &data;
    message.msg_iovlen = 1;
    ssize_t read;
    do {
      read = recvmsg(fd, &message, MSG_CMSG_CLOEXEC);
    } while (read < 0 && errno == EINTR);
    // Rights belong on the first header read. The kernel closes discarded
    // descriptors when no control buffer is supplied here.
    if (read <= 0 || (message.msg_flags & (MSG_CTRUNC | MSG_TRUNC))) {
      return false;
    }
    bytes += read;
    size -= read;
  }
  return true;
}

bool
ReadHeader(int connection, uint32_t& size, std::vector<FileDescriptor>& descriptors)
{
  // Reserve before recvmsg so adopting installed descriptors cannot allocate.
  descriptors.reserve(kMaxPassedFiles);
  alignas(cmsghdr) std::array<char, CMSG_SPACE(kMaxPassedFiles * sizeof(int))> control{};
  iovec data{&size, sizeof(size)};
  msghdr message{};
  message.msg_iov = &data;
  message.msg_iovlen = 1;
  message.msg_control = control.data();
  message.msg_controllen = control.size();
  ssize_t received;
  do {
    received = recvmsg(connection, &message, MSG_CMSG_CLOEXEC);
  } while (received < 0 && errno == EINTR);
  if (received <= 0) {
    return false;
  }
  bool valid = !(message.msg_flags & (MSG_CTRUNC | MSG_TRUNC));
  for (auto* header = CMSG_FIRSTHDR(&message); header; header = CMSG_NXTHDR(&message, header)) {
    if (header->cmsg_level != SOL_SOCKET || header->cmsg_type != SCM_RIGHTS ||
        header->cmsg_len < CMSG_LEN(0)) {
      valid = false;
      continue;
    }
    const size_t bytes = header->cmsg_len - CMSG_LEN(0);
    valid = valid && bytes % sizeof(int) == 0;
    const auto* files = reinterpret_cast<const int*>(CMSG_DATA(header));
    for (size_t index = 0; index < bytes / sizeof(int); ++index) {
      descriptors.emplace_back(files[index]);
    }
  }
  if (!valid) {
    throw std::invalid_argument("invalid request descriptors");
  }
  return received == static_cast<ssize_t>(sizeof(size)) ||
      ReadAll(connection, reinterpret_cast<char*>(&size) + received, sizeof(size) - received);
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

void
SendResponse(int connection, const Response& response)
{
  const std::string message = response.SerializeAsString();
  const uint32_t size = htonl(message.size());
  if (!WriteAll(connection, &size, sizeof(size)) || !WriteAll(connection, message.data(), message.size())) {
    LogError("send response", {errno, std::generic_category()});
  }
}

Response
RequestFailed(const Request& request, Failure::Code code, const std::string& message)
{
  Response response;
  response.set_request_id(request.request_id());
  response.set_transaction_id(request.transaction_id());
  response.mutable_failure()->set_code(code);
  response.mutable_failure()->set_message(message);
  return response;
}

const char*
CommandName(Request::CommandCase command)
{
  switch (command) {
    case Request::kDirectRestore:
      return "direct_restore";
    case Request::kStagedRestore:
      return "staged_restore";
    case Request::kPrepareStagedCheckpoint:
      return "prepare_staged_checkpoint";
    case Request::kPrepareDirectCheckpoint:
      return "prepare_direct_checkpoint";
    case Request::kCapabilities:
      return "capabilities";
    case Request::kCheckpointGpu:
      return "checkpoint_gpu";
    case Request::kRestoreGpu:
      return "restore_gpu";
    case Request::kCommit:
      return "commit";
    case Request::kAbort:
      return "abort";
    default:
      return "invalid";
  }
}

const char*
ResultName(const Response& response)
{
  switch (response.result_case()) {
    case Response::kDirectRestoreReady:
      return "direct_restore";
    case Response::kStagedRestoreDirectory:
      return "staged_restore";
    case Response::kStagedCheckpointDirectory:
      return "staged_checkpoint";
    case Response::kDirectCheckpointDirectory:
      return "direct_checkpoint";
    case Response::kCapabilities:
      return "capabilities";
    case Response::kGpuCheckpointComplete:
      return "gpu_checkpoint_complete";
    case Response::kGpuRestoreComplete:
      return "gpu_restore_complete";
    case Response::kCommitComplete:
      return "committed";
    case Response::kAbortComplete:
      return "aborted";
    case Response::kFailure:
      return "failed";
    default:
      return "invalid";
  }
}

void ReapHandlers(std::vector<std::future<void>>& handlers);
void WaitForHandlers(std::vector<std::future<void>>& handlers);

void
LogOutcome(const Request& request, const Response& response,
           std::optional<std::chrono::milliseconds> duration = std::nullopt)
{
  std::osyncstream log(std::cerr);
  log << "transaction=" << request.transaction_id()
      << " command=" << CommandName(request.command_case())
      << " result=" << ResultName(response);
  if (duration) {
    log << " duration_ms=" << duration->count();
  }
  if (response.has_failure()) {
    log << " error=" << response.failure().message();
  }
  log << '\n';
}

bool
IsAllowedClient(int connection)
{
  ucred peer{};
  socklen_t peer_size = sizeof(peer);
  return getsockopt(connection, SOL_SOCKET, SO_PEERCRED, &peer, &peer_size) == 0 &&
         peer_size == sizeof(peer) && (peer.uid == 0 || peer.uid == geteuid());
}

class GpuConnections {
 public:
  explicit GpuConnections(Broker& broker) : broker_(broker)
  {
    handlers_.reserve(kMaxGpuHandlers);
  }

  ~GpuConnections()
  {
    Stop();
    WaitForHandlers(handlers_);
  }

  void Stop()
  {
    std::lock_guard lock(mutex_);
    stopping_.store(true, std::memory_order_relaxed);
  }

  std::optional<Response> Start(int connection, const Request& request, std::vector<FileDescriptor> descriptors)
  {
    auto socket = FileDescriptor::Duplicate(connection);
    std::lock_guard lock(mutex_);
    if (stopping_.load(std::memory_order_relaxed) || shutting_down.load(std::memory_order_relaxed)) {
      return RequestFailed(request, Failure::UNAVAILABLE, "PageBroker is shutting down");
    }
    ReapHandlers(handlers_);
    if (handlers_.size() >= kMaxGpuHandlers) {
      return RequestFailed(request, Failure::UNAVAILABLE, "GPU connection limit reached");
    }
    handlers_.emplace_back(std::async(std::launch::async,
        [this, request, socket = std::move(socket), descriptors = std::move(descriptors)]() mutable {
          Run(request, std::move(socket), std::move(descriptors));
        }));
    return std::nullopt;
  }

 private:
  void Watch(int socket, const CancellationPtr& cancellation, std::stop_token stop)
  {
    while (!stop.stop_requested()) {
      if (shutting_down.load(std::memory_order_relaxed) || stopping_.load(std::memory_order_relaxed)) {
        cancellation->Cancel();
        return;
      }
      pollfd event{socket, POLLRDHUP, 0};
      const int ready = poll(&event, 1, kGpuWatchPollTimeoutMs);
      if (ready < 0 && errno == EINTR) {
        continue;
      }
      if (ready < 0) {
        LogError("watch GPU connection", {errno, std::generic_category()});
        cancellation->Cancel();
        return;
      }
      if (event.revents & (POLLRDHUP | POLLHUP | POLLERR | POLLNVAL)) {
        cancellation->Cancel();
        return;
      }
    }
  }

  void Run(const Request& request, FileDescriptor socket, std::vector<FileDescriptor> descriptors)
  {
    // The socket closes on return, even while its completed future is retained.
    const CancellationPtr cancellation = std::make_shared<Cancellation>();
    if (stopping_.load(std::memory_order_relaxed) || shutting_down.load(std::memory_order_relaxed)) {
      cancellation->Cancel();
    }
    std::jthread watcher([&](std::stop_token stop) { Watch(socket.get(), cancellation, stop); });
    const auto response = broker_.HandleGpuRequest(request, cancellation, std::move(descriptors));
    LogOutcome(request, response);
    SendResponse(socket.get(), response);
  }

  Broker& broker_;
  std::atomic<bool> stopping_{false};
  std::mutex mutex_;
  std::vector<std::future<void>> handlers_;
};

void
HandleConnection(int connection, Broker& broker, GpuConnections& gpu_connections)
{
  if (!IsAllowedClient(connection)) {
    return;
  }
  uint32_t size = 0;
  std::vector<FileDescriptor> descriptors;
  try {
    if (!ReadHeader(connection, size, descriptors)) {
      return;
    }
  } catch (const std::invalid_argument& error) {
    SendResponse(connection, RequestFailed(Request{}, Failure::INVALID_REQUEST, error.what()));
    return;
  }
  size = ntohl(size);

  Response response;
  if (size > kMaxMessageSize) {
    response = RequestFailed(Request{}, Failure::INVALID_REQUEST, "invalid request");
  } else {
    std::string message(size, '\0');
    Request request;
    if (!ReadAll(connection, message.data(), size) || !request.ParseFromString(message) || !request.IsInitialized()) {
      response = RequestFailed(Request{}, Failure::INVALID_REQUEST, "invalid request");
    } else {
      if (request.has_checkpoint_gpu() || request.has_restore_gpu()) {
        try {
          if (const auto rejected = gpu_connections.Start(connection, request, std::move(descriptors))) {
            LogOutcome(request, *rejected);
            SendResponse(connection, *rejected);
          }
        } catch (const std::exception& error) {
          response = RequestFailed(request, Failure::INTERNAL_ERROR, error.what());
          LogOutcome(request, response);
          SendResponse(connection, response);
        }
        return;
      }
      if (!descriptors.empty()) {
        SendResponse(connection, RequestFailed(request, Failure::INVALID_REQUEST,
                                               "descriptors are only allowed for GPU requests"));
        return;
      }
      const auto request_start = std::chrono::steady_clock::now();
      response = broker.HandleRequest(request);
      const auto duration =
          std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - request_start);
      LogOutcome(request, response, duration);
    }
  }

  SendResponse(connection, response);
}

void
ServeConnection(int connection, Broker& broker, GpuConnections& gpu_connections)
{
  FileDescriptor descriptor(connection);
  if (const auto error = ConfigureConnection(descriptor.get()); error) {
    LogError("set connection timeout", error);
    return;
  }
  HandleConnection(descriptor.get(), broker, gpu_connections);
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
  for (auto& handler : handlers) {
    WaitForHandler(handler);
  }
}

void
Serve(FileDescriptor& listener, Broker& broker, size_t max_concurrent_requests)
{
  // Control handlers must finish before the GPU connection owner is destroyed.
  GpuConnections gpu_connections(broker);
  std::vector<std::future<void>> handlers;
  handlers.reserve(max_concurrent_requests);
  auto next_transaction_reap = std::chrono::steady_clock::now();
  auto accept_retry_delay = kAcceptRetryInitialDelay;
  while (!shutting_down.load(std::memory_order_relaxed)) {
    ReapHandlers(handlers);
    const auto now = std::chrono::steady_clock::now();
    if (now >= next_transaction_reap) {
      broker.ReapExpiredTransactions(now);
      next_transaction_reap = now + kTransactionReapInterval;
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
      continue;
    }
    try {
      handlers.emplace_back(
          std::async(std::launch::async, [connection, &broker, &gpu_connections] {
            ServeConnection(connection, broker, gpu_connections);
          }));
    }
    catch (const std::exception& error) {
      FileDescriptor descriptor(connection);
      std::cerr << "start connection: " << error.what() << '\n';
    }
  }
  gpu_connections.Stop();
  listener = FileDescriptor(-1);
  broker.CancelGpuWork();
  WaitForHandlers(handlers);
}

size_t
ParseSize(std::string_view option, std::string_view input, bool allow_zero)
{
  size_t value = 0;
  const auto [end, error] = std::from_chars(input.data(), input.data() + input.size(), value);
  if (error != std::errc{} || end != input.data() + input.size() || (!allow_zero && value == 0)) {
    throw std::invalid_argument("invalid value for " + std::string(option) + ": " + std::string(input));
  }
  return value;
}

}  // namespace

DaemonOptions
ParseDaemonOptions(std::span<const std::string_view> arguments)
{
  constexpr size_t kPathArgumentCount = 3;
  if (arguments.size() < kPathArgumentCount) {
    throw std::invalid_argument("socket path, staging directory and storage root are required");
  }
  DaemonOptions options;
  options.socket_path = arguments[0];
  options.staging_directory = arguments[1];
  options.storage_root = arguments[2];
  for (size_t index = kPathArgumentCount; index < arguments.size(); ++index) {
    const auto option = arguments[index];
    if (++index == arguments.size()) {
      throw std::invalid_argument("missing value for " + std::string(option));
    }
    const auto input = arguments[index];
    if (option == "--custom-storage-engine") {
      if (input != "on" && input != "off") {
        throw std::invalid_argument("invalid value for --custom-storage-engine: " + std::string(input));
      }
      options.enable_gpu = input == "on";
    } else if (option == "--max-concurrent-requests") {
      options.max_concurrent_requests = ParseSize(option, input, false);
    } else if (option == "--custom-storage-buffer-count") {
      options.gpu.buffer_count = ParseSize(option, input, false);
    } else if (option == "--custom-storage-chunk-bytes") {
      options.gpu.chunk_bytes = ParseSize(option, input, false);
    } else if (option == "--custom-storage-max-pinned-bytes") {
      options.gpu.max_pinned_bytes = ParseSize(option, input, true);
    } else {
      throw std::invalid_argument("unknown option: " + std::string(option));
    }
  }
  return options;
}

ExitCode
RunDaemon(const DaemonOptions& options)
{
  shutting_down.store(false, std::memory_order_relaxed);
  snapshot::pagebroker::FatalCleanupShutdown cleanup_shutdown(shutting_down);
  if (!RaiseFileDescriptorLimit())
    return ExitCode::FAILURE;
  if (const auto error = InstallSignalHandlers(); error)
    return Fail("install signal handlers", error);
  if (const auto error = PrepareDirectories(options.socket_path, options.staging_directory); error)
    return Fail("create daemon directories", error);
  if (options.socket_path.string().size() >= sizeof(sockaddr_un::sun_path)) {
    std::cerr << "socket path is too long\n";
    return ExitCode::INVALID_ARGUMENTS;
  }
  snapshot::pagebroker::gpu::GpuEnginePtr gpu_engine;
  try {
    if (options.enable_gpu) {
      gpu_engine = std::make_shared<snapshot::pagebroker::gpu::GpuEngine>(options.gpu);
    }
  } catch (const std::exception& error) {
    std::cerr << "initialize GPU engine: " << error.what() << '\n';
    return ExitCode::FAILURE;
  }
  auto [listener, error] = CreateListener(options.socket_path);
  if (error)
    return Fail("create listener", error);

  if (chmod(options.socket_path.c_str(), S_IRUSR | S_IWUSR)) {
    return Fail("protect control socket", {errno, std::generic_category()});
  }
  try {
    Broker broker(options.staging_directory, options.storage_root, std::move(gpu_engine));
    Serve(listener, broker, options.max_concurrent_requests);
  } catch (const std::exception& failure) {
    std::cerr << "PageBroker: " << failure.what() << '\n';
    return ExitCode::FAILURE;
  }
  return ExitCode::SUCCESS;
}
