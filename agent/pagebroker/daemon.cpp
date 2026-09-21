// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon.hpp"

#include <arpa/inet.h>
#include <sys/resource.h>
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
#include "file_descriptor.hpp"
#include "allocation_session.hpp"
#include "native_session.hpp"

namespace fs = std::filesystem;
using snapshot::pagebroker::Broker;
using snapshot::pagebroker::Failure;
using snapshot::pagebroker::Request;
using snapshot::pagebroker::Response;

namespace {
constexpr timeval kConnectionTimeout{30, 0};
constexpr rlim_t kRequiredFileDescriptors = 4096;
constexpr int kShutdownPollTimeoutMs = 1000;
constexpr auto kAcceptRetryInitialDelay = std::chrono::milliseconds(10);
constexpr auto kAcceptRetryMaxDelay = std::chrono::milliseconds(1000);
constexpr auto kTransactionReapInterval = std::chrono::minutes(2);
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

const char*
CommandName(Request::CommandCase command)
{
  switch (command) {
    case Request::kStagedRestore:
      return "staged_restore";
    case Request::kPrepareStagedCheckpoint:
      return "prepare_staged_checkpoint";
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
    case Response::kStagedRestoreDirectory:
      return "staged_restore";
    case Response::kStagedCheckpointDirectory:
      return "staged_checkpoint";
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

void
HandleConnection(int connection, Broker& broker)
{
  using namespace snapshot::pagebroker;
  Response response;
  Request request;
  std::vector<FileDescriptor> descriptors;
  std::unique_ptr<AllocationSession> session;
  std::unique_ptr<NativeSession> native;
  try {
    if (!ReceiveFrame(connection, request, descriptors))
      return;
    if (!descriptors.empty())
      throw std::invalid_argument("general broker requests cannot carry descriptors");
    if (request.has_bind_native()) {
      native = broker.BindNative(request);
      response.set_request_id(request.request_id());
      response.set_transaction_id(request.transaction_id());
      response.mutable_native_session();
    } else if (request.has_bind_allocations()) {
      session = broker.BindAllocations(request);
      response.set_request_id(request.request_id());
      response.set_transaction_id(request.transaction_id());
      response.mutable_allocation_session()->mutable_completed();
    } else {
      const auto request_start = std::chrono::steady_clock::now();
      response = broker.HandleRequest(request);
      const auto duration =
          std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - request_start);
      std::osyncstream(std::cerr) << "transaction=" << request.transaction_id()
                                  << " command=" << CommandName(request.command_case())
                                  << " result=" << ResultName(response) << " duration_ms=" << duration.count()
                                  << (response.has_failure() ? " error=" + response.failure().message() : "") << '\n';
    }
  } catch (const std::exception& error) {
    response = InvalidRequest();
    response.set_request_id(request.request_id());
    response.set_transaction_id(request.transaction_id());
    response.mutable_failure()->set_message(error.what());
  }
  SendFrame(connection, response);
  if (native) {
    SetAllocationTimeout(connection);
    v1::NativeSessionRequest operation;
    while (ReceiveFrame(connection, operation, descriptors)) {
      if (!descriptors.empty()) throw std::invalid_argument("native command cannot carry descriptors");
      auto reply = native->Execute(operation);
      SendFrame(connection, reply);
      if (reply.has_failure() || operation.operation() == v1::NativeSessionRequest::COMPLETE) return;
    }
    return;
  }
  if (!session)
    return;
  SetAllocationTimeout(connection);
  for (;;) {
    v1::AllocationSessionRequest operation;
    if (!ReceiveFrame(connection, operation, descriptors))
      return;
    auto reply = session->Execute(operation, descriptors);
    descriptors.clear();
    SendFrame(connection, reply);
    if (reply.has_finished() || reply.has_failure())
      return;
  }
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

void
Serve(FileDescriptor& listener, Broker& broker, size_t max_concurrent_requests)
{
  std::vector<std::future<void>> handlers;
  auto next_transaction_reap = std::chrono::steady_clock::now();
  auto accept_retry_delay = kAcceptRetryInitialDelay;
  while (!shutting_down) {
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
          std::async(std::launch::async, [connection, &broker] { ServeConnection(connection, broker); }));
    }
    catch (const std::exception& error) {
      FileDescriptor descriptor(connection);
      std::cerr << "start connection: " << error.what() << '\n';
    }
  }
  WaitForHandlers(handlers);
}
}  // namespace

ExitCode
RunDaemon(
    const fs::path& socket_path,
    const fs::path& staging_directory,
    const fs::path& storage_root,
    size_t max_concurrent_requests,
    const fs::path& allocation_worker)
{
  shutting_down = 0;
  if (!RaiseFileDescriptorLimit())
    return ExitCode::FAILURE;
  if (const auto error = InstallSignalHandlers(); error)
    return Fail("install signal handlers", error);
  if (const auto error = PrepareDirectories(socket_path, staging_directory); error)
    return Fail("create daemon directories", error);
  if (socket_path.string().size() >= sizeof(sockaddr_un::sun_path)) {
    std::cerr << "socket path is too long\n";
    return ExitCode::INVALID_ARGUMENTS;
  }
  auto [listener, error] = CreateListener(socket_path);
  if (error)
    return Fail("create listener", error);

  Broker broker(staging_directory, storage_root, allocation_worker);
  broker.StartGpuEngine();
  Serve(listener, broker, max_concurrent_requests);
  return ExitCode::SUCCESS;
}
