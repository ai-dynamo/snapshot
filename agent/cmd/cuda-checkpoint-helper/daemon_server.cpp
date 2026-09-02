/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "daemon_server.h"

#include <cuda.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/un.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cctype>
#include <cerrno>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <string>
#include <system_error>
#include <thread>
#include <vector>

#include "cuda_operation.h"
#include "daemon_protocol.h"

namespace cuda_checkpoint_server {

namespace daemon_protocol = cuda_checkpoint_daemon;
using Clock = std::chrono::steady_clock;
constexpr int kClientReceiveTimeoutMilliseconds = 5000;

double SecondsSince(Clock::time_point start) {
  return std::chrono::duration<double>(Clock::now() - start).count();
}

class ScopedFd {
public:
  explicit ScopedFd(int fd) : fd_(fd) {}
  ScopedFd(const ScopedFd &) = delete;
  ScopedFd &operator=(const ScopedFd &) = delete;
  ~ScopedFd() noexcept {
    if (fd_ >= 0) {
      close(fd_);
    }
  }

  int get() const { return fd_; }

private:
  int fd_;
};

class DaemonThreadShutdown {
public:
  DaemonThreadShutdown(
      daemon_protocol::ShutdownSignalOwner *signal_owner,
      daemon_protocol::ShutdownSignalOwner::ShutdownResult *result)
      : signal_owner_(signal_owner), result_(result) {}
  DaemonThreadShutdown(const DaemonThreadShutdown &) = delete;
  DaemonThreadShutdown &operator=(const DaemonThreadShutdown &) = delete;

