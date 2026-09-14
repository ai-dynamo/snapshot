// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <arpa/inet.h>
#include <chrono>
#include <cstdint>
#include <string>
#include <sys/socket.h>
#include <unistd.h>

#include "daemon_connection.hpp"
#include "pagebroker_types.hpp"

namespace {

using namespace snapshot::pagebroker;
using namespace std::chrono_literals;

TEST(DaemonConnectionTest, ConnectionLimitIsTypedPreMutationBusy)
{
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM, 0, sockets), 0);
  Request request;
  request.set_request_id("request-123");
  request.set_transaction_id("transaction-456");
  request.mutable_cuda_restore();
  const std::string request_message = request.SerializeAsString();
  const uint32_t request_size =
      htonl(static_cast<uint32_t>(request_message.size()));
  ASSERT_EQ(send(sockets[1], &request_size, sizeof(request_size), 0),
            static_cast<ssize_t>(sizeof(request_size)));
  ASSERT_EQ(send(sockets[1], request_message.data(), request_message.size(), 0),
            static_cast<ssize_t>(request_message.size()));

  ASSERT_TRUE(HandleConnectionLimit(sockets[0]));
  ASSERT_EQ(close(sockets[0]), 0);

  uint32_t network_size = 0;
  ASSERT_EQ(recv(sockets[1], &network_size, sizeof(network_size), MSG_WAITALL),
            static_cast<ssize_t>(sizeof(network_size)));
  std::string message(ntohl(network_size), '\0');
  ASSERT_EQ(recv(sockets[1], message.data(), message.size(), MSG_WAITALL),
            static_cast<ssize_t>(message.size()));
  ASSERT_EQ(close(sockets[1]), 0);

  Response response;
  ASSERT_TRUE(response.ParseFromString(message));
  EXPECT_EQ(response.request_id(), request.request_id());
  EXPECT_EQ(response.transaction_id(), request.transaction_id());
  ASSERT_TRUE(response.has_failure());
  EXPECT_EQ(response.failure().code(), Failure::BUSY);
  EXPECT_TRUE(response.failure().has_target_may_be_mutated());
  EXPECT_FALSE(response.failure().target_may_be_mutated());
}

TEST(DaemonConnectionTest, SlowRejectedClientCannotBlockAcceptLoop)
{
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM, 0, sockets), 0);

  const auto started = std::chrono::steady_clock::now();
  EXPECT_FALSE(HandleConnectionLimit(sockets[0]));
  const auto elapsed = std::chrono::steady_clock::now() - started;

  EXPECT_LT(elapsed, 500ms);
  EXPECT_EQ(close(sockets[0]), 0);
  EXPECT_EQ(close(sockets[1]), 0);
}

}  // namespace
