// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the ticket format and the control-socket helpers. These need
// no CUDA headers and no LD_PRELOAD: util.c and posix.c are linked in.

#include <dirent.h>
#include <fcntl.h>
#include <gtest/gtest.h>
#include <pthread.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include <cstring>
#include <string>

extern "C" {
#include "../posix.h"
#include "../protocol.h"
#include "../util.h"
}

namespace {

int open_fds() {
  int count = 0;
  DIR* dir = opendir("/proc/self/fd");
  if (dir == nullptr) return -1;
  while (readdir(dir) != nullptr) count++;
  closedir(dir);
  return count - 3;  // ".", "..", and the directory stream itself
}

cuinterpose_posix_ticket sample_ticket() {
  cuinterpose_posix_ticket ticket{};
  ticket.magic = CUINTERPOSE_POSIX_TICKET_MAGIC;
  ticket.version = CUINTERPOSE_POSIX_TICKET_VERSION;
  ticket.resource_kind = CUINTERPOSE_RESOURCE_UNICAST;
  std::memcpy(ticket.creator_participant, "0123456789abcdef0123456789abcdef", 33);
  for (size_t i = 0; i < sizeof(ticket.allocation_id); i++) ticket.allocation_id[i] = static_cast<uint8_t>(i + 1);
  for (size_t i = 0; i < sizeof(ticket.authorization); i++) ticket.authorization[i] = static_cast<uint8_t>(0xa0 + i);
  std::strcpy(ticket.creator_endpoint, "/snapshot-control/cuinterpose-42.sock");
  return ticket;
}

TEST(Util, BoundedSecondsParsing) {
  EXPECT_EQ(cuinterpose_bounded_seconds(nullptr, 7), 7u);
  EXPECT_EQ(cuinterpose_bounded_seconds("", 7), 7u);
  EXPECT_EQ(cuinterpose_bounded_seconds("30", 7), 30u);
  EXPECT_EQ(cuinterpose_bounded_seconds(" 30\n", 7), 30u);
  EXPECT_EQ(cuinterpose_bounded_seconds("3x", 7), 7u) << "trailing garbage must not arm a shorter timeout";
  EXPECT_EQ(cuinterpose_bounded_seconds("0", 7), 7u);
  EXPECT_EQ(cuinterpose_bounded_seconds("-5", 7), 7u);
  EXPECT_EQ(cuinterpose_bounded_seconds("86401", 7), 7u);
  EXPECT_EQ(cuinterpose_bounded_seconds("86400", 7), 86400u);
}

TEST(Util, RandomIdIsLowercaseHex) {
  char id[CUINTERPOSE_ID_SIZE];
  ASSERT_EQ(cuinterpose_random_id(id), 0);
  EXPECT_TRUE(cuinterpose_is_lower_hex_id(id));
  EXPECT_EQ(std::strlen(id), 32u);
  char other[CUINTERPOSE_ID_SIZE];
  ASSERT_EQ(cuinterpose_random_id(other), 0);
  EXPECT_STRNE(id, other);
  EXPECT_FALSE(cuinterpose_is_lower_hex_id("0123456789ABCDEF0123456789abcdef"));
  EXPECT_FALSE(cuinterpose_is_lower_hex_id("0123456789abcdef0123456789abcde"));
  EXPECT_FALSE(cuinterpose_is_lower_hex_id(nullptr));
}

TEST(Util, HeaderStringsMustBeTerminated) {
  cuinterpose_header header{};
  EXPECT_TRUE(cuinterpose_header_strings_terminated(&header));
  std::memset(header.message, 'x', sizeof(header.message));
  EXPECT_FALSE(cuinterpose_header_strings_terminated(&header));
  header = {};
  std::memset(header.participant_id, 'x', sizeof(header.participant_id));
  EXPECT_FALSE(cuinterpose_header_strings_terminated(&header));
  header = {};
  cuinterpose_header_error(&header, "boom");
  EXPECT_EQ(header.status, -1);
  EXPECT_STREQ(header.message, "boom");
}

TEST(Util, HeaderRoundTripWithDescriptor) {
  int pair[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, pair), 0);
  int passed = memfd_create("payload", MFD_CLOEXEC);
  ASSERT_GE(passed, 0);
  ASSERT_EQ(write(passed, "hi", 2), 2);

  cuinterpose_header out{};
  out.magic = CUINTERPOSE_MAGIC;
  out.version = CUINTERPOSE_VERSION;
  out.operation = CUINTERPOSE_EXPORT;
  std::strcpy(out.participant_id, "0123456789abcdef0123456789abcdef");
  ASSERT_EQ(cuinterpose_send_header(pair[0], &out, passed), 0);

