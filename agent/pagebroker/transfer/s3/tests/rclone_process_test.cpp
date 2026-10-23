// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "transfer/s3/s3_client.hpp"

#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/wait.h>
#include <unistd.h>

#include <fstream>
#include <future>
#include <thread>

#include "file_descriptor.hpp"
#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace {
Cancellation
Deadline(std::chrono::milliseconds duration = std::chrono::seconds(30))
{
  return Cancellation(Cancellation::Clock::now() + duration);
}

class RcloneProcessTest : public ::testing::Test {
 protected:
  S3Connection Connection()
  {
    S3Connection connection;
    connection.access_key_id = "fake-access-key";
    connection.secret_access_key = "fake-secret-key";
    connection.session_token = "fake-session-token";
    return connection;
  }
  void SetUp() override { std::ofstream(source) << "original"; }
  void ExpectReaped()
  {
    EXPECT_EQ(waitpid(-1, nullptr, WNOHANG), -1);
    EXPECT_EQ(errno, ECHILD);
  }
  test::TemporaryDirectory directory;
  std::filesystem::path source = directory.path() / "source";
  S3Client client{Connection()};
};

TEST_F(RcloneProcessTest, PassesLiteralArgumentsAndOnlySuppliedEnvironment)
{
  EXPECT_EQ(client.UploadFile(source, {"bucket", "literal/$(false); `false` with spaces quote'\""}, Deadline()), 8U);
}

TEST_F(RcloneProcessTest, PassesOpenedSourceAndClosesUnrelatedDescriptors)
{
  FileDescriptor input(open(source.c_str(), O_RDONLY));
  ASSERT_GE(input.get(), 0);
  FileDescriptor unrelated(fcntl(input.get(), F_DUPFD, 200));
  ASSERT_GE(unrelated.get(), 200);
  EXPECT_EQ(client.UploadFile(source, {"bucket", "descriptors/" + std::to_string(unrelated.get())}, Deadline()), 8U);
}

TEST_F(RcloneProcessTest, DrainsDiagnosticsWithoutBlockingCompletion)
{
  EXPECT_EQ(client.UploadFile(source, {"bucket", "flood"}, Deadline()), 8U);
}

TEST_F(RcloneProcessTest, KillsAndReapsOnOversizedOutput)
{
  EXPECT_THROW(client.Head({"bucket", "overflow"}, Deadline()), S3ResponseError);
  ExpectReaped();
}

TEST_F(RcloneProcessTest, EscalatesCancellationAndReapsBeforeReturning)
{
  const auto ready = source.string() + ".ready";
  Cancellation cancellation = Deadline();
  auto result = std::async(std::launch::async, [&] {
    try {
      client.UploadFile(source, {"bucket", "stubborn"}, cancellation);
      ADD_FAILURE() << "cancelled upload succeeded";
      return CancellationError::Reason::DEADLINE_EXCEEDED;
    } catch (const CancellationError& error) {
      return error.reason;
    }
  });
  const auto readiness_deadline = Cancellation::Clock::now() + std::chrono::seconds(5);
  while (!std::filesystem::exists(ready) && Cancellation::Clock::now() < readiness_deadline)
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  EXPECT_TRUE(std::filesystem::exists(ready));
  cancellation.Cancel();
  EXPECT_EQ(result.wait_for(std::chrono::seconds(4)), std::future_status::ready);
  EXPECT_EQ(result.get(), CancellationError::Reason::CANCELLED);
  ExpectReaped();
}

TEST_F(RcloneProcessTest, DeadlineIssuesNoWrites)
{
  try {
    client.UploadFile(source, {"bucket", "literal"}, Cancellation(Cancellation::Clock::now()));
    FAIL();
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
  }
  ExpectReaped();
}

TEST_F(RcloneProcessTest, MissingExecutableFailsAndReaps)
{
  const std::filesystem::path executable(PAGEBROKER_RCLONE_PATH);
  const auto saved = executable.string() + ".saved";
  std::filesystem::rename(executable, saved);
  std::exception_ptr failure;
  try {
    client.UploadFile(source, {"bucket", "literal"}, Deadline());
  } catch (...) {
    failure = std::current_exception();
  }
  std::filesystem::rename(saved, executable);
  ASSERT_TRUE(failure);
  try {
    std::rethrow_exception(failure);
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 0);
  }
  ExpectReaped();
}

TEST_F(RcloneProcessTest, ChildExit127FailsAndReaps)
{
  try {
    client.UploadFile(source, {"bucket", "exit127"}, Deadline());
    FAIL();
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 0);
  }
  ExpectReaped();
}
} // namespace
} // namespace snapshot::pagebroker
