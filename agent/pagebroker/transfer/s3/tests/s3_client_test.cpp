// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_client.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>

#include <fstream>
#include <limits>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace {
S3Connection
OfflineConnection()
{
  S3Connection connection;
  connection.region = "us-east-1";
  connection.endpoint = "http://127.0.0.1:1";
  connection.access_key_id = "local-test-access-key";
  connection.secret_access_key = "local-test-secret-key";
  return connection;
}

S3Limits
SmallLimits()
{
  S3Limits limits;
  limits.part_size = limits.buffer_budget = 5 * 1024 * 1024;
  limits.workers = limits.active_files = 1;
  return limits;
}

TEST(S3ClientTest, ValidatesResourceLimitsBeforeConstruction)
{
  auto limits = SmallLimits();
  EXPECT_NO_THROW(limits.Validate());
  for (auto size : {0ULL, 1ULL, 5ULL * 1024 * 1024 - 1, 5ULL * 1024 * 1024 * 1024 + 1}) {
    auto bad = limits;
    bad.part_size = size;
    EXPECT_THROW(S3Client(OfflineConnection(), bad), std::invalid_argument);
  }
  auto bad = limits;
  ++bad.buffer_budget;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad.buffer_budget = 0;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad = limits;
  bad.workers = 0;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad = limits;
  bad.active_files = 0;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad = limits;
  bad.request_retries = std::numeric_limits<unsigned>::max();
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad = limits;
  bad.cleanup_rounds = 0;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  for (auto duration : {std::chrono::milliseconds(0), std::chrono::milliseconds(-1),
                        std::chrono::milliseconds::max()}) {
    bad = limits;
    bad.operation_timeout = duration;
    EXPECT_THROW(bad.Validate(), std::invalid_argument);
    bad = limits;
    bad.request_timeout = duration;
    EXPECT_THROW(bad.Validate(), std::invalid_argument);
    bad = limits;
    bad.connect_timeout = duration;
    EXPECT_THROW(bad.Validate(), std::invalid_argument);
  }
}

TEST(S3ClientTest, ValidatesMultipartBoundariesWithoutAllocation)
{
  const auto limits = SmallLimits();
  EXPECT_NO_THROW(limits.ValidateFileSize(0));
  EXPECT_NO_THROW(limits.ValidateFileSize(limits.part_size));
  EXPECT_NO_THROW(limits.ValidateFileSize(limits.part_size * 10'000));
  EXPECT_THROW(limits.ValidateFileSize(limits.part_size * 10'000 + 1), std::invalid_argument);
  EXPECT_THROW(limits.ValidateFileSize(std::numeric_limits<std::uint64_t>::max()), std::invalid_argument);
}

TEST(S3ClientTest, RejectsIncompleteCredentialsAndMalformedSettings)
{
  auto connection = OfflineConnection();
  connection.secret_access_key.clear();
  EXPECT_THROW(S3Client(connection, SmallLimits()), std::invalid_argument);
  connection.access_key_id.clear();
  connection.session_token = "local-test-session-token";
  EXPECT_THROW(S3Client(connection, SmallLimits()), std::invalid_argument);
  connection = OfflineConnection();
  connection.endpoint = "ftp://localhost";
  EXPECT_THROW(S3Client(connection, SmallLimits()), std::invalid_argument);
  connection = OfflineConnection();
  connection.region = std::string("us-east-1\0suffix", 16);
  EXPECT_THROW(S3Client(connection, SmallLimits()), std::invalid_argument);
}

TEST(S3ClientTest, RejectsDestinationsBeforeOpeningSource)
{
  S3Client uploader(OfflineConnection(), SmallLimits());
  for (const auto& destination : {
         S3ObjectLocation{"", "data"}, {"bucket", ""}, {"bucket", "line\nbreak"},
         {"bucket", std::string("nul\0key", 7)}, {"s3://bucket", "data"}, {"directory--x-s3", "data"},
         {"bucket", std::string(1025, 'a')}})
    EXPECT_THROW(uploader.UploadFile("/does-not-exist", destination), std::invalid_argument);
  EXPECT_THROW(uploader.UploadFile(std::string("source\0suffix", 13), {"bucket", "data"}), std::invalid_argument);
}

TEST(S3ClientTest, RejectsUnsupportedSourcesWithoutConnecting)
{
  S3Client uploader(OfflineConnection(), SmallLimits());
  test::TemporaryDirectory root;
  const S3ObjectLocation destination{"local-test-bucket", "data"};
  EXPECT_THROW(uploader.UploadFile(root.path() / "missing", destination), std::system_error);
  EXPECT_THROW(uploader.UploadFile(root.path(), destination), std::invalid_argument);
  const auto fifo = root.path() / "fifo";
  ASSERT_EQ(mkfifo(fifo.c_str(), 0600), 0);
  EXPECT_THROW(uploader.UploadFile(fifo, destination), std::invalid_argument);
  const auto file = root.path() / "file";
  std::ofstream(file) << "data";
  std::filesystem::create_symlink(file, root.path() / "link");
  EXPECT_THROW(uploader.UploadFile(root.path() / "link", destination), std::system_error);
}

TEST(S3ClientTest, RejectsExpiredAndCancelledWorkBeforeOpeningSource)
{
  S3Client uploader(OfflineConnection(), SmallLimits());
  const S3ObjectLocation destination{"local-test-bucket", "data"};
  S3UploadOptions options;
  options.control.deadline = TransferControl::Clock::now();
  try {
    uploader.UploadFile("/missing", destination, options);
    FAIL() << "deadline was ignored";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.primary.code, "DeadlineExceeded");
    EXPECT_EQ(error.remote_state, S3RemoteState::NO_WRITES_ISSUED);
  }
  std::stop_source cancellation;
  cancellation.request_stop();
  options.control = {TransferControl::Clock::time_point::max(), cancellation.get_token()};
  try {
    uploader.UploadFile("/missing", destination, options);
    FAIL() << "cancellation was ignored";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.primary.code, "Cancelled");
    EXPECT_EQ(error.remote_state, S3RemoteState::NO_WRITES_ISSUED);
  }
}

TEST(S3ClientTest, RejectsInvalidOrCancelledMetadataWithoutConnecting)
{
  S3Client client(OfflineConnection(), SmallLimits());
  const S3ObjectLocation object{"local-test-bucket", "index.json"};
  EXPECT_THROW(client.Head({"", "index.json"}), std::invalid_argument);
  EXPECT_THROW(client.Get(object, std::numeric_limits<std::size_t>::max()), std::invalid_argument);
  EXPECT_THROW(client.Put(object, "{}", 1), std::invalid_argument);
  std::stop_source cancellation;
  cancellation.request_stop();
  for (const auto& control : {
      TransferControl{TransferControl::Clock::now(), {}},
      TransferControl{TransferControl::Clock::time_point::max(), cancellation.get_token()}}) {
    EXPECT_THROW(client.Head(object, control), TransferInterrupted);
    EXPECT_THROW(client.Get(object, 1024, control), TransferInterrupted);
    EXPECT_THROW(client.Put(object, "{}", 1024, control), TransferInterrupted);
  }
}
}  // namespace
}  // namespace snapshot::pagebroker
