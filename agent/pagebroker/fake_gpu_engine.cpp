// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "fd_transport.hpp"
#include "gpu_engine.pb.h"
#include <unistd.h>
#include <chrono>
#include <filesystem>
#include <fstream>
#include <thread>

using namespace snapshot::pagebroker;
int main() {
  v1::NativeSessionReply reply;
  reply.set_report("ready");
  SendFrame(3, reply);
  internal::NativeBinding binding;
  std::vector<FileDescriptor> descriptors;
  while (ReceiveFrame(3, binding, descriptors)) {
    std::thread([binding, connection = std::move(descriptors[0]), directory = std::move(descriptors[1])]() {
      const std::filesystem::path path("/proc/self/fd/" + std::to_string(directory.get()));
      std::ofstream(path / "engine-pid") << getpid();
      std::ofstream(path / "target-pid") << binding.host_pid();
      v1::NativeSessionReply reply;
      reply.set_report("ready");
      SendFrame(connection.get(), reply);
      internal::NativeCommand command;
      std::vector<FileDescriptor> descriptors;
      while (ReceiveFrame(connection.get(), command, descriptors)) {
        if (command.has_drain()) {
          std::ofstream(path / "draining") << "true";
          while (!std::filesystem::exists(path / "allow-drain"))
            std::this_thread::sleep_for(std::chrono::milliseconds(5));
          reply.set_report("drained");
          SendFrame(connection.get(), reply);
          return;
        }
        reply.set_report("prepared");
        SendFrame(connection.get(), reply);
      }
    }).detach();
    descriptors.clear();
  }
}
