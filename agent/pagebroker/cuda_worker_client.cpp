// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "cuda_worker_client.hpp"

#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#include <cerrno>
#include <climits>
#include <cstring>
#include <utility>
#include <vector>

namespace snapshot::pagebroker {
namespace {

using Clock = std::chrono::steady_clock;
namespace protocol = cuda_checkpoint_daemon;

class ScopedFd {
public:
  explicit ScopedFd(int fd = -1) : fd_(fd) {}
  ScopedFd(const ScopedFd &) = delete;
  ScopedFd &operator=(const ScopedFd &) = delete;
  ~ScopedFd() {
    if (fd_ >= 0)
      close(fd_);
  }
  int get() const { return fd_; }

private:
  int fd_;
};

int RemainingMilliseconds(Clock::time_point deadline) {
  const auto remaining =
      std::chrono::duration_cast<std::chrono::milliseconds>(deadline -
                                                             Clock::now());
  if (remaining <= std::chrono::milliseconds::zero())
    return 0;
  return static_cast<int>(std::min<int64_t>(remaining.count(), INT_MAX));
}

bool WaitFor(int fd, short events, Clock::time_point deadline,
             std::string *error) {
  for (;;) {
    pollfd descriptor{.fd = fd, .events = events, .revents = 0};
    const int timeout = RemainingMilliseconds(deadline);
    if (timeout == 0) {
      *error = "CUDA worker RPC timed out";
      return false;
    }
    const int result = poll(&descriptor, 1, timeout);
    if (result > 0) {
      if ((descriptor.revents & events) != 0)
        return true;
      *error = "CUDA worker socket closed";
      return false;
    }
    if (result == 0) {
      *error = "CUDA worker RPC timed out";
      return false;
    }
    if (errno != EINTR) {
      *error = std::string("poll CUDA worker socket: ") +
               std::strerror(errno);
      return false;
    }
  }
}

bool MakeAddress(const std::string &path, sockaddr_un *address,
                 std::string *error) {
  if (path.empty() || path.front() != '/' ||
      path.size() >= sizeof(address->sun_path)) {
    *error = "CUDA worker socket path is not a bounded absolute path";
    return false;
  }
  *address = {};
  address->sun_family = AF_UNIX;
  std::memcpy(address->sun_path, path.c_str(), path.size() + 1);
  return true;
}

bool IsTimeout(const std::string &error) {
  return error == "CUDA worker RPC timed out";
}

} // namespace

CudaWorkerClient::CudaWorkerClient(std::string socket_path,
                                   std::chrono::milliseconds operation_timeout,
                                   std::chrono::milliseconds health_timeout)
    : socket_path_(std::move(socket_path)),
      operation_timeout_(operation_timeout),
      health_timeout_(health_timeout) {}

CudaWorkerRpcResult
CudaWorkerClient::Call(const protocol::Request &request) const {
  return CallSocket(socket_path_, request, operation_timeout_);
}

CudaWorkerRpcResult CudaWorkerClient::Health() const {
  protocol::Request request;
  return CallSocket(socket_path_ + ".health", request, health_timeout_);
}

CudaWorkerRpcResult CudaWorkerClient::CallSocket(
    const std::string &path, const protocol::Request &request,
    std::chrono::milliseconds timeout) const {
  CudaWorkerRpcResult result;
  std::vector<unsigned char> encoded;
  if (timeout <= std::chrono::milliseconds::zero() ||
      !protocol::EncodeRequest(request, &encoded, &result.error)) {
    result.status = CudaWorkerRpcStatus::kInvalidRequest;
    return result;
  }
  sockaddr_un address{};
  if (!MakeAddress(path, &address, &result.error)) {
    result.status = CudaWorkerRpcStatus::kConnectFailed;
    return result;
  }

  ScopedFd socket_fd(socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0));
  if (socket_fd.get() < 0) {
    result.status = CudaWorkerRpcStatus::kConnectFailed;
    result.error = std::string("create CUDA worker socket: ") +
                   std::strerror(errno);
    return result;
  }
  const int flags = fcntl(socket_fd.get(), F_GETFL);
  if (flags < 0 || fcntl(socket_fd.get(), F_SETFL, flags | O_NONBLOCK) < 0) {
    result.status = CudaWorkerRpcStatus::kConnectFailed;
    result.error = std::string("configure CUDA worker socket: ") +
                   std::strerror(errno);
    return result;
  }
  const auto deadline = Clock::now() + timeout;
  if (connect(socket_fd.get(), reinterpret_cast<sockaddr *>(&address),
              sizeof(address)) != 0) {
    if (errno != EINPROGRESS ||
        !WaitFor(socket_fd.get(), POLLOUT, deadline, &result.error)) {
      result.status = IsTimeout(result.error) ? CudaWorkerRpcStatus::kTimeout
                                              : CudaWorkerRpcStatus::kConnectFailed;
      return result;
    }
    int socket_error = 0;
    socklen_t socket_error_size = sizeof(socket_error);
    if (getsockopt(socket_fd.get(), SOL_SOCKET, SO_ERROR, &socket_error,
                   &socket_error_size) != 0 ||
        socket_error != 0) {
      result.status = CudaWorkerRpcStatus::kConnectFailed;
      result.error = std::string("connect CUDA worker socket: ") +
                     std::strerror(socket_error == 0 ? errno : socket_error);
      return result;
    }
  }

