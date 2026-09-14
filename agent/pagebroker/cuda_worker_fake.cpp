// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "daemon_protocol.h"

#include <poll.h>
#include <signal.h>
#include <sys/socket.h>
#include <unistd.h>

#include <chrono>
#include <cstring>
#include <filesystem>
#include <string>
#include <thread>
#include <vector>

namespace protocol = cuda_checkpoint_daemon;
namespace {
volatile sig_atomic_t stopping = 0;
std::string operation_socket_path;
std::string storage_root;

void Stop(int) { stopping = 1; }

int Serve(int listener, bool health) {
  const int connection = accept4(listener, nullptr, nullptr, SOCK_CLOEXEC);
  if (connection < 0)
    return errno == EINTR ? 0 : 1;
  std::vector<unsigned char> packet(protocol::kMaxRequestSize + 1);
  const ssize_t received =
      recv(connection, packet.data(), packet.size(), MSG_TRUNC);
  protocol::Request request;
  std::string error;
  if (received <= 0 ||
      !protocol::ParseRequest(packet.data(), static_cast<size_t>(received),
                              &request, &error, storage_root)) {
    close(connection);
    return 0;
  }
  if (!health && request.device_map == "disconnect") {
    close(connection);
    return 0;
  }
  if (!health && request.device_map == "timeout")
    std::this_thread::sleep_for(std::chrono::milliseconds(250));
  if (!health && request.device_map == "malformed") {
    const unsigned char malformed[] = {1, 2, 3};
    (void)send(connection, malformed, sizeof(malformed), MSG_NOSIGNAL);
    close(connection);
    return 0;
  }
  if (!health && request.device_map == "bad-header") {
    std::vector<unsigned char> malformed(protocol::kResponseHeaderSize, 0);
    (void)send(connection, malformed.data(), malformed.size(), MSG_NOSIGNAL);
    close(connection);
    return 0;
  }
  if (!health && request.device_map == "oversized") {
    std::vector<unsigned char> oversized(protocol::kMaxResponseSize + 1, 0);
    (void)send(connection, oversized.data(), oversized.size(), MSG_NOSIGNAL);
    close(connection);
    return 0;
  }
  protocol::Response response;
  if (health) {
    const std::string one_timeout = operation_socket_path + ".hang-health-once";
    if (std::filesystem::exists(one_timeout)) {
      std::error_code ignored;
      std::filesystem::remove(one_timeout, ignored);
      std::this_thread::sleep_for(std::chrono::milliseconds(60));
    }
    if (std::filesystem::exists(operation_socket_path + ".hang-health"))
      std::this_thread::sleep_for(std::chrono::milliseconds(250));
    response.flags = protocol::kResponseCapabilityDeferredCUDA;
    const bool wrong_incarnation =
        std::filesystem::exists(operation_socket_path + ".wrong-incarnation");
    response.output =
        "{\"incarnation\":\"fake-" +
        std::to_string(static_cast<long>(wrong_incarnation ? getpid() + 1
                                                           : getpid())) +
        "\"}";
  } else if (request.device_map == "fatal") {
    response.cuda_status = 999;
    response.flags = protocol::kResponseFatal;
    response.error = "injected fatal response";
  }
  std::vector<unsigned char> encoded;
  if (protocol::EncodeResponse(response, &encoded, &error))
    (void)send(connection, encoded.data(), encoded.size(), MSG_NOSIGNAL);
  close(connection);
  return 0;
}
} // namespace

int main(int argc, char **argv) {
  std::string socket_path;
  for (int index = 1; index + 1 < argc; index += 2) {
    if (std::string(argv[index]) == "--socket")
      socket_path = argv[index + 1];
    else if (std::string(argv[index]) == "--storage-root")
      storage_root = argv[index + 1];
  }
  if (socket_path.empty() || storage_root.empty())
    return 2;
  operation_socket_path = socket_path;
  struct sigaction action {};
  action.sa_handler = Stop;
  sigemptyset(&action.sa_mask);
  if (sigaction(SIGTERM, &action, nullptr) != 0)
    return 2;
  protocol::OwnedUnixSocket operation;
  protocol::OwnedUnixSocket health;
  std::string error;
  if (!operation.Bind(socket_path, 16, &error) ||
      !health.Bind(socket_path + ".health", 4, &error))
    return 2;
  while (!stopping) {
    pollfd descriptors[] = {
        {.fd = operation.fd(), .events = POLLIN, .revents = 0},
        {.fd = health.fd(), .events = POLLIN, .revents = 0},
    };
    const int ready = poll(descriptors, 2, 50);
    if (ready < 0 && errno != EINTR)
      return 1;
    if (ready > 0 && (descriptors[0].revents & POLLIN) != 0 &&
        Serve(operation.fd(), false) != 0)
      return 1;
    if (ready > 0 && (descriptors[1].revents & POLLIN) != 0 &&
        Serve(health.fd(), true) != 0)
      return 1;
  }
  return 0;
}
