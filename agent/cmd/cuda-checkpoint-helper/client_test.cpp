// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "client.hpp"
#include "fd_transport.hpp"
#include "helper.pb.h"

#include <gtest/gtest.h>
#include <fcntl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>

#include <array>
#include <atomic>
#include <chrono>
#include <future>
#include <iostream>
#include <string>
#include <thread>
#include <vector>

namespace {
namespace protocol = snapshot::cuda_checkpoint::internal;
using snapshot::pagebroker::ReceiveFrame;
using snapshot::pagebroker::SendFrame;
using Request = protocol::GPUSessionRequest;
using Reply = protocol::GPUSessionReply;

std::array<FileDescriptor, 2> Pair()
{
  int descriptors[2];
  if (socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, descriptors))
    throw std::runtime_error("socketpair");
  const timeval timeout{2, 0};
  for (const int fd : descriptors) {
    setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));
  }
  return {FileDescriptor(descriptors[0]), FileDescriptor(descriptors[1])};
}
std::string FD(const FileDescriptor& file) { return std::to_string(file.get()); }

std::optional<int> Client(std::vector<std::string> args)
{
  args.insert(args.begin(), "cuda-checkpoint-helper");
  std::vector<char*> argv;
  for (auto& arg : args) argv.push_back(arg.data());
  return snapshot::cuda_checkpoint::RunClient(argv.size(), argv.data());
}

Request Read(int fd)
{
  Request request;
  std::vector<FileDescriptor> descriptors;
  if (!ReceiveFrame(fd, request, descriptors) || !descriptors.empty())
    throw std::runtime_error("missing phase request");
  return request;
}
void Respond(int fd, Reply::Status status)
{
  Reply reply;
  reply.set_status(status);
  SendFrame(fd, reply);
}
void Fail(int fd, const char* error)
{
  Reply reply;
  reply.mutable_failure()->set_message(error);
  SendFrame(fd, reply);
}
void ExpectEOF(int fd)
{
  char byte;
  EXPECT_EQ(recv(fd, &byte, 1, 0), 0);
}

TEST(HelperClient, LeavesLegacyCommandsToTheCEntryPoint)
{
  EXPECT_EQ(Client({"--get-restore-tid", "--pid", "123"}), std::nullopt);
}

TEST(HelperClient, WaitReadyPrintsTypedCapabilityWithProtoFieldNames)
{
  auto control = Pair();
  Reply ready;
  ready.set_status(Reply::READY);
  ready.set_custom_storage_available(true);
  ready.mutable_metrics()->set_visible_devices(8);
  SendFrame(control[1].get(), ready);
  testing::internal::CaptureStdout();
  const auto result = Client({"--wait-ready", "--control-fd", FD(control[0])});
  const auto output = testing::internal::GetCapturedStdout();
  EXPECT_EQ(result, 0);
  EXPECT_NE(output.find("\"custom_storage_available\":true"), std::string::npos);
  EXPECT_NE(output.find("\"visible_devices\":8"), std::string::npos);
}

TEST(HelperClient, RejectsWrongStartupStatusAndUnexpectedDescriptors)
{
  for (const bool extra_descriptor : {false, true}) {
    auto control = Pair();
    Reply ready;
    ready.set_status(extra_descriptor ? Reply::READY : Reply::DRAINED);
    SendFrame(control[1].get(), ready, extra_descriptor ? std::vector<int>{control[1].get()} : std::vector<int>{});
    EXPECT_EQ(Client({"--wait-ready", "--control-fd", FD(control[0])}), 1);
  }
}

TEST(HelperClient, BindHandsOffSessionAndDirectoryBeforeAcceptingReady)
{
  auto control = Pair();
  auto session = Pair();
  FileDescriptor directory(open("/tmp", O_RDONLY | O_DIRECTORY | O_CLOEXEC));
  auto server = std::async(std::launch::async, [&] {
    protocol::HelperRequest request;
    std::vector<FileDescriptor> descriptors;
    EXPECT_TRUE(ReceiveFrame(control[1].get(), request, descriptors));
    EXPECT_EQ(request.session_id(), 71);
    EXPECT_EQ(request.bind().namespace_pid(), 12);
    EXPECT_EQ(request.bind().container_pid(), 999);
    EXPECT_EQ(request.bind().direction(), protocol::BindGPUSession::LOAD);
    EXPECT_EQ(request.bind().storage_mode(), protocol::BindGPUSession::CUSTOM_STORAGE);
    EXPECT_TRUE(request.bind().enable_checksum_digest());
    EXPECT_EQ(descriptors.size(), 2);
    struct stat actual{}, expected{};
    EXPECT_EQ(fstat(descriptors.at(1).get(), &actual), 0);
    EXPECT_EQ(fstat(directory.get(), &expected), 0);
    EXPECT_EQ(actual.st_ino, expected.st_ino);
    EXPECT_EQ(actual.st_dev, expected.st_dev);
    Respond(descriptors.at(0).get(), Reply::READY);
  });
  EXPECT_EQ(Client({"--bind-batch", "--control-fd", FD(control[0]), "--directory-fd", FD(directory),
                    "--container-pid", "999", "--direction", "load", "--storage-mode", "custom",
                    "--visible-device", "GPU-one", "--checksum", "--session",
                    "71:12:" + FD(session[0]) + ":" + FD(session[1])}), 0);
  server.get();
}

