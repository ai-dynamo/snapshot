// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "gpu_engine.hpp"
#include "gpu_engine.pb.h"
#include "fd_transport.hpp"
#include <gtest/gtest.h>
#include <fcntl.h>
#include <unistd.h>
#include <fstream>

using namespace snapshot::pagebroker;

TEST(GpuEngine, ReusesReadyProcessAndAcknowledgesDrain)
{
  char directory[] = "/tmp/pagebroker-engine-XXXXXX";
  ASSERT_NE(mkdtemp(directory), nullptr);
  const std::filesystem::path root(directory);
  GpuEngine engine(std::filesystem::absolute("build/fake-gpu-engine"));
  engine.Start();
  int previous_pid = 0;
  for (int iteration = 0; iteration < 2; ++iteration) {
    const auto path = root / std::to_string(iteration);
    std::filesystem::create_directory(path);
    FileDescriptor file(open(path.c_str(), O_DIRECTORY | O_CLOEXEC));
    v1::BindNativeSession binding;
    binding.set_direction(v1::BindNativeSession::LOAD);
    auto connection = engine.Bind(binding, getpid(), file.get());
    v1::NativeSessionReply reply;
    std::vector<FileDescriptor> descriptors;
    ASSERT_TRUE(ReceiveFrame(connection.get(), reply, descriptors));
    EXPECT_EQ(reply.report(), "ready");
    int pid = 0, target = 0;
    std::ifstream(path / "engine-pid") >> pid;
    std::ifstream(path / "target-pid") >> target;
    EXPECT_GT(pid, 0);
    EXPECT_EQ(target, getpid());
    if (previous_pid) { EXPECT_EQ(pid, previous_pid); }
    previous_pid = pid;
    internal::NativeCommand command;
    command.mutable_execute()->set_operation(v1::NativeSessionRequest::PREPARE);
    SendFrame(connection.get(), command);
    ASSERT_TRUE(ReceiveFrame(connection.get(), reply, descriptors));
    EXPECT_EQ(reply.report(), "prepared");
    std::ofstream(path / "allow-drain") << "true";
    command.set_drain(true);
    SendFrame(connection.get(), command);
    ASSERT_TRUE(ReceiveFrame(connection.get(), reply, descriptors));
    EXPECT_EQ(reply.report(), "drained");
  }
  engine.Stop();
  std::filesystem::remove_all(root);
}