  ~DaemonThreadShutdown() noexcept {
    *result_ = signal_owner_->StopAndJoinNoThrow();
  }

private:
  daemon_protocol::ShutdownSignalOwner *signal_owner_;
  daemon_protocol::ShutdownSignalOwner::ShutdownResult *result_;
};

bool ValidSocketPath(const std::string &path) {
  const std::filesystem::path socket_path(path);
  const std::string filename = socket_path.filename();
  const bool clean_filename =
      !filename.empty() &&
      std::all_of(filename.begin(), filename.end(), [](unsigned char c) {
        return std::isalnum(c) || c == '.' || c == '_' || c == '-';
      });
  return !path.empty() && path.front() == '/' &&
         path.size() + sizeof(".health") <= sizeof(sockaddr_un::sun_path) &&
         socket_path.lexically_normal() == socket_path &&
         socket_path.parent_path() ==
             std::filesystem::path("/run/cuda-checkpoint-helper") &&
         clean_filename;
}

int RunHealthClient(const std::string &socket_path) {
  sockaddr_un address{};
  const std::string health_socket_path = socket_path + ".health";
  if (!ValidSocketPath(socket_path) ||
      health_socket_path.size() >= sizeof(address.sun_path)) {
    std::fprintf(stderr, "invalid daemon socket path\n");
    return 1;
  }
  address.sun_family = AF_UNIX;
  std::memcpy(address.sun_path, health_socket_path.c_str(),
              health_socket_path.size() + 1);
  const int socket_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
  if (socket_fd < 0 ||
      connect(socket_fd, reinterpret_cast<sockaddr *>(&address),
              sizeof(address)) != 0) {
    if (socket_fd >= 0) {
      close(socket_fd);
    }
    return 1;
  }
  timeval timeout{.tv_sec = kClientReceiveTimeoutMilliseconds / 1000,
                  .tv_usec = 0};
  if (setsockopt(socket_fd, SOL_SOCKET, SO_SNDTIMEO, &timeout,
                 sizeof(timeout)) != 0 ||
      setsockopt(socket_fd, SOL_SOCKET, SO_RCVTIMEO, &timeout,
                 sizeof(timeout)) != 0) {
    close(socket_fd);
    return 1;
  }
  daemon_protocol::Request request;
  std::vector<unsigned char> packet;
  std::string error;
  if (!daemon_protocol::EncodeRequest(request, &packet, &error) ||
      send(socket_fd, packet.data(), packet.size(), MSG_NOSIGNAL) !=
          static_cast<ssize_t>(packet.size())) {
    close(socket_fd);
    return 1;
  }
  packet.resize(daemon_protocol::kMaxResponseSize + 1);
  const ssize_t received =
      recv(socket_fd, packet.data(), packet.size(), MSG_TRUNC);
  close(socket_fd);
  daemon_protocol::Response response;
  if (received <= 0 ||
      static_cast<size_t>(received) > daemon_protocol::kMaxResponseSize ||
      !daemon_protocol::ParseResponse(packet.data(), received, &response,
                                      &error) ||
      response.cuda_status != CUDA_SUCCESS ||
      (response.flags & daemon_protocol::kResponseCapabilityDeferredCUDA) ==
          0) {
    return 1;
  }
  return 0;
}

bool RunHealthServer(daemon_protocol::OwnedUnixSocket *socket, int shutdown_fd,
                     int log_fd,
                     const daemon_protocol::OperationHealth *health,
                     cuda_checkpoint_operation::Service *operation_service) {
  std::vector<unsigned char> packet(daemon_protocol::kMaxRequestSize + 1);
  for (;;) {
    const int server_poll =
        daemon_protocol::PollForInputOrStop(socket->fd(), shutdown_fd);
    if (server_poll == -2) {
      dprintf(log_fd, "health socket poll failed: %s\n", std::strerror(errno));
      return false;
    }
    if (server_poll == 0) {
      return true;
    }
    const int accepted_fd =
        accept4(socket->fd(), nullptr, nullptr, SOCK_CLOEXEC);
    if (accepted_fd < 0) {
      if (errno == EINTR || errno == EAGAIN || errno == ECONNABORTED) {
        continue;
      }
      if (errno == EMFILE || errno == ENFILE || errno == ENOBUFS ||
          errno == ENOMEM) {
        dprintf(log_fd, "health socket accept temporarily failed: %s\n",
                std::strerror(errno));
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
        continue;
      }
      dprintf(log_fd, "health socket accept failed: %s\n",
              std::strerror(errno));
      return false;
    }
    ScopedFd client_fd(accepted_fd);
    const int client_poll = daemon_protocol::PollForInputOrStop(
        client_fd.get(), shutdown_fd, {}, kClientReceiveTimeoutMilliseconds);
    if (client_poll == -2) {
      dprintf(log_fd, "health client poll failed: %s\n",
              std::strerror(errno));
      return false;
    }
    if (client_poll == 0) {
      return true;
    }
    if (client_poll < 0) {
      continue;
    }
    const ssize_t received =
        recv(client_fd.get(), packet.data(), packet.size(), MSG_TRUNC);
    daemon_protocol::Request request;
    daemon_protocol::Response response;
    std::string error;
    if (received <= 0 ||
        static_cast<size_t>(received) > daemon_protocol::kMaxRequestSize ||
        !daemon_protocol::ParseRequest(packet.data(), received, &request,
                                       &error)) {
      response.cuda_status = CUDA_ERROR_INVALID_VALUE;
      response.error =
          received <= 0 ? "failed to receive health request" : error;
    } else if (request.action != daemon_protocol::Action::kHealth) {
      response.cuda_status = CUDA_ERROR_INVALID_VALUE;
      response.error = "health socket accepts only health requests";
    } else {
      std::string reap_error;
      const CUresult release_status =
          operation_service->ReapExited("/host/proc", &reap_error);
      response = daemon_protocol::HealthResponseAfterReap(
          *health, static_cast<int32_t>(release_status), reap_error);
      if (!reap_error.empty()) {
        // An unreadable /proc identity is not proof that the target exited.
        // Keep liveness successful so kubelet does not restart the helper and
        // release retained contexts during shutdown. Operation requests remain
        // fail-closed until identity can be established again.
        dprintf(log_fd, "target-context reaping deferred: %s\n",
                reap_error.c_str());
      }
    }
    std::vector<unsigned char> encoded;
    if (daemon_protocol::EncodeResponse(response, &encoded, &error)) {
      (void)send(client_fd.get(), encoded.data(), encoded.size(), MSG_NOSIGNAL);
    }
  }
  return true;
}

int RunDaemon(const std::string &socket_path, uint64_t max_operation_seconds) {
  daemon_protocol::ShutdownSignalOwner signal_owner;
  std::string setup_error;
  if (!signal_owner.Start(&setup_error)) {
    std::fprintf(stderr, "daemon shutdown setup failed: %s\n",
                 setup_error.c_str());
    return 1;
  }
  std::string incarnation;
  if (!daemon_protocol::GenerateIncarnation(&incarnation, &setup_error)) {
    std::fprintf(stderr, "%s\n", setup_error.c_str());
    return 1;
  }

  const std::filesystem::path path(socket_path);
  if (!ValidSocketPath(socket_path)) {
    std::fprintf(stderr, "invalid daemon socket path\n");
    return 1;
  }
  std::error_code filesystem_error;
  std::filesystem::create_directories(path.parent_path(), filesystem_error);
  if (filesystem_error || chmod(path.parent_path().c_str(), 0700) != 0) {
    std::fprintf(stderr, "failed to create private daemon socket directory\n");
    return 1;
  }

  cuda_checkpoint_operation::Service operation_service{
      std::chrono::seconds(max_operation_seconds)};
  cuda_checkpoint_operation::InitializationMetrics initialization;
  std::string initialization_error;
  if (!operation_service.Initialize(&initialization, &initialization_error)) {
    std::fprintf(stderr, "%s\n", initialization_error.c_str());
    return 1;
  }
  daemon_protocol::OwnedUnixSocket operation_socket;
  daemon_protocol::OwnedUnixSocket health_socket;
  std::string socket_error;
  if (!operation_socket.Bind(socket_path, 16, &socket_error)) {
    std::fprintf(stderr, "daemon operation socket setup failed: %s\n",
                 socket_error.c_str());
    return 1;
  }
  if (!health_socket.Bind(socket_path + ".health", 4, &socket_error)) {
    std::fprintf(stderr, "daemon health socket setup failed: %s\n",
                 socket_error.c_str());
    return 1;
  }
  // Operation capture redirects process-wide stderr. Keep the health thread on
  // the original container-log descriptor so its diagnostics cannot leak into
  // an unrelated operation response.
  ScopedFd health_log_fd(dup(STDERR_FILENO));
  if (health_log_fd.get() < 0) {
    std::fprintf(stderr, "duplicate daemon health log descriptor failed: %s\n",
                 std::strerror(errno));
    return 1;
  }
  daemon_protocol::OperationHealth operation_health{
      std::chrono::seconds(max_operation_seconds), incarnation};
  operation_health.MarkReady(initialization.custom_storage_available);
  daemon_protocol::ShutdownSignalOwner::ShutdownResult shutdown_result;
  daemon_protocol::ShutdownSignalOwner::ShutdownResult health_shutdown_result;
  std::atomic<bool> health_thread_failed{false};
  bool daemon_fatal = false;
  {
    // The guard is destroyed before the jthread: it stops and joins the
    // signal owner, which wakes the health server, and then jthread joins the
    // server.
    std::jthread health_thread;
    try {
      health_thread = std::jthread([&]() noexcept {
        try {
          if (!RunHealthServer(&health_socket, signal_owner.health_stop_fd(),
                               health_log_fd.get(), &operation_health,
                               &operation_service)) {
            health_thread_failed.store(true, std::memory_order_release);
            health_shutdown_result = signal_owner.RequestShutdownNoThrow();
          }
        } catch (...) {
          health_shutdown_result = signal_owner.RequestShutdownNoThrow();
          health_thread_failed.store(true, std::memory_order_release);
        }
      });
    } catch (const std::system_error &exception) {
      std::fprintf(stderr, "create daemon health thread failed: %s\n",
                   exception.what());
      return 1;
    }
    DaemonThreadShutdown shutdown_threads(&signal_owner, &shutdown_result);
    try {
      std::fprintf(
          stdout,
          "{\"event\":\"cuda_checkpoint_daemon_ready\",\"schema_version\":1,"
          "\"cuda_init_seconds\":%.6f,"
          "\"cuda_device_count\":%d,\"device_enumeration_seconds\":%.6f,"
          "\"primary_context_retain_seconds\":%.6f,\"cuda_driver_version\":%"
          "d,"
          "\"custom_storage_driver_api_available\":%s,"
          "\"custom_storage_transfer_backend_available\":%s,"
          "\"custom_storage_available\":%s,"
          "\"context_lifecycle\":\"target_identity\","
          "\"incarnation\":\"%s\"}\n",
          initialization.cuda_init_seconds, initialization.cuda_device_count,
          initialization.device_enumeration_seconds, 0.0,
          initialization.cuda_driver_version,
          initialization.custom_storage_driver_api_available ? "true"
                                                            : "false",
          initialization.custom_storage_transfer_backend_available
              ? "true"
              : "false",
          initialization.custom_storage_available ? "true" : "false",
          incarnation.c_str());
      std::fflush(stdout);

      std::vector<unsigned char> packet(daemon_protocol::kMaxRequestSize + 1);
      while (!signal_owner.ShutdownRequested() && !daemon_fatal) {
        const int server_poll = daemon_protocol::PollForInputOrStop(
            operation_socket.fd(), signal_owner.operation_stop_fd());
        if (server_poll == -2) {
          std::fprintf(stderr, "operation socket poll failed: %s\n",
                       std::strerror(errno));
          daemon_fatal = true;
          break;
        }
        if (server_poll == 0) {
          break;
        }
        const int accepted_fd =
            accept4(operation_socket.fd(), nullptr, nullptr, SOCK_CLOEXEC);
        if (accepted_fd < 0) {
          if (errno == EINTR || errno == EAGAIN || errno == ECONNABORTED) {
            continue;
          }
          if (errno == EMFILE || errno == ENFILE || errno == ENOBUFS ||
              errno == ENOMEM) {
            std::fprintf(stderr,
                         "operation socket accept temporarily failed: %s\n",
                         std::strerror(errno));
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
            continue;
          }
          if (signal_owner.ShutdownRequested()) {
            break;
          }
          std::perror("operation socket accept");
          daemon_fatal = true;
          break;
        }
        ScopedFd client_fd(accepted_fd);
        const int client_poll = daemon_protocol::PollForInputOrStop(
            client_fd.get(), signal_owner.operation_stop_fd(), {},
            kClientReceiveTimeoutMilliseconds);
        if (client_poll == -2) {
          std::fprintf(stderr, "operation client poll failed: %s\n",
                       std::strerror(errno));
          daemon_fatal = true;
          break;
        }
        if (client_poll == 0) {
          break;
        }
        if (client_poll < 0) {
          continue;
        }
        const ssize_t received =
            recv(client_fd.get(), packet.data(), packet.size(), MSG_TRUNC);
        daemon_protocol::Response response;
        daemon_protocol::Request request;
        std::string protocol_error;
        if (received <= 0 ||
            static_cast<size_t>(received) > daemon_protocol::kMaxRequestSize ||
            !daemon_protocol::ParseRequest(packet.data(), received, &request,
                                           &protocol_error)) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          response.error =
              received <= 0 ? "failed to receive request" : protocol_error;
        } else if (request.action == daemon_protocol::Action::kHealth) {
          response.cuda_status = CUDA_ERROR_INVALID_VALUE;
          response.error = "health requests must use the health socket";
        } else {
          const auto rpc_start = Clock::now();
          operation_health.Begin(request.action, request.pid);
          daemon_fatal = !daemon_protocol::ExecuteValidated(
              request, "/host/proc",
              [&operation_service](const daemon_protocol::Request &validated) {
                return operation_service.Execute(validated);
              },
              &response);
          operation_health.End();
          std::fprintf(stdout,
                       "{\"event\":\"cuda_checkpoint_daemon_operation\","
                       "\"schema_version\":1,\"action\":\"%s\","
                       "\"pid\":%u,\"cuda_status\":%d,\"fatal\":%s,\"rpc_"
                       "service_seconds\":%.6f}\n",
                       daemon_protocol::ActionName(request.action), request.pid,
                       response.cuda_status,
                       (response.flags & daemon_protocol::kResponseFatal) != 0
                           ? "true"
                           : "false",
                       SecondsSince(rpc_start));
          std::fflush(stdout);
        }
        std::vector<unsigned char> encoded;
        if (!daemon_protocol::EncodeResponse(response, &encoded,
                                             &protocol_error)) {
          daemon_protocol::Response bounded{
              .cuda_status = CUDA_ERROR_OPERATING_SYSTEM,
              .flags = response.flags & daemon_protocol::kResponseFatal,
              .output = "",
              .error = "daemon response exceeded protocol limit",
          };
          (void)daemon_protocol::EncodeResponse(bounded, &encoded,
                                                &protocol_error);
        }
        (void)send(client_fd.get(), encoded.data(), encoded.size(),
                   MSG_NOSIGNAL);
      }
    } catch (const std::exception &exception) {
      std::fprintf(stderr, "daemon processing failed: %s\n", exception.what());
      daemon_fatal = true;
    } catch (...) {
      std::fprintf(stderr, "daemon processing failed: unknown exception\n");
      daemon_fatal = true;
    }
  }
  if (health_thread_failed.load(std::memory_order_acquire)) {
    std::fprintf(stderr, "daemon health thread failed\n");
    daemon_fatal = true;
    if (!health_shutdown_result.ok()) {
      shutdown_result = health_shutdown_result;
    }
  }
  if (!shutdown_result.ok()) {
    std::fprintf(stderr, "daemon shutdown failed: %s: %s\n",
                 shutdown_result.operation,
                 std::strerror(shutdown_result.error_code));
    daemon_fatal = true;
  }
  signal_owner.Close();
  health_socket.Close();
  operation_socket.Close();
  std::string termination_error;
  CUresult termination_status = CUDA_ERROR_OPERATING_SYSTEM;
  while (termination_status != CUDA_SUCCESS) {
    termination_error.clear();
    termination_status = operation_service.TerminateRetainedTargets(
        "/host/proc", &termination_error);
    if (termination_status == CUDA_SUCCESS) {
      break;
    }
    // Releasing the helper's retained primary-context references while a
    // restored target is still alive can fault that target. An inconclusive
    // identity check is not evidence of exit, so graceful shutdown must stay
    // alive and retry instead of falling through to ReleaseAll(). Kubernetes
    // may still force-kill the helper after its Pod grace period. A live node
    // agent handles that loss fail-closed; whole-Pod loss is the documented
    // POSIX preview limitation until the replacement agent reconciles it.
    std::fprintf(stderr,
                 "terminate retained restore targets failed; retaining CUDA "
                 "contexts and retrying: %s\n",
                 termination_error.c_str());
    std::fflush(stderr);
    std::this_thread::sleep_for(std::chrono::seconds(1));
  }
  const auto release_start = Clock::now();
  const CUresult status = operation_service.ReleaseAll();
  std::fprintf(
      stdout,
      "{\"event\":\"cuda_checkpoint_daemon_stopped\",\"schema_version\":1,"
      "\"primary_context_release_seconds\":%.6f,\"primary_context_release_"
      "status\":%d,\"retained_target_termination_status\":%d,"
      "\"context_lifecycle\":\"target_identity\"}\n",
      SecondsSince(release_start), static_cast<int>(status),
      static_cast<int>(termination_status));
  return status == CUDA_SUCCESS && !daemon_fatal
             ? 0
             : 1;
}


} // namespace cuda_checkpoint_server