TEST(HelperClient, AdmissionFailureCancelsAdmittedAndUnadmittedSiblings)
{
  auto control = Pair();
  auto first = Pair();
  auto second = Pair();
  FileDescriptor directory(open("/tmp", O_RDONLY | O_DIRECTORY | O_CLOEXEC));
  auto server = std::async(std::launch::async, [&] {
    protocol::HelperRequest request;
    std::vector<FileDescriptor> descriptors;
    EXPECT_TRUE(ReceiveFrame(control[1].get(), request, descriptors));
    Fail(descriptors.at(0).get(), "invalid saved manifest");
  });
  EXPECT_EQ(Client({"--bind-batch", "--control-fd", FD(control[0]), "--directory-fd", FD(directory),
                    "--container-pid", "999", "--direction", "load", "--storage-mode", "custom",
                    "--visible-device", "GPU-one", "--session", "1:12:" + FD(first[0]) + ":" + FD(first[1]),
                    "--session", "2:13:" + FD(second[0]) + ":" + FD(second[1])}), 1);
  server.get();
  ExpectEOF(first[1].get());
  ExpectEOF(second[1].get());
}

TEST(HelperClient, LoadOverlapsPreparationAndTransferAndCompletesAfterAllCopiesInReverseOrder)
{
  auto first = Pair();
  auto second = Pair();
  std::promise<void> transfer_started;
  auto started = transfer_started.get_future();
  std::promise<void> prepared;
  auto second_prepared = prepared.get_future();
  std::atomic<bool> second_transferred = false;
  std::atomic<bool> second_completed = false;
  auto first_server = std::async(std::launch::async, [&] {
    auto request = Read(first[1].get());
    EXPECT_EQ(request.operation(), Request::PREPARE);
    EXPECT_EQ(request.target_pid(), 101);
    Respond(first[1].get(), Reply::PREPARED);
    EXPECT_EQ(Read(first[1].get()).operation(), Request::TRANSFER);
    transfer_started.set_value();
    EXPECT_EQ(second_prepared.wait_for(std::chrono::seconds(1)), std::future_status::ready);
    Respond(first[1].get(), Reply::TRANSFERRED);
    EXPECT_EQ(Read(first[1].get()).operation(), Request::COMPLETE);
    EXPECT_TRUE(second_transferred);
    EXPECT_TRUE(second_completed);
    Respond(first[1].get(), Reply::COMPLETE);
  });
  auto second_server = std::async(std::launch::async, [&] {
    EXPECT_EQ(Read(second[1].get()).operation(), Request::PREPARE);
    EXPECT_EQ(started.wait_for(std::chrono::seconds(1)), std::future_status::ready);
    Respond(second[1].get(), Reply::PREPARED);
    prepared.set_value();
    EXPECT_EQ(Read(second[1].get()).operation(), Request::TRANSFER);
    std::this_thread::sleep_for(std::chrono::milliseconds(20));
    second_transferred = true;
    Respond(second[1].get(), Reply::TRANSFERRED);
    EXPECT_EQ(Read(second[1].get()).operation(), Request::COMPLETE);
    second_completed = true;
    Respond(second[1].get(), Reply::COMPLETE);
  });
  EXPECT_EQ(Client({"--run-batch", "--direction", "load", "--session", "11:" + FD(first[0]) + ":101",
                    "--session", "12:" + FD(second[0]) + ":102"}), 0);
  first_server.get();
  second_server.get();
}