  if (!WaitFor(socket_fd.get(), POLLOUT, deadline, &result.error)) {
    result.status = IsTimeout(result.error) ? CudaWorkerRpcStatus::kTimeout
                                            : CudaWorkerRpcStatus::kDisconnected;
    return result;
  }
  std::vector<int> descriptors;
  const auto append_descriptors = [&](const protocol::Request &target) {
    for (const auto &file : target.pinned_storage_files) {
      if (file.descriptor_fd < 0 ||
          descriptors.size() >=
              protocol::kMaxPinnedStorageFilesPerRequest) {
        return false;
      }
      descriptors.push_back(file.descriptor_fd);
    }
    return true;
  };
  bool descriptors_valid = true;
  if (request.action == protocol::Action::kRestoreBatch) {
    for (const auto &target : request.targets)
      descriptors_valid = descriptors_valid && append_descriptors(target);
  } else {
    descriptors_valid = append_descriptors(request);
  }
  if (!descriptors_valid) {
    result.status = CudaWorkerRpcStatus::kInvalidRequest;
    result.error = "CUDA worker request has invalid or excessive pinned "
                   "storage descriptors";
    return result;
  }
  std::vector<unsigned char> control(
      descriptors.empty() ? 0 : CMSG_SPACE(sizeof(int) * descriptors.size()));
  iovec payload{.iov_base = encoded.data(), .iov_len = encoded.size()};
  msghdr message{};
  message.msg_iov = &payload;
  message.msg_iovlen = 1;
  if (!descriptors.empty()) {
    message.msg_control = control.data();
    message.msg_controllen = control.size();
    cmsghdr *header = CMSG_FIRSTHDR(&message);
    header->cmsg_level = SOL_SOCKET;
    header->cmsg_type = SCM_RIGHTS;
    header->cmsg_len = CMSG_LEN(sizeof(int) * descriptors.size());
    std::memcpy(CMSG_DATA(header), descriptors.data(),
                sizeof(int) * descriptors.size());
  }
  ssize_t sent;
  do {
    sent = sendmsg(socket_fd.get(), &message, MSG_NOSIGNAL);
  } while (sent < 0 && errno == EINTR);
  if (sent != static_cast<ssize_t>(encoded.size())) {
    result.status = CudaWorkerRpcStatus::kDisconnected;
    result.error = sent < 0 ? std::string("send CUDA worker request: ") +
                                  std::strerror(errno)
                            : "short CUDA worker request send";
    return result;
  }
  const bool mutating = request.action != protocol::Action::kHealth;

  if (!WaitFor(socket_fd.get(), POLLIN, deadline, &result.error)) {
    result.status = IsTimeout(result.error) ? CudaWorkerRpcStatus::kTimeout
                                            : CudaWorkerRpcStatus::kDisconnected;
    result.unknown_outcome = mutating;
    return result;
  }
  std::vector<unsigned char> response(protocol::kMaxResponseSize + 1);
  ssize_t received;
  do {
    received = recv(socket_fd.get(), response.data(), response.size(),
                    MSG_TRUNC);
  } while (received < 0 && errno == EINTR);
  if (received == 0) {
    result.status = CudaWorkerRpcStatus::kDisconnected;
    result.error = "CUDA worker disconnected before replying";
    result.unknown_outcome = mutating;
    return result;
  }
  if (received < 0) {
    result.status = CudaWorkerRpcStatus::kDisconnected;
    result.error = std::string("receive CUDA worker response: ") +
                   std::strerror(errno);
    result.unknown_outcome = mutating;
    return result;
  }
  if (static_cast<size_t>(received) > protocol::kMaxResponseSize ||
      !protocol::ParseResponse(response.data(), static_cast<size_t>(received),
                               &result.response, &result.error)) {
    result.status = CudaWorkerRpcStatus::kProtocolError;
    if (static_cast<size_t>(received) > protocol::kMaxResponseSize)
      result.error = "CUDA worker response exceeded the protocol limit";
    result.unknown_outcome = mutating;
    return result;
  }
  if ((result.response.flags & protocol::kResponseFatal) != 0) {
    result.status = CudaWorkerRpcStatus::kFatalResponse;
    result.error = result.response.error.empty()
                       ? "CUDA worker reported a fatal operation outcome"
                       : result.response.error;
    result.unknown_outcome = mutating;
    return result;
  }
  result.status = CudaWorkerRpcStatus::kOk;
  return result;
}

} // namespace snapshot::pagebroker
