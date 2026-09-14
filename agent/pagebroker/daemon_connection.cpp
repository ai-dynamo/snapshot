// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon_connection.hpp"

#include <arpa/inet.h>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <poll.h>
#include <string>
#include <sys/socket.h>

#include "pagebroker_types.hpp"

namespace snapshot::pagebroker {
namespace {

constexpr uint32_t kMaxMessageSize = 64 << 10;
constexpr auto kConnectionLimitDeadline = std::chrono::milliseconds(100);

bool WaitFor(int fd, short events,
             std::chrono::steady_clock::time_point deadline)
{
  for (;;) {
    const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(
        deadline - std::chrono::steady_clock::now());
    if (remaining <= std::chrono::milliseconds::zero())
      return false;
    pollfd descriptor{fd, events, 0};
    const int ready = poll(&descriptor, 1, static_cast<int>(remaining.count()));
    if (ready < 0)
      return false;
    if (ready == 0 || (descriptor.revents & (POLLERR | POLLHUP | POLLNVAL)))
      return false;
    if (descriptor.revents & events)
      return true;
  }
}

bool ReadAll(int fd, void* buffer, size_t size,
             std::chrono::steady_clock::time_point deadline)
{
  auto* bytes = static_cast<char*>(buffer);
  while (size > 0) {
    if (!WaitFor(fd, POLLIN, deadline))
      return false;
    const ssize_t read_size = recv(fd, bytes, size, MSG_DONTWAIT);
    if (read_size < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
      continue;
    if (read_size <= 0)
      return false;
    bytes += read_size;
    size -= read_size;
  }
  return true;
}

bool WriteAll(int fd, const void* buffer, size_t size,
              std::chrono::steady_clock::time_point deadline)
{
  const auto* bytes = static_cast<const char*>(buffer);
  while (size > 0) {
    if (!WaitFor(fd, POLLOUT, deadline))
      return false;
    const ssize_t written =
        send(fd, bytes, size, MSG_NOSIGNAL | MSG_DONTWAIT);
    if (written < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
      continue;
    if (written <= 0)
      return false;
    bytes += written;
    size -= written;
  }
  return true;
}

}  // namespace

bool HandleConnectionLimit(int connection)
{
  const auto deadline =
      std::chrono::steady_clock::now() + kConnectionLimitDeadline;
  uint32_t network_size = 0;
  if (!ReadAll(connection, &network_size, sizeof(network_size), deadline))
    return false;
  const uint32_t size = ntohl(network_size);
  if (size > kMaxMessageSize)
    return false;
  std::string request_message(size, '\0');
  Request request;
  if (!ReadAll(connection, request_message.data(), request_message.size(),
               deadline) ||
      !request.ParseFromString(request_message) || !request.IsInitialized())
    return false;

  Response response;
  response.set_request_id(request.request_id());
  response.set_transaction_id(request.transaction_id());
  auto* failure = response.mutable_failure();
  failure->set_code(Failure::BUSY);
  failure->set_message("PageBroker connection capacity is busy");
  failure->set_target_may_be_mutated(false);

  const std::string message = response.SerializeAsString();
  network_size = htonl(static_cast<uint32_t>(message.size()));
  return WriteAll(connection, &network_size, sizeof(network_size), deadline) &&
         WriteAll(connection, message.data(), message.size(), deadline);
}

}  // namespace snapshot::pagebroker
