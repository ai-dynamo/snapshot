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
  void Upload(const S3ObjectLocation& prefix, Cancellation cancellation = Deadline())
  {
    client.UploadFiles(directory.path(), prefix, {{"source", 8}}, cancellation);
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
  EXPECT_NO_THROW(Upload({"bucket", "literal/$(false); `false` with spaces quote'\""}, Deadline()));
}

TEST_F(RcloneProcessTest, PassesFileListAndClosesUnrelatedDescriptors)
{
  FileDescriptor input(open(source.c_str(), O_RDONLY));
  ASSERT_GE(input.get(), 0);
  FileDescriptor unrelated(fcntl(input.get(), F_DUPFD, 200));
  ASSERT_GE(unrelated.get(), 200);
  EXPECT_NO_THROW(Upload({"bucket", "descriptors/" + std::to_string(unrelated.get())}, Deadline()));
}

TEST_F(RcloneProcessTest, DrainsDiagnosticsWithoutBlockingCompletion)
{
  EXPECT_NO_THROW(Upload({"bucket", "flood"}, Deadline()));
}

TEST_F(RcloneProcessTest, SendsNestedFilesInOneProcess)
{
  std::filesystem::create_directory(directory.path() / "nested");
  std::ofstream(directory.path() / "nested/# second") << "more";
  client.UploadFiles(directory.path(), {"bucket", "batch"}, {{"source", 8}, {"nested/# second", 4}}, Deadline());
  std::ifstream launches(directory.path() / "launches");
  std::string line;
  ASSERT_TRUE(std::getline(launches, line));
  EXPECT_EQ(line, "launch");
  EXPECT_FALSE(std::getline(launches, line));
  ExpectReaped();
}

TEST_F(RcloneProcessTest, EmptyBatchDoesNotLaunch)
{
  EXPECT_NO_THROW(client.UploadFiles(directory.path(), {"bucket", "exit127"}, {}, Deadline()));
  ExpectReaped();
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
      Upload({"bucket", "stubborn"}, cancellation);
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
    Upload({"bucket", "literal"}, Cancellation(Cancellation::Clock::now()));
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
    Upload({"bucket", "literal"}, Deadline());
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
    Upload({"bucket", "exit127"}, Deadline());
    FAIL();
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 0);
  }
  ExpectReaped();
}
} // namespace
} // namespace snapshot::pagebroker
