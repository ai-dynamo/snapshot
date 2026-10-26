// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_client.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>

#include <cstdlib>
#include <fstream>
#include <functional>
#include <initializer_list>
#include <limits>
#include <utility>
#include <vector>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace {
Cancellation
Deadline(std::chrono::milliseconds duration = std::chrono::seconds(30))
{
  return Cancellation(Cancellation::Clock::now() + duration);
}

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
  limits.part_size = 8 * 1024 * 1024;
  limits.active_files = 1;
  return limits;
}

// These tests run serially; restore the process environment after each case.
class CredentialEnvironment {
 public:
  CredentialEnvironment()
  {
    for (const auto* name :
         {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_SHARED_CREDENTIALS_FILE",
          "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_CA_BUNDLE"}) {
      const auto* value = std::getenv(name);
      saved_.push_back({name, value ? std::optional<std::string>(value) : std::nullopt});
      unsetenv(name);
    }
  }
  ~CredentialEnvironment()
  {
    for (const auto& [name, value] : saved_) {
      if (value)
        setenv(name.c_str(), value->c_str(), 1);
      else
        unsetenv(name.c_str());
    }
  }

 private:
  std::vector<std::pair<std::string, std::optional<std::string>>> saved_;
};

TEST(S3ClientTest, CapturesEnvironmentCredentialsIncludingSessionToken)
{
  CredentialEnvironment environment;
  setenv("AWS_ACCESS_KEY_ID", "fake-key", 1);
  setenv("AWS_SECRET_ACCESS_KEY", "fake-secret", 1);
  setenv("AWS_SESSION_TOKEN", "fake-token", 1);
  const auto snapshot = ResolveS3Credentials({});
  EXPECT_EQ(snapshot.access_key_id, "fake-key");
  EXPECT_EQ(snapshot.secret_access_key, "fake-secret");
  EXPECT_EQ(snapshot.session_token, "fake-token");
  setenv("AWS_ACCESS_KEY_ID", "replacement", 1);
  EXPECT_EQ(ResolveS3Credentials(snapshot).access_key_id, "fake-key");
  EXPECT_EQ(ResolveS3Credentials(OfflineConnection()).access_key_id, "local-test-access-key");
}

TEST(S3ClientTest, DoesNotDiscoverCredentialsFromFilesOrProfiles)
{
  CredentialEnvironment environment;
  test::TemporaryDirectory directory;
  const auto path = directory.path() / "credentials";
  std::ofstream(path) << "[default]\naws_access_key_id=file-key\naws_secret_access_key=file-secret\n";
  setenv("AWS_SHARED_CREDENTIALS_FILE", path.c_str(), 1);
  setenv("AWS_PROFILE", "default", 1);
  EXPECT_THROW(ResolveS3Credentials({}), std::invalid_argument);
  setenv("AWS_ACCESS_KEY_ID", "fake-key", 1);
  EXPECT_THROW(ResolveS3Credentials({}), std::invalid_argument);
  setenv("AWS_SECRET_ACCESS_KEY", "fake-secret", 1);
  const auto snapshot = ResolveS3Credentials({});
  EXPECT_EQ(snapshot.access_key_id, "fake-key");
  EXPECT_EQ(snapshot.secret_access_key, "fake-secret");
  EXPECT_TRUE(snapshot.session_token.empty());
}

TEST(S3ClientTest, RejectsPartialCredentialsWithoutMixingSources)
{
  CredentialEnvironment environment;
  EXPECT_THROW(ResolveS3Credentials({}), std::invalid_argument);
  setenv("AWS_SESSION_TOKEN", "fake-token", 1);
  EXPECT_THROW(ResolveS3Credentials({}), std::invalid_argument);
  setenv("AWS_SECRET_ACCESS_KEY", "fake-secret", 1);
  EXPECT_THROW(ResolveS3Credentials({}), std::invalid_argument);
  setenv("AWS_ACCESS_KEY_ID", "fake-key", 1);
  auto explicit_connection = OfflineConnection();
  explicit_connection.secret_access_key.clear();
  EXPECT_THROW(ResolveS3Credentials(explicit_connection), std::invalid_argument);
  const auto snapshot = ResolveS3Credentials(OfflineConnection());
  EXPECT_EQ(snapshot.access_key_id, "local-test-access-key");
  EXPECT_EQ(snapshot.secret_access_key, "local-test-secret-key");
  EXPECT_TRUE(snapshot.session_token.empty());
}