  cuinterpose_header in{};
  int received = -1;
  ASSERT_EQ(cuinterpose_receive_header(pair[1], &in, &received), 0);
  EXPECT_EQ(in.magic, CUINTERPOSE_MAGIC);
  EXPECT_EQ(in.operation, CUINTERPOSE_EXPORT);
  EXPECT_STREQ(in.participant_id, out.participant_id);
  ASSERT_GE(received, 0);
  EXPECT_NE(received, passed);
  char buffer[3] = {};
  EXPECT_EQ(pread(received, buffer, 2, 0), 2);
  EXPECT_STREQ(buffer, "hi");
  // The received descriptor is close-on-exec so an exec'd child cannot inherit it.
  EXPECT_NE(fcntl(received, F_GETFD) & FD_CLOEXEC, 0);
  close(received);

  // Without a descriptor, *passed_fd is -1.
  ASSERT_EQ(cuinterpose_send_header(pair[0], &out, -1), 0);
  received = 99;
  ASSERT_EQ(cuinterpose_receive_header(pair[1], &in, &received), 0);
  EXPECT_EQ(received, -1);

  close(passed);
  close(pair[0]);
  close(pair[1]);
}

// A peer that sends two descriptors, or a short message, must not leak fds
// into this process.
TEST(Util, ReceiveHeaderClosesDescriptorsOnMalformedMessages) {
  int pair[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, pair), 0);
  int a = memfd_create("a", MFD_CLOEXEC);
  int b = memfd_create("b", MFD_CLOEXEC);
  ASSERT_GE(a, 0);
  ASSERT_GE(b, 0);
  const int before = open_fds();

  {
    cuinterpose_header out{};
    out.magic = CUINTERPOSE_MAGIC;
    char control[CMSG_SPACE(sizeof(int) * 2)] = {};
    iovec vector{&out, sizeof(out)};
    msghdr message{};
    message.msg_iov = &vector;
    message.msg_iovlen = 1;
    message.msg_control = control;
    message.msg_controllen = sizeof(control);
    cmsghdr* item = CMSG_FIRSTHDR(&message);
    item->cmsg_level = SOL_SOCKET;
    item->cmsg_type = SCM_RIGHTS;
    item->cmsg_len = CMSG_LEN(sizeof(int) * 2);
    int fds[2] = {a, b};
    std::memcpy(CMSG_DATA(item), fds, sizeof(fds));
    ASSERT_EQ(sendmsg(pair[0], &message, MSG_NOSIGNAL), static_cast<ssize_t>(sizeof(out)));
  }
  cuinterpose_header in{};
  int received = 5;
  EXPECT_EQ(cuinterpose_receive_header(pair[1], &in, &received), -1) << "two descriptors is a protocol violation";
  EXPECT_EQ(received, -1);
  EXPECT_EQ(open_fds(), before) << "the rejected descriptors must have been closed";

  {
    // Short message carrying one descriptor: rejected, descriptor closed.
    char control[CMSG_SPACE(sizeof(int))] = {};
    char short_payload[8] = {};
    iovec vector{short_payload, sizeof(short_payload)};
    msghdr message{};
    message.msg_iov = &vector;
    message.msg_iovlen = 1;
    message.msg_control = control;
    message.msg_controllen = sizeof(control);
    cmsghdr* item = CMSG_FIRSTHDR(&message);
    item->cmsg_level = SOL_SOCKET;
    item->cmsg_type = SCM_RIGHTS;
    item->cmsg_len = CMSG_LEN(sizeof(int));
    std::memcpy(CMSG_DATA(item), &a, sizeof(a));
    ASSERT_EQ(sendmsg(pair[0], &message, MSG_NOSIGNAL), static_cast<ssize_t>(sizeof(short_payload)));
    close(pair[0]);  // so MSG_WAITALL returns short
  }
  received = 5;
  EXPECT_EQ(cuinterpose_receive_header(pair[1], &in, &received), -1);
  EXPECT_EQ(received, -1);
  EXPECT_EQ(open_fds(), before - 1) << "only pair[0] was closed by the test itself";

  close(a);
  close(b);
  close(pair[1]);
}

TEST(Util, SendAllToClosedPeerFailsWithoutSignal) {
  int pair[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, pair), 0);
  close(pair[1]);
  char buffer[64] = {};
  // Would raise SIGPIPE with plain write(); MSG_NOSIGNAL turns it into EPIPE.
  EXPECT_EQ(cuinterpose_send_all(pair[0], buffer, sizeof(buffer)), -1);
  close(pair[0]);
}

