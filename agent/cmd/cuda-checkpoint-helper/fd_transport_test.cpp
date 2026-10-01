// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "fd_transport.hpp"

#include <arpa/inet.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>
#include <google/protobuf/wrappers.pb.h>
#include <gtest/gtest.h>
#include <array>
#include <cerrno>
#include <cstring>
#include <filesystem>
#include <string>

namespace snapshot::pagebroker {
namespace {
std::string Prefix(uint32_t size)
{
  const auto encoded = htonl(size);
  return {reinterpret_cast<const char*>(&encoded), sizeof(encoded)};
}

size_t DescriptorCount()
{
  size_t count = 0;
  for (const auto& entry : std::filesystem::directory_iterator("/proc/self/fd")) {
    (void)entry;
    ++count;
  }
  return count;
}

class FdTransport : public ::testing::Test {
 protected:
  void SetUp() override
  {
    int sockets[2];
    ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
    sender = FileDescriptor(sockets[0]);
    receiver = FileDescriptor(sockets[1]);
  }

  void SendRaw(const std::string& data, const std::vector<int>& descriptors = {})
  {
    // Permit more rights than production accepts to exercise MSG_CTRUNC cleanup.
    ASSERT_LE(descriptors.size(), 8U);
    alignas(cmsghdr) std::array<char, CMSG_SPACE(8 * sizeof(int))> control{};
    iovec buffer{const_cast<char*>(data.data()), data.size()};
    msghdr message{};
    message.msg_iov = &buffer;
    message.msg_iovlen = 1;
    if (!descriptors.empty()) {
      message.msg_control = control.data();
      message.msg_controllen = CMSG_SPACE(descriptors.size() * sizeof(int));
      auto* entry = CMSG_FIRSTHDR(&message);
      entry->cmsg_level = SOL_SOCKET;
      entry->cmsg_type = SCM_RIGHTS;
      entry->cmsg_len = CMSG_LEN(descriptors.size() * sizeof(int));
      std::memcpy(CMSG_DATA(entry), descriptors.data(), descriptors.size() * sizeof(int));
    }
    ASSERT_EQ(sendmsg(sender.get(), &message, MSG_NOSIGNAL), static_cast<ssize_t>(data.size()));
  }

  FileDescriptor sender{-1};
  FileDescriptor receiver{-1};
};

TEST_F(FdTransport, RoundTripOwnsOrderedCloseOnExecDescriptors)
{
  FileDescriptor null(open("/dev/null", O_RDONLY | O_CLOEXEC));
  FileDescriptor zero(open("/dev/zero", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(null.get(), 0);
  ASSERT_GE(zero.get(), 0);
  google::protobuf::BytesValue sent;
  sent.set_value(std::string("hello\0world", 11));
  SendFrame(sender.get(), sent, {null.get(), zero.get()});
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  ASSERT_TRUE(ReceiveFrame(receiver.get(), received, descriptors));
  EXPECT_EQ(received.value(), sent.value());
  ASSERT_EQ(descriptors.size(), 2U);
  const std::array<int, 2> received_fds{descriptors[0].get(), descriptors[1].get()};
  for (const auto fd : received_fds)
    EXPECT_NE(fcntl(fd, F_GETFD) & FD_CLOEXEC, 0);
  char byte = 1;
  EXPECT_EQ(read(received_fds[0], &byte, 1), 0);
  EXPECT_EQ(read(received_fds[1], &byte, 1), 1);
  EXPECT_EQ(byte, 0);

  // Reusing the receive vector closes its previous rights. The sender still
  // owns its original descriptors, and a second frame carries no stale rights.
  sent.set_value("next");
  SendFrame(sender.get(), sent);
  ASSERT_TRUE(ReceiveFrame(receiver.get(), received, descriptors));
  EXPECT_EQ(received.value(), "next");
  EXPECT_TRUE(descriptors.empty());
  for (const auto fd : received_fds) {
    EXPECT_EQ(fcntl(fd, F_GETFD), -1);
    EXPECT_EQ(errno, EBADF);
  }
  EXPECT_GE(fcntl(null.get(), F_GETFD), 0);
  EXPECT_GE(fcntl(zero.get(), F_GETFD), 0);
}

TEST_F(FdTransport, EndOfStreamBetweenFramesIsClean)
{
  ASSERT_EQ(shutdown(sender.get(), SHUT_WR), 0);
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_FALSE(ReceiveFrame(receiver.get(), received, descriptors));
  EXPECT_TRUE(descriptors.empty());
}

TEST_F(FdTransport, TruncatedLengthPrefixIsAnError)
{
  SendRaw(Prefix(10).substr(0, 2));
  ASSERT_EQ(shutdown(sender.get(), SHUT_WR), 0);
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
}

TEST_F(FdTransport, TruncatedBodyRetainsRightsUntilCallerReleasesThem)
{
  FileDescriptor source(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(source.get(), 0);
  const auto before = DescriptorCount();
  SendRaw(Prefix(10) + "short", {source.get()});
  ASSERT_EQ(shutdown(sender.get(), SHUT_WR), 0);
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
  ASSERT_EQ(descriptors.size(), 1U);
  EXPECT_EQ(DescriptorCount(), before + 1);
  descriptors.clear();
  EXPECT_EQ(DescriptorCount(), before);
}

TEST_F(FdTransport, RejectsBodyRightsWithoutLeakingThem)
{
  FileDescriptor source(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(source.get(), 0);
  google::protobuf::BytesValue sent;
  sent.set_value("payload");
  const auto body = sent.SerializeAsString();
  const auto before = DescriptorCount();
  SendRaw(Prefix(body.size()));
  SendRaw(body, {source.get()});
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
  descriptors.clear();
  EXPECT_EQ(DescriptorCount(), before);
}

TEST_F(FdTransport, RejectsExcessRightsWithoutLeakingReceivedOrTruncatedRights)
{
  FileDescriptor source(open("/dev/null", O_RDONLY | O_CLOEXEC));
  ASSERT_GE(source.get(), 0);
  const auto before = DescriptorCount();
  SendRaw(Prefix(0), {source.get(), source.get(), source.get()});
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
  descriptors.clear();
  EXPECT_EQ(DescriptorCount(), before);
}

TEST_F(FdTransport, RejectsOversizedLengthBeforeWaitingForBody)
{
  SendRaw(Prefix(kFrameSizeLimit + 1));
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
}

TEST_F(FdTransport, RejectsInvalidProtobuf)
{
  SendRaw(Prefix(1) + std::string(1, '\x0f'));  // Invalid protobuf wire type 7.
  google::protobuf::BytesValue received;
  std::vector<FileDescriptor> descriptors;
  EXPECT_THROW(ReceiveFrame(receiver.get(), received, descriptors), std::runtime_error);
}

TEST_F(FdTransport, RejectsOversizedOutboundFramesBeforeWriting)
{
  google::protobuf::BytesValue message;
  message.set_value(std::string(kFrameSizeLimit, 'x'));
  EXPECT_THROW(SendFrame(sender.get(), message), std::runtime_error);
  message.clear_value();
  EXPECT_THROW(SendFrame(sender.get(), message, {0, 1, 2}), std::runtime_error);
  char byte;
  EXPECT_EQ(recv(receiver.get(), &byte, 1, MSG_DONTWAIT), -1);
  EXPECT_EQ(errno, EAGAIN);
}
}  // namespace
}  // namespace snapshot::pagebroker