TEST(S3ClientTest, PreservesRegionAndCaStartupSettings)
{
  CredentialEnvironment environment;
  auto connection = OfflineConnection();
  connection.region.clear();
  setenv("AWS_DEFAULT_REGION", "default-region", 1);
  setenv("AWS_CA_BUNDLE", "/fake-ca.pem", 1);
  auto snapshot = ResolveS3Credentials(connection);
  EXPECT_EQ(snapshot.region, "default-region");
  EXPECT_EQ(snapshot.ca_file, "/fake-ca.pem");
  setenv("AWS_REGION", "preferred-region", 1);
  EXPECT_EQ(ResolveS3Credentials(connection).region, "preferred-region");
  connection.region = "explicit-region";
  connection.ca_file = "/explicit-ca.pem";
  snapshot = ResolveS3Credentials(connection);
  EXPECT_EQ(snapshot.region, "explicit-region");
  EXPECT_EQ(snapshot.ca_file, "/explicit-ca.pem");
}

TEST(S3ClientTest, RequiresResolvedCredentialsEvenWhenEnvironmentIsAvailable)
{
  CredentialEnvironment environment;
  setenv("AWS_ACCESS_KEY_ID", "fake-key", 1);
  setenv("AWS_SECRET_ACCESS_KEY", "fake-secret", 1);
  EXPECT_THROW(S3Client({}, SmallLimits()), std::invalid_argument);
  const auto connection = ResolveS3Credentials({});
  unsetenv("AWS_ACCESS_KEY_ID");
  unsetenv("AWS_SECRET_ACCESS_KEY");
  EXPECT_NO_THROW(S3Client(connection, SmallLimits()));
}

TEST(S3ClientTest, ValidatesResourceLimitsBeforeConstruction)
{
  auto limits = SmallLimits();
  EXPECT_NO_THROW(limits.Validate());
  for (auto size : {0ULL, 1ULL, 8ULL * 1024 * 1024 - 1, 5ULL * 1024 * 1024 * 1024 + 1}) {
    auto bad = limits;
    bad.part_size = size;
    EXPECT_THROW(S3Client(OfflineConnection(), bad), std::invalid_argument);
  }
  auto bad = limits;
  ++bad.buffer_budget;
  EXPECT_NO_THROW(bad.Validate());
  for (auto memory : {0ULL, 1ULL, 8ULL * 1024 * 1024 - 1}) {
    bad.buffer_budget = memory;
    EXPECT_THROW(S3Client(OfflineConnection(), bad), std::invalid_argument);
  }
  bad = limits;
  bad.active_files = 0;
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  bad = limits;
  bad.request_retries = std::numeric_limits<unsigned>::max();
  EXPECT_THROW(bad.Validate(), std::invalid_argument);
  for (auto duration :
       {std::chrono::milliseconds(0), std::chrono::milliseconds(-1), std::chrono::milliseconds::max()}) {
    bad = limits;
    bad.request_timeout = duration;
    EXPECT_THROW(bad.Validate(), std::invalid_argument);
    bad = limits;
    bad.connect_timeout = duration;
    EXPECT_THROW(bad.Validate(), std::invalid_argument);
  }
}