TEST(Ticket, RoundTrip) {
  cuinterpose_posix_ticket ticket = sample_ticket();
  int fd = -1;
  ASSERT_EQ(cuinterpose_posix_create_ticket(&ticket, &fd), 0);
  ASSERT_GE(fd, 0);
  cuinterpose_posix_ticket read{};
  EXPECT_EQ(cuinterpose_posix_read_ticket(fd, &read), 0);
  EXPECT_EQ(std::memcmp(&read, &ticket, sizeof(ticket)), 0);
  // Sealed: nobody can alter a ticket after it is handed out.
  EXPECT_EQ(write(fd, "x", 1), -1);
  EXPECT_EQ(ftruncate(fd, 0), -1);
  close(fd);
}

TEST(Ticket, RejectsThingsThatAreNotTickets) {
  cuinterpose_posix_ticket read{};
  EXPECT_EQ(cuinterpose_posix_read_ticket(-1, &read), -1);

  int plain = memfd_create("plain", MFD_CLOEXEC);
  ASSERT_GE(plain, 0);
  cuinterpose_posix_ticket ticket = sample_ticket();
  ASSERT_EQ(write(plain, &ticket, sizeof(ticket)), static_cast<ssize_t>(sizeof(ticket)));
  EXPECT_EQ(cuinterpose_posix_read_ticket(plain, &read), -1) << "an unsealed memfd is not a ticket";
  close(plain);

  int devnull = open("/dev/null", O_RDONLY | O_CLOEXEC);
  ASSERT_GE(devnull, 0);
  EXPECT_EQ(cuinterpose_posix_read_ticket(devnull, &read), -1) << "a character device is a raw fd";
  close(devnull);

  auto rejects = [&](void (*mutate)(cuinterpose_posix_ticket&), const char* why) {
    cuinterpose_posix_ticket bad = sample_ticket();
    mutate(bad);
    int fd = -1;
    ASSERT_EQ(cuinterpose_posix_create_ticket(&bad, &fd), 0);
    EXPECT_EQ(cuinterpose_posix_read_ticket(fd, &read), -1) << why;
    close(fd);
  };
  rejects([](cuinterpose_posix_ticket& t) { t.magic = 0; }, "bad magic");
  rejects([](cuinterpose_posix_ticket& t) { t.version = 1; }, "old version");
  rejects([](cuinterpose_posix_ticket& t) { t.creator_participant[0] = 'X'; }, "participant not hex");
  rejects([](cuinterpose_posix_ticket& t) { t.creator_endpoint[0] = 'r'; }, "relative endpoint");
  rejects([](cuinterpose_posix_ticket& t) { std::memset(t.authorization, 0, sizeof(t.authorization)); }, "no authorization");
  rejects([](cuinterpose_posix_ticket& t) { t.resource_kind = 9; }, "unknown resource kind");
  rejects([](cuinterpose_posix_ticket& t) { t.allocation_size = 1; }, "unicast ticket with multicast fields");
  rejects([](cuinterpose_posix_ticket& t) { t.reserved[0] = 1; }, "reserved bytes must be zero");
}

// A fake creator endpoint: accepts one connection and answers as told.
struct Creator {
  std::string path;
  int listener = -1;
  cuinterpose_header reply{};
  int reply_fd = -1;
  bool garbage = false;
  cuinterpose_header request{};
  pthread_t thread{};

  explicit Creator(const std::string& dir) : path(dir + "/cuinterpose-1.sock") {
    listener = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    std::strncpy(address.sun_path, path.c_str(), sizeof(address.sun_path) - 1);
    if (bind(listener, reinterpret_cast<sockaddr*>(&address), sizeof(address)) != 0 || listen(listener, 1) != 0) {
      close(listener);
      listener = -1;
    }
  }
  static void* run(void* arg) {
    auto* self = static_cast<Creator*>(arg);
    int client = accept4(self->listener, nullptr, nullptr, SOCK_CLOEXEC);
    if (client < 0) return nullptr;
    int passed = -1;
    if (cuinterpose_receive_header(client, &self->request, &passed) == 0) {
      if (self->garbage) {
        (void)cuinterpose_send_all(client, "junk", 4);
      } else {
        (void)cuinterpose_send_header(client, &self->reply, self->reply_fd);
      }
    }
    if (passed >= 0) close(passed);
    close(client);
    return nullptr;
  }
  void start() { pthread_create(&thread, nullptr, run, this); }
  void join() { pthread_join(thread, nullptr); }
  ~Creator() {
    if (listener >= 0) close(listener);
    unlink(path.c_str());
  }
};

