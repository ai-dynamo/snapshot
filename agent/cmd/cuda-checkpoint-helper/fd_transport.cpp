// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "fd_transport.hpp"

#include <arpa/inet.h>
#include <sys/socket.h>
#include <cerrno>
#include <cstring>
#include <stdexcept>
#include <system_error>

namespace snapshot::pagebroker {
namespace {
void CheckIO(ssize_t result)
{
  if (result < 0)
    throw std::system_error(errno, std::generic_category(), "session socket");
  if (result == 0)
    throw std::runtime_error("session socket closed during frame");
}

// recvmsg on every read also rejects descriptors smuggled into a frame body.
size_t ReceiveBytes(int socket, void* output, size_t size, std::vector<FileDescriptor>& descriptors)
{
  alignas(cmsghdr) char control[CMSG_SPACE(kFrameDescriptorLimit * sizeof(int))]{};
  iovec buffer{output, size};
  msghdr header{};
  header.msg_iov = &buffer;
  header.msg_iovlen = 1;
  header.msg_control = control;
  header.msg_controllen = sizeof(control);
  ssize_t count;
  do {
    count = recvmsg(socket, &header, MSG_CMSG_CLOEXEC);
  } while (count < 0 && errno == EINTR);
  if (count < 0)
    CheckIO(count);
  bool invalid = (header.msg_flags & (MSG_CTRUNC | MSG_TRUNC)) != 0;
  for (auto* entry = CMSG_FIRSTHDR(&header); entry; entry = CMSG_NXTHDR(&header, entry)) {
    if (entry->cmsg_level != SOL_SOCKET || entry->cmsg_type != SCM_RIGHTS ||
        entry->cmsg_len < CMSG_LEN(0)) {
      invalid = true;
      continue;
    }
    const size_t bytes = entry->cmsg_len - CMSG_LEN(0);
    invalid |= bytes % sizeof(int) != 0;
    const auto* fds = reinterpret_cast<const int*>(CMSG_DATA(entry));
    for (size_t i = 0; i < bytes / sizeof(int); ++i)
      descriptors.emplace_back(fds[i]);
  }
  if (invalid || descriptors.size() > kFrameDescriptorLimit)
    throw std::runtime_error("invalid session descriptor frame");
  return static_cast<size_t>(count);
}
}  // namespace

bool ReceiveFrame(int socket, google::protobuf::MessageLite& message, std::vector<FileDescriptor>& descriptors)
{
  descriptors.clear();
  uint32_t prefix = 0;
  size_t count = ReceiveBytes(socket, &prefix, sizeof(prefix), descriptors);
  if (!count)
    return false;
  while (count < sizeof(prefix)) {
    std::vector<FileDescriptor> extra;
    const size_t read = ReceiveBytes(socket, reinterpret_cast<char*>(&prefix) + count, sizeof(prefix) - count, extra);
    CheckIO(read);
    if (!extra.empty())
      throw std::runtime_error("descriptors must accompany frame prefix");
    count += read;
  }
  const size_t size = ntohl(prefix);
  if (size > kFrameSizeLimit)
    throw std::runtime_error("session frame exceeds limit");
  std::string body(size, '\0');
  for (size_t offset = 0; offset < size;) {
    std::vector<FileDescriptor> extra;
    const size_t read = ReceiveBytes(socket, body.data() + offset, size - offset, extra);
    CheckIO(read);
    if (!extra.empty())
      throw std::runtime_error("unexpected body descriptors");
    offset += read;
  }
  if (!message.ParseFromString(body))
    throw std::runtime_error("invalid session protobuf");
  return true;
}

void SendFrame(int socket, const google::protobuf::MessageLite& message, const std::vector<int>& descriptors)
{
  const std::string body = message.SerializeAsString();
  if (body.size() > kFrameSizeLimit || descriptors.size() > kFrameDescriptorLimit)
    throw std::runtime_error("session frame exceeds limit");
  const uint32_t prefix = htonl(body.size());
  std::string bytes(reinterpret_cast<const char*>(&prefix), sizeof(prefix));
  bytes += body;
  alignas(cmsghdr) char control[CMSG_SPACE(kFrameDescriptorLimit * sizeof(int))]{};
  iovec buffer{bytes.data(), bytes.size()};
  msghdr header{};
  header.msg_iov = &buffer;
  header.msg_iovlen = 1;
  if (!descriptors.empty()) {
    header.msg_control = control;
    header.msg_controllen = CMSG_SPACE(descriptors.size() * sizeof(int));
    auto* entry = CMSG_FIRSTHDR(&header);
    entry->cmsg_level = SOL_SOCKET;
    entry->cmsg_type = SCM_RIGHTS;
    entry->cmsg_len = CMSG_LEN(descriptors.size() * sizeof(int));
    std::memcpy(CMSG_DATA(entry), descriptors.data(), descriptors.size() * sizeof(int));
  }
  ssize_t sent;
  do {
    sent = sendmsg(socket, &header, MSG_NOSIGNAL);
  } while (sent < 0 && errno == EINTR);
  CheckIO(sent);
  for (size_t offset = sent; offset < bytes.size();) {
    do {
      sent = send(socket, bytes.data() + offset, bytes.size() - offset, MSG_NOSIGNAL);
    } while (sent < 0 && errno == EINTR);
    CheckIO(sent);
    offset += sent;
  }
}

void SetSessionTimeout(int socket)
{
  // Includes transfer and cleanup, not merely command dispatch.
  const timeval timeout{300, 0};
  if (setsockopt(socket, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) ||
      setsockopt(socket, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)))
    throw std::system_error(errno, std::generic_category(), "session timeout");
}
}  // namespace snapshot::pagebroker