TEST(S3ClientTest, RequiresBufferPaddingForEveryRcloneProcess)
{
  auto limits = SmallLimits();
  limits.active_files = 2;
  for (auto part_size : {8ULL * 1024 * 1024, 8ULL * 1024 * 1024 + 1}) {
    limits.part_size = part_size;
    limits.buffer_budget = 18 * 1024 * 1024 - 1;
    EXPECT_THROW(limits.Validate(), std::invalid_argument);
    ++limits.buffer_budget;
    EXPECT_NO_THROW(limits.Validate());
    ++limits.buffer_budget;
    EXPECT_NO_THROW(limits.Validate());
  }
  limits.buffer_budget = std::numeric_limits<std::uint64_t>::max();
  EXPECT_THROW(limits.Validate(), std::invalid_argument);
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

TEST(S3ClientTest, DefaultPartSizeSupports625GiBFiles)
{
  const S3Limits limits;
  EXPECT_EQ(limits.part_size, 64ULL * 1024 * 1024);
  constexpr std::uint64_t max_file_size = 625ULL * 1024 * 1024 * 1024;
  EXPECT_NO_THROW(limits.ValidateFileSize(max_file_size));
  EXPECT_THROW(limits.ValidateFileSize(max_file_size + 1), std::invalid_argument);
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

TEST(S3ClientTest, RejectsInvalidBatchBeforeConnecting)
{
  S3Client client(OfflineConnection(), SmallLimits());
  test::TemporaryDirectory root;
  const S3ObjectLocation destination{"local-test-bucket", "data"};
  std::ofstream(root.path() / "file") << "data";
  for (const auto& path : {"", "/file", "../file", "nested/../file", "line\nbreak", "file/", "a//file"})
    EXPECT_THROW(client.UploadFiles(root.path(), destination, {{path, 4}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path(), {"", "data"}, {{"file", 4}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{"file", 3}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{"missing", 0}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{".", 0}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path() / "file", destination, {}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(std::string("source\0suffix", 13), destination, {}, Deadline()), std::invalid_argument);
  ASSERT_EQ(mkfifo((root.path() / "fifo").c_str(), 0600), 0);
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{"fifo", 0}}, Deadline()), std::invalid_argument);
  std::filesystem::create_symlink(root.path() / "file", root.path() / "link");
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{"link", 4}}, Deadline()), std::invalid_argument);
  std::filesystem::create_directory_symlink(root.path(), root.path() / "directory-link");
  EXPECT_THROW(client.UploadFiles(root.path(), destination, {{"directory-link/file", 4}}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.UploadFiles(root.path() / "directory-link", destination, {}, Deadline()), std::invalid_argument);
  EXPECT_NO_THROW(client.UploadFiles(root.path(), destination, {}, Deadline()));
}

TEST(S3ClientTest, RejectsExpiredAndCancelledWorkBeforeOpeningSource)
{
  S3Client uploader(OfflineConnection(), SmallLimits());
  const S3ObjectLocation destination{"local-test-bucket", "data"};
  Cancellation cancellation(Cancellation::Clock::now());
  try {
    uploader.UploadFiles("/missing", destination, {{"file", 0}}, cancellation);
    FAIL() << "deadline was ignored";
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
  }
  cancellation = Deadline();
  cancellation.Cancel();
  try {
    uploader.UploadFiles("/missing", destination, {{"file", 0}}, cancellation);
    FAIL() << "cancellation was ignored";
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::CANCELLED);
  }
}

TEST(S3ClientTest, RequiresCallerDeadline)
{
  S3Client client(OfflineConnection(), SmallLimits());
  EXPECT_THROW(client.Head({"bucket", "data"}, Cancellation{}), std::invalid_argument);
}

TEST(S3ClientTest, RejectsInvalidOrCancelledMetadataWithoutConnecting)
{
  S3Client client(OfflineConnection(), SmallLimits());
  const S3ObjectLocation object{"local-test-bucket", "index.json"};
  EXPECT_THROW(client.Head({"", "index.json"}, Deadline()), std::invalid_argument);
  EXPECT_THROW(client.Get(object, std::numeric_limits<std::size_t>::max(), Deadline()), std::invalid_argument);
  EXPECT_THROW(client.Put(object, "{}", 1, Deadline()), std::invalid_argument);
  Cancellation cancelled = Deadline();
  cancelled.Cancel();
  for (const auto& cancellation : {Cancellation(Cancellation::Clock::now()), cancelled}) {
    const auto code = cancellation.DeadlineExceeded() ? "DeadlineExceeded" : "Cancelled";
    for (const auto& [operation, call] : std::initializer_list<std::pair<const char*, std::function<void()>>>{
             {"HeadObject", [&] { client.Head(object, cancellation); }},
             {"HeadObject", [&] { client.Get(object, 1024, cancellation); }},
             {"PutObject", [&] { client.Put(object, "{}", 1024, cancellation); }}}) {
      SCOPED_TRACE(operation);
      SCOPED_TRACE(code);
      try {
        call();
        FAIL() << "cancelled metadata request succeeded";
      } catch (const CancellationError& error) {
        EXPECT_EQ(error.reason, *cancellation.Reason());
      }
    }
  }
}
} // namespace
} // namespace snapshot::pagebroker