class RequestExport : public ::testing::Test {
 protected:
  void SetUp() override {
    char tmpl[] = "/tmp/cuinterpose-test-XXXXXX";
    ASSERT_NE(mkdtemp(tmpl), nullptr);
    dir = tmpl;
    ticket = sample_ticket();
  }
  void TearDown() override { rmdir(dir.c_str()); }
  cuinterpose_header good_reply(const Creator& creator) const {
    cuinterpose_header reply{};
    reply.magic = CUINTERPOSE_MAGIC;
    reply.version = CUINTERPOSE_VERSION;
    reply.operation = CUINTERPOSE_EXPORT;
    reply.resource_kind = ticket.resource_kind;
    std::strcpy(reply.participant_id, ticket.creator_participant);
    std::memcpy(reply.allocation_id, ticket.allocation_id, sizeof(reply.allocation_id));
    (void)creator;
    return reply;
  }
  std::string dir;
  cuinterpose_posix_ticket ticket;
};

TEST_F(RequestExport, ReturnsTheCreatorsDescriptor) {
  Creator creator(dir);
  ASSERT_GE(creator.listener, 0);
  std::strcpy(ticket.creator_endpoint, creator.path.c_str());
  creator.reply = good_reply(creator);
  creator.reply_fd = memfd_create("real", MFD_CLOEXEC);
  ASSERT_EQ(write(creator.reply_fd, "gpu", 3), 3);
  creator.start();

  int fd = -1;
  char error[96] = {};
  EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, error, sizeof(error)), 0) << error;
  creator.join();
  ASSERT_GE(fd, 0);
  char buffer[4] = {};
  EXPECT_EQ(pread(fd, buffer, 3, 0), 3);
  EXPECT_STREQ(buffer, "gpu");
  close(fd);
  close(creator.reply_fd);
  // The creator saw an EXPORT request carrying the ticket's identity and authorization.
  EXPECT_EQ(creator.request.operation, CUINTERPOSE_EXPORT);
  EXPECT_STREQ(creator.request.participant_id, ticket.creator_participant);
  EXPECT_EQ(std::memcmp(creator.request.authorization, ticket.authorization, sizeof(ticket.authorization)), 0);
  EXPECT_EQ(std::memcmp(creator.request.allocation_id, ticket.allocation_id, sizeof(ticket.allocation_id)), 0);
}

TEST_F(RequestExport, ReportsTheCreatorsError) {
  Creator creator(dir);
  ASSERT_GE(creator.listener, 0);
  std::strcpy(ticket.creator_endpoint, creator.path.c_str());
  creator.reply = good_reply(creator);
  cuinterpose_header_error(&creator.reply, "creator resource is unavailable");
  creator.start();
  int fd = -1;
  char error[96] = {};
  EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, error, sizeof(error)), -1);
  creator.join();
  EXPECT_EQ(fd, -1);
  EXPECT_STREQ(error, "creator resource is unavailable");
}

TEST_F(RequestExport, ClosesADescriptorAttachedToABadReply) {
  Creator creator(dir);
  ASSERT_GE(creator.listener, 0);
  std::strcpy(ticket.creator_endpoint, creator.path.c_str());
  creator.reply = good_reply(creator);
  creator.reply.allocation_id[0] ^= 0xff;  // wrong allocation
  creator.reply_fd = memfd_create("real", MFD_CLOEXEC);
  const int before = open_fds();
  creator.start();
  int fd = -1;
  char error[96] = {};
  EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, error, sizeof(error)), -1);
  creator.join();
  EXPECT_EQ(fd, -1);
  EXPECT_STREQ(error, "invalid creator export response");
  EXPECT_EQ(open_fds(), before) << "the descriptor on a rejected reply must be closed";
  close(creator.reply_fd);
}

TEST_F(RequestExport, HandlesGarbageAndAbsentCreators) {
  {
    Creator creator(dir);
    ASSERT_GE(creator.listener, 0);
    std::strcpy(ticket.creator_endpoint, creator.path.c_str());
    creator.garbage = true;
    creator.start();
    int fd = -1;
    char error[96] = {};
    EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, error, sizeof(error)), -1);
    creator.join();
    EXPECT_EQ(fd, -1);
    EXPECT_STREQ(error, "cannot contact creator endpoint");
  }
  std::strcpy(ticket.creator_endpoint, (dir + "/nobody.sock").c_str());
  int fd = -1;
  char error[96] = {};
  EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, error, sizeof(error)), -1);
  EXPECT_EQ(fd, -1);
  EXPECT_STREQ(error, "cannot contact creator endpoint");
  // A NULL error buffer is fine.
  EXPECT_EQ(cuinterpose_posix_request_export(&ticket, &fd, nullptr, 0), -1);
}

}  // namespace