TEST(HelperClient, SaveLocksEveryParticipantBeforePreparationAndPreparesAllBeforeTransfers)
{
  auto first = Pair();
  auto second = Pair();
  std::atomic<int> locked = 0;
  std::atomic<int> prepared = 0;
  auto serve = [&](int fd) {
    EXPECT_EQ(Read(fd).operation(), Request::LOCK);
    ++locked;
    Respond(fd, Reply::LOCKED);
    EXPECT_EQ(Read(fd).operation(), Request::PREPARE);
    EXPECT_EQ(locked, 2);
    ++prepared;
    Respond(fd, Reply::PREPARED);
    EXPECT_EQ(Read(fd).operation(), Request::TRANSFER);
    EXPECT_EQ(prepared, 2);
    Respond(fd, Reply::TRANSFERRED);
    EXPECT_EQ(Read(fd).operation(), Request::COMPLETE);
    Respond(fd, Reply::COMPLETE);
  };
  auto first_server = std::async(std::launch::async, serve, first[1].get());
  auto second_server = std::async(std::launch::async, serve, second[1].get());
  EXPECT_EQ(Client({"--run-batch", "--direction", "save", "--session", "11:" + FD(first[0]) + ":11",
                    "--session", "12:" + FD(second[0]) + ":12"}), 0);
  first_server.get();
  second_server.get();
}

TEST(HelperClient, TransferFailureCancelsAllSocketsBeforeJoiningAndNeverCompletes)
{
  auto first = Pair();
  auto second = Pair();
  std::promise<void> started;
  auto second_started = started.get_future();
  auto first_server = std::async(std::launch::async, [&] {
    EXPECT_EQ(Read(first[1].get()).operation(), Request::PREPARE);
    Respond(first[1].get(), Reply::PREPARED);
    EXPECT_EQ(Read(first[1].get()).operation(), Request::TRANSFER);
    EXPECT_EQ(second_started.wait_for(std::chrono::seconds(1)), std::future_status::ready);
    Fail(first[1].get(), "checksum mismatch");
    ExpectEOF(first[1].get());
  });
  auto second_server = std::async(std::launch::async, [&] {
    EXPECT_EQ(Read(second[1].get()).operation(), Request::PREPARE);
    Respond(second[1].get(), Reply::PREPARED);
    EXPECT_EQ(Read(second[1].get()).operation(), Request::TRANSFER);
    started.set_value();
    ExpectEOF(second[1].get());
    Fail(second[1].get(), "cancelled after draining");
  });
  EXPECT_EQ(Client({"--run-batch", "--direction", "load", "--session", "11:" + FD(first[0]) + ":101",
                    "--session", "12:" + FD(second[0]) + ":102"}), 1);
  first_server.get();
  second_server.get();
}

TEST(HelperClient, TelemetryOutputFailureDoesNotFailSuccessfulCUDAActions)
{
  auto session = Pair();
  auto server = std::async(std::launch::async, [&] {
    EXPECT_EQ(Read(session[1].get()).operation(), Request::PREPARE);
    Respond(session[1].get(), Reply::PREPARED);
    EXPECT_EQ(Read(session[1].get()).operation(), Request::TRANSFER);
    Respond(session[1].get(), Reply::TRANSFERRED);
    EXPECT_EQ(Read(session[1].get()).operation(), Request::COMPLETE);
    Respond(session[1].get(), Reply::COMPLETE);
  });
  std::cout.setstate(std::ios::badbit);
  const auto result = Client({"--run-batch", "--direction", "load", "--session", "11:" + FD(session[0]) + ":101"});
  std::cout.clear();
  EXPECT_EQ(result, 0);
  server.get();
}

TEST(HelperClient, DrainRequiresTheMatchingControlAcknowledgement)
{
  for (const bool wrong_id : {false, true}) {
    auto control = Pair();
    auto server = std::async(std::launch::async, [&] {
      protocol::HelperRequest request;
      std::vector<FileDescriptor> descriptors;
      EXPECT_TRUE(ReceiveFrame(control[1].get(), request, descriptors));
      EXPECT_TRUE(descriptors.empty());
      EXPECT_TRUE(request.drain());
      EXPECT_EQ(request.session_id(), 44);
      Reply reply;
      reply.set_status(Reply::DRAINED);
      reply.set_session_id(wrong_id ? 45 : 44);
      SendFrame(control[1].get(), reply);
    });
    EXPECT_EQ(Client({"--drain-batch", "--control-fd", FD(control[0]), "--session", "44"}), wrong_id ? 1 : 0);
    server.get();
  }
}
} // namespace
