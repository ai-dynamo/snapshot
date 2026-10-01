// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_client.hpp"

#include <gtest/gtest.h>
#include <sys/resource.h>
#include <sys/stat.h>

#include <aws/core/auth/AWSCredentialsProvider.h>
#include <aws/s3/S3Client.h>
#include <aws/s3/model/HeadObjectRequest.h>

#include <algorithm>
#include <chrono>
#include <cstdlib>
#include <fstream>
#include <future>
#include <iostream>
#include <vector>

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"
#include "transfer/s3/s3_storage_backend.hpp"
#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

class S3RoundTripTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    fixture_ = Environment("PAGEBROKER_S3_TEST_FIXTURE");
    bucket_ = Environment("PAGEBROKER_S3_TEST_BUCKET");
    ASSERT_FALSE(fixture_.empty());
    ASSERT_FALSE(bucket_.empty());
    connection_.region = Environment("AWS_DEFAULT_REGION");
    connection_.endpoint = Environment("AWS_ENDPOINT_URL");
    connection_.ca_file = Environment("AWS_CA_BUNDLE");
    connection_.use_virtual_addressing = false;
    connection_.access_key_id = Environment("AWS_ACCESS_KEY_ID");
    connection_.secret_access_key = Environment("AWS_SECRET_ACCESS_KEY");
    limits_.part_size = 5 * 1024 * 1024;
    limits_.buffer_budget = 4 * limits_.part_size;
    limits_.workers = 2;
    limits_.active_files = 2;
    limits_.request_retries = 2;
    limits_.operation_timeout = std::chrono::seconds(30);
    limits_.request_timeout = std::chrono::seconds(3);
    limits_.connect_timeout = std::chrono::seconds(1);
  }

  ModelStreamerSessionOptions ReaderOptions() const
  {
    ModelStreamerSessionOptions options;
    options.region = connection_.region;
    options.endpoint = connection_.endpoint;
    options.access_key_id = connection_.access_key_id;
    options.secret_access_key = connection_.secret_access_key;
    return options;
  }

  S3ObjectLocation Destination(const std::string& scenario, const std::string& name = "data") const
  {
    return {bucket_, "roundtrip/" + scenario + "/" + name};
  }

  S3Config Config() const
  {
    S3Config config;
    config.transfer = {connection_, limits_, std::chrono::seconds(30)};
    return config;
  }

  S3CheckpointUploadPlan UploadPlan(const Path& source, const std::string& scenario) const
  {
    S3CheckpointUploadPlan plan{source, {}};
    for (const auto& entry : fs::recursive_directory_iterator(source)) {
      if (entry.is_regular_file()) {
        const auto relative = entry.path().lexically_relative(source);
        plan.files.push_back({relative, Destination(scenario, relative.generic_string())});
      }
    }
    return plan;
  }

  Path SingleFileTree(const std::string& name, bool large = false)
  {
    const auto source = temporary_.path() / name;
    fs::create_directory(source);
    fs::copy_file(fixture_ / (large ? "nested/large" : "small"), source / "data");
    return source;
  }

  RestorePlan FilePlan(const S3UploadResult& result) const
  {
    RestorePlan plan;
    plan.root_permissions = fs::perms::owner_all;
    plan.files.push_back({"s3://" + result.destination.bucket + "/" + result.destination.key,
                          "data", result.size_bytes, fs::perms::owner_read, result.sha256});
    return plan;
  }

  void VerifyFile(const S3UploadResult& result, const Path& source, const std::string& name)
  {
    ASSERT_EQ(result.size_bytes, fs::file_size(source));
    ASSERT_EQ(result.sha256, utils::ComputeFileSha256(source));
    ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
    const auto destination = temporary_.path() / name;
    restore_reader.Stage(FilePlan(result), destination);
    EXPECT_EQ(utils::ComputeFileSha256(destination / "data"), utils::ComputeFileSha256(source));
    EXPECT_EQ(fs::status(destination / "data").permissions(), fs::perms::owner_read);
  }

  S3Error FailedUpload(S3Client& uploader, const std::string& scenario, bool large = true)
  {
    try {
      uploader.UploadFile(fixture_ / (large ? "nested/large" : "small"), Destination(scenario));
      ADD_FAILURE() << "upload unexpectedly succeeded: " << scenario;
    }
    catch (const S3Error& error) {
      return error;
    }
    return S3Error({"test", "UnexpectedSuccess"});
  }

  // Test-only metadata requests synchronize with the local fault service.
  // The caller keeps an uploader (or an engine that uploaded) alive here.
  bool Barrier(const std::string& operation, const std::string& scenario)
  {
    Aws::S3::S3ClientConfiguration config(Aws::Client::ClientConfigurationInitValues{true});
    config.region = connection_.region;
    config.endpointOverride = connection_.endpoint;
    config.useVirtualAddressing = false;
    config.caFile = connection_.ca_file;
    config.httpRequestTimeoutMs = 12000;
    Aws::S3::S3Client client(Aws::Auth::AWSCredentials(connection_.access_key_id, connection_.secret_access_key), nullptr, config);
    Aws::S3::Model::HeadObjectRequest request;
    request.WithBucket(bucket_).WithKey("roundtrip/" + operation + "/" + scenario);
    return client.HeadObject(request).IsSuccess();
  }

  test::TemporaryDirectory temporary_;
  Path fixture_;
  std::string bucket_;
  S3Connection connection_;
  S3Limits limits_;
};

TEST_F(S3RoundTripTest, UploadsAndRestoresVerifiedTree)
{
  // Exercise the proposed production part size; the fixture crosses two parts.
  limits_.part_size = 16 * 1024 * 1024;
  limits_.buffer_budget = 2 * limits_.part_size;
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto plan = storage.UploadCheckpoint(UploadPlan(fixture_, "tree"));
  EXPECT_EQ(plan.root_permissions, fs::status(fixture_).permissions());
  std::size_t source_count = 0;
  for (const auto& entry : fs::recursive_directory_iterator(fixture_)) {
    ++source_count;
    const auto relative = entry.path().lexically_relative(fixture_);
    if (entry.is_directory()) {
      const auto directory = std::find_if(plan.directories.begin(), plan.directories.end(), [&](const auto& value) {
        return value.relative_path == relative;
      });
      ASSERT_NE(directory, plan.directories.end());
      EXPECT_EQ(directory->permissions, entry.status().permissions());
    } else {
      const auto file = std::find_if(plan.files.begin(), plan.files.end(), [&](const auto& value) {
        return value.relative_path == relative;
      });
      ASSERT_NE(file, plan.files.end());
      EXPECT_EQ(file->source_locator, "s3://" + bucket_ + "/roundtrip/tree/" + relative.generic_string());
      EXPECT_EQ(file->size_bytes, entry.file_size());
      EXPECT_EQ(file->expected_sha256, utils::ComputeFileSha256(entry.path()));
      EXPECT_EQ(file->permissions, entry.status().permissions());
    }
  }
  EXPECT_EQ(source_count, plan.directories.size() + plan.files.size());
  EXPECT_TRUE(std::is_sorted(plan.directories.begin(), plan.directories.end(), [](const auto& a, const auto& b) {
    return a.relative_path < b.relative_path;
  }));
  const auto destination = temporary_.path() / "tree";
  restore_reader.Stage(plan, destination);
  EXPECT_EQ(fs::status(destination).permissions(), plan.root_permissions);
  std::size_t count = 0;
  for ([[maybe_unused]] const auto& entry : fs::recursive_directory_iterator(destination))
    ++count;
  EXPECT_EQ(count, plan.directories.size() + plan.files.size());
  for (const auto& directory : plan.directories) {
    EXPECT_TRUE(fs::is_directory(destination / directory.relative_path));
    EXPECT_EQ(fs::status(destination / directory.relative_path).permissions(), directory.permissions);
  }
  for (const auto& file : plan.files) {
    EXPECT_EQ(fs::file_size(destination / file.relative_path), fs::file_size(fixture_ / file.relative_path));
    EXPECT_EQ(utils::ComputeFileSha256(destination / file.relative_path), utils::ComputeFileSha256(fixture_ / file.relative_path));
    EXPECT_EQ(fs::status(destination / file.relative_path).permissions(), file.permissions);
  }
}

TEST_F(S3RoundTripTest, PublishesAndReadsBoundedMetadata)
{
  S3Client client(connection_, limits_);
  const S3ObjectLocation object{bucket_, "metadata/index.json"};
  EXPECT_FALSE(client.Head(object));
  EXPECT_FALSE(client.Get(object, 1024));
  client.Put(object, "{}", 2);
  EXPECT_EQ(client.Head(object), 2U);
  EXPECT_EQ(client.Get(object, 2), "{}");
  EXPECT_THROW(client.Get(object, 1), S3ResponseError);
  try {
    client.Put(object, "different", 1024);
    FAIL() << "conditional metadata write overwrote an object";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.primary.http_status, 412);
  }
  EXPECT_EQ(client.Get(object, 2), "{}");
  const auto empty = Destination("metadata", "empty-index");
  client.Put(empty, "", 0);
  EXPECT_EQ(client.Get(empty, 0), "");
}

TEST_F(S3RoundTripTest, IndependentClientsRetainSdkLifetime)
{
  S3Client client(connection_, limits_);
  {
    S3Client other(connection_, limits_);
    EXPECT_TRUE(other.Get({bucket_, "fixture/small"}, 1024));
  }
  EXPECT_TRUE(client.Get({bucket_, "fixture/small"}, 1024));
}

TEST_F(S3RoundTripTest, HandlesMultipartThresholdAndExactParts)
{
  S3Client uploader(connection_, limits_);
  for (const auto size : {limits_.part_size - 1, limits_.part_size, limits_.part_size + 1, 2 * limits_.part_size}) {
    const auto path = temporary_.path() / "source";
    std::ofstream file(path, std::ios::binary);
    const std::string chunk(65536, 'a');
    for (std::uint64_t offset = 0; offset < size; offset += chunk.size())
      file.write(chunk.data(), std::min<std::uint64_t>(chunk.size(), size - offset));
    file.close();
    const auto name = std::to_string(size);
    VerifyFile(uploader.UploadFile(path, Destination("boundary", name)), path, name);
  }
}

TEST_F(S3RoundTripTest, PreflightsEntireTreeBeforeAnyUpload)
{
  S3StorageBackend storage(Config());
  const auto source = SingleFileTree("preflight-source");
  std::ofstream(source / "last") << "last file";
  const S3CheckpointUploadPlan valid{source, {{"data", Destination("preflight", "first")},
                                             {"last", Destination("preflight", "last")}}};
  for (const auto& path : std::vector<std::string>{"", ".", "..", "/absolute", "../escape", "a/../last",
                                                 "a//last", "last/", std::string("last\0hidden", 11)}) {
    auto invalid = valid;
    invalid.files.back().relative_path = path;
    EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument) << path;
  }
  auto invalid = valid;
  invalid.files.pop_back();
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  invalid = valid;
  invalid.files.push_back({"extra", Destination("preflight", "extra")});
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  invalid = valid;
  invalid.files.back().relative_path = "data";
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  invalid = valid;
  invalid.files.back().relative_path = "missing";
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  invalid = valid;
  invalid.files.back().destination = invalid.files.front().destination;
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  for (const auto& key : std::vector<std::string>{"", "bad\nkey", "bad\rkey", std::string("bad\0key", 7), std::string(1025, 'x')}) {
    invalid = valid;
    invalid.files.back().destination.key = key;
    EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  }
  invalid = valid;
  invalid.files.back().destination.bucket = "INVALID";
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);

  fs::create_symlink(source / "data", source / "link");
  EXPECT_THROW(storage.UploadCheckpoint(valid), std::runtime_error);
  fs::remove(source / "link");
  const auto link = temporary_.path() / "linked-root";
  fs::create_directory_symlink(source, link);
  invalid = valid;
  invalid.source_directory = link / "";
  EXPECT_THROW(storage.UploadCheckpoint(invalid), std::invalid_argument);
  ASSERT_EQ(mkfifo((source / "fifo").c_str(), 0600), 0);
  EXPECT_THROW(storage.UploadCheckpoint(valid), std::runtime_error);
  fs::remove(source / "fifo");

  // A sparse last file exercises the part-count limit without allocating it.
  fs::resize_file(source / "last", limits_.part_size * 10000 + 1);
  EXPECT_THROW(storage.UploadCheckpoint(valid), std::invalid_argument);
  fs::resize_file(source / "last", 0);
  // The host asserts no request at all used the preflight prefix.
}

TEST_F(S3RoundTripTest, RestoresAnEmptyDirectoryTreeWithoutObjects)
{
  const auto source = temporary_.path() / "empty-source";
  fs::create_directories(source / "nested/empty");
  fs::permissions(source / "nested/empty", fs::perms::owner_read | fs::perms::owner_exec);
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto plan = storage.UploadCheckpoint({source, {}});
  ASSERT_TRUE(plan.files.empty());
  ASSERT_EQ(plan.directories.size(), 2);
  restore_reader.Stage(plan, temporary_.path() / "empty-restore");
  EXPECT_EQ(fs::status(temporary_.path() / "empty-restore/nested/empty").permissions(),
            fs::status(source / "nested/empty").permissions());
}

TEST_F(S3RoundTripTest, PreservesPartialTreeFailureAndReusesBackend)
{
  const auto source = SingleFileTree("partial-source");
  std::ofstream(source / "last") << "last file";
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const S3CheckpointUploadPlan input{source, {{"data", Destination("engine-partial", "first")},
                                             {"last", Destination("engine-partial", "bad")}}};
  try {
    storage.UploadCheckpoint(input);
    FAIL() << "second object should be denied";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.destination.key, Destination("engine-partial", "bad").key);
    EXPECT_EQ(error.primary.operation, "PutObject");
    EXPECT_EQ(error.primary.http_status, 403);
    EXPECT_EQ(error.cleanup_state, S3CleanupState::NOT_NEEDED);
  }
  EXPECT_TRUE(Barrier("engine-partial", "first"));
  const auto plan = storage.UploadCheckpoint(UploadPlan(source, "engine-after-partial"));
  EXPECT_NO_THROW(restore_reader.Stage(plan, temporary_.path() / "partial-recovered"));
}

TEST_F(S3RoundTripTest, RetriesTransientErrorsAndRejectsAccessDenial)
{
  S3Client uploader(connection_, limits_);
  VerifyFile(uploader.UploadFile(fixture_ / "small", Destination("retry")), fixture_ / "small", "retry");
  const auto error = FailedUpload(uploader, "denied", false);
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.primary.http_status, 403);
  EXPECT_EQ(error.cleanup_state, S3CleanupState::NOT_NEEDED);
}

TEST_F(S3RoundTripTest, AbortsFailedMultipartAndRemainsUsable)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "part-failure");
  EXPECT_EQ(error.primary.operation, "UploadPart");
  EXPECT_FALSE(error.upload_id.empty());
  EXPECT_EQ(error.remote_state, S3RemoteState::NOT_COMPLETED);
  EXPECT_EQ(error.cleanup_state, S3CleanupState::CONFIRMED);
  VerifyFile(uploader.UploadFile(fixture_ / "small", Destination("after-failure")), fixture_ / "small", "after-failure");
}

TEST_F(S3RoundTripTest, PreservesPrimaryErrorWhenAbortFails)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "abort-failure");
  EXPECT_EQ(error.primary.operation, "UploadPart");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  ASSERT_TRUE(error.cleanup_failure);
  EXPECT_EQ(error.cleanup_failure->operation, "AbortMultipartUpload");
  EXPECT_FALSE(error.upload_id.empty());
}

TEST_F(S3RoundTripTest, ReportsUnconfirmedCleanupWhenVerificationFails)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "verify-failure");
  EXPECT_EQ(error.primary.operation, "UploadPart");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  ASSERT_TRUE(error.cleanup_failure);
  EXPECT_EQ(error.cleanup_failure->operation, "ListParts");
}

TEST_F(S3RoundTripTest, SourceTruncationAbortsInsteadOfSendingUninitializedBytes)
{
  S3Client uploader(connection_, limits_);
  const auto source = temporary_.path() / "source";
  fs::copy_file(fixture_ / "nested/large", source);
  auto upload = std::async(std::launch::async, [&] {
    try {
      uploader.UploadFile(source, Destination("source-truncated"));
      return S3Error({"test", "UnexpectedSuccess"});
    }
    catch (const S3Error& error) {
      return error;
    }
  });
  EXPECT_TRUE(Barrier("wait", "source-truncated"));
  fs::resize_file(source, 0);
  EXPECT_TRUE(Barrier("release", "source-truncated"));
  const auto error = upload.get();
  EXPECT_EQ(error.primary.code, "SourceShortened");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::CONFIRMED);
  EXPECT_EQ(error.remote_state, S3RemoteState::NOT_COMPLETED);
}

TEST_F(S3RoundTripTest, RejectsErrorEmbeddedInSuccessfulHttpResponse)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "complete-error");
  EXPECT_EQ(error.primary.operation, "CompleteMultipartUpload");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::CONFIRMED);
}

TEST_F(S3RoundTripTest, ReportsUncertainCompletionWithoutDeletingObjects)
{
  S3Client uploader(connection_, limits_);
  for (const auto& scenario : {"lost-complete", "lost-put"}) {
    const auto error = FailedUpload(uploader, scenario, std::string(scenario) == "lost-complete");
    EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
  }
  // The host independently verifies both completed objects still exist.
}

TEST_F(S3RoundTripTest, ReportsLostInitiationWithoutRetryingOrGuessingUploadId)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "lost-create");
  EXPECT_EQ(error.primary.operation, "CreateMultipartUpload");
  EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  EXPECT_TRUE(error.upload_id.empty());
}

TEST_F(S3RoundTripTest, DetectsCorruptionThroughNativeReader)
{
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = SingleFileTree("integrity-source");
  const auto corrupt = storage.UploadCheckpoint(UploadPlan(source, "corrupt"));
  EXPECT_THROW(restore_reader.Stage(corrupt, temporary_.path() / "corrupt"), std::runtime_error);
  EXPECT_EQ(fs::status(temporary_.path() / "corrupt/data").permissions(), fs::perms::owner_read | fs::perms::owner_write);
  const auto good = storage.UploadCheckpoint(UploadPlan(source, "integrity-good"));
  auto truncated = good;
  ++truncated.files.front().size_bytes;
  EXPECT_THROW(restore_reader.Stage(truncated, temporary_.path() / "truncated"), std::runtime_error);
  EXPECT_EQ(fs::status(temporary_.path() / "truncated/data").permissions(), fs::perms::owner_read | fs::perms::owner_write);
  EXPECT_NO_THROW(restore_reader.Stage(good, temporary_.path() / "integrity-recovered"));
  EXPECT_EQ(utils::ComputeFileSha256(temporary_.path() / "integrity-recovered/data"), utils::ComputeFileSha256(source / "data"));
}

TEST_F(S3RoundTripTest, BoundsConcurrentUploadsAndIsolatesFailure)
{
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = SingleFileTree("concurrent-source", true);
  const auto small = SingleFileTree("concurrent-small");
  std::vector<std::future<RestorePlan>> uploads;
  for (unsigned i = 0; i < 6; ++i)
    uploads.push_back(std::async(std::launch::async, [&, i] {
      return storage.UploadCheckpoint({source, {{"data", Destination("bounded", std::to_string(i))}}});
    }));
  auto failing = std::async(std::launch::async, [&] {
    EXPECT_THROW(storage.UploadCheckpoint(UploadPlan(small, "concurrent-failure")), S3Error);
  });
  std::vector<std::future<void>> restores;
  for (unsigned i = 0; i < uploads.size(); ++i) {
    const auto destination = temporary_.path() / ("concurrent-" + std::to_string(i));
    const auto plan = uploads[i].get();
    restores.push_back(std::async(std::launch::async, [&, plan, destination] {
      restore_reader.Stage(plan, destination);
      EXPECT_EQ(utils::ComputeFileSha256(destination / "data"), utils::ComputeFileSha256(source / "data"));
    }));
  }
  failing.get();
  for (auto& restore : restores)
    EXPECT_NO_THROW(restore.get());
  // The host-side counters independently assert active upload and HTTP bounds.
}

TEST_F(S3RoundTripTest, RestoreContinuesWhileUploadIsBlocked)
{
  limits_.request_timeout = std::chrono::seconds(12);
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto small = SingleFileTree("overlap-small");
  const auto large = SingleFileTree("overlap-large", true);
  const auto seed = storage.UploadCheckpoint(UploadPlan(small, "overlap-seed"));
  auto upload = std::async(std::launch::async, [&] {
    return storage.UploadCheckpoint(UploadPlan(large, "engine-overlap"));
  });
  EXPECT_TRUE(Barrier("wait", "engine-overlap"));
  auto restore = std::async(std::launch::async, [&] {
    restore_reader.Stage(seed, temporary_.path() / "overlap-restored");
  });
  EXPECT_EQ(restore.wait_for(std::chrono::seconds(3)), std::future_status::ready);
  EXPECT_TRUE(Barrier("release", "engine-overlap"));
  EXPECT_NO_THROW(restore.get());
  EXPECT_NO_THROW(upload.get());
}

TEST_F(S3RoundTripTest, DeadlineDrainsAndReleasesResources)
{
  limits_.operation_timeout = std::chrono::milliseconds(150);
  limits_.request_timeout = std::chrono::milliseconds(300);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  const auto begin = std::chrono::steady_clock::now();
  const auto error = FailedUpload(uploader, "deadline", false);
  EXPECT_NE(error.primary.code, "UnexpectedSuccess");
  EXPECT_LT(std::chrono::steady_clock::now() - begin, std::chrono::seconds(5));
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-deadline")));
}

TEST_F(S3RoundTripTest, MultipartDeadlineDrainsBeforeAborting)
{
  limits_.operation_timeout = std::chrono::milliseconds(500);
  limits_.request_timeout = std::chrono::milliseconds(700);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "multipart-deadline");
  EXPECT_NE(error.primary.code, "UnexpectedSuccess");
  EXPECT_FALSE(error.upload_id.empty());
  EXPECT_EQ(error.cleanup_state, S3CleanupState::CONFIRMED);
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-multipart-deadline")));
}

TEST_F(S3RoundTripTest, CoexistsWithReaderAndMultipleUploaderLifetimes)
{
  ModelStreamerRestore reader(ReaderOptions(), std::chrono::seconds(30));
  RestorePlan reader_fixture;
  reader_fixture.files.push_back({"s3://" + bucket_ + "/fixture/small", "data", fs::file_size(fixture_ / "small"),
                                  fs::perms::owner_read, utils::ComputeFileSha256(fixture_ / "small")});
  reader.Stage(reader_fixture, temporary_.path() / "before");
  {
    S3Client first(connection_, limits_);
    {
      auto provider = connection_;
      provider.access_key_id.clear();
      provider.secret_access_key.clear();
      auto config = Config();
      config.transfer.connection = provider;
      S3StorageBackend second(config);
      ModelStreamerRestore provider_reader({provider.region, provider.endpoint}, std::chrono::seconds(30));
      const auto source = SingleFileTree("provider-source");
      auto reading = std::async(std::launch::async, [&] { reader.Stage(reader_fixture, temporary_.path() / "during"); });
      const auto plan = second.UploadCheckpoint(UploadPlan(source, "provider"));
      EXPECT_NO_THROW(provider_reader.Stage(plan, temporary_.path() / "provider-restored"));
      EXPECT_NO_THROW(reading.get());
    }
    EXPECT_NO_THROW(first.UploadFile(fixture_ / "small", Destination("after-second")));
  }
  EXPECT_NO_THROW(reader.Stage(reader_fixture, temporary_.path() / "after"));
}

TEST_F(S3RoundTripTest, RejectsUntrustedTLS)
{
  if (Environment("PAGEBROKER_S3_TEST_TLS_REJECT") != "1")
    GTEST_SKIP() << "run in a separate process without the fixture CA";
  ASSERT_TRUE(connection_.endpoint.starts_with("https://"));
  limits_.request_retries = 0;
  S3StorageBackend storage(Config());
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = SingleFileTree("untrusted-source");
  EXPECT_THROW(storage.UploadCheckpoint(UploadPlan(source, "untrusted")), S3Error);
  RestorePlan plan;
  plan.files.push_back({"s3://" + bucket_ + "/fixture/small", "data", fs::file_size(fixture_ / "small"),
                        fs::perms::owner_read, utils::ComputeFileSha256(fixture_ / "small")});
  EXPECT_THROW(restore_reader.Stage(plan, temporary_.path() / "untrusted-restore"), std::runtime_error);
}

TEST_F(S3RoundTripTest, UploadMemoryProbe)
{
  const auto requested = Environment("PAGEBROKER_S3_TEST_MEMORY_MIB");
  if (requested.empty())
    GTEST_SKIP() << "selected in a separate process by the resource check";
  const auto size = std::stoull(requested) * 1024 * 1024;
  ASSERT_LE(size, 256ULL * 1024 * 1024);
  const auto tree = temporary_.path() / "memory-tree";
  fs::create_directory(tree);
  const auto source = tree / "data";
  std::ofstream file(source, std::ios::binary);
  const std::string chunk(128 * 1024, 'm');
  for (std::uint64_t offset = 0; offset < size; offset += chunk.size())
    file.write(chunk.data(), std::min<std::uint64_t>(chunk.size(), size - offset));
  file.close();
  const auto begin = std::chrono::steady_clock::now();
  S3StorageBackend storage(Config());
  const auto plan = storage.UploadCheckpoint({tree, {{"data", Destination("memory", requested)}}});
  ASSERT_EQ(plan.files.size(), 1);
  EXPECT_EQ(plan.files.front().size_bytes, size);
  EXPECT_EQ(plan.files.front().expected_sha256, utils::ComputeFileSha256(source));
  struct rusage usage {};
  ASSERT_EQ(getrusage(RUSAGE_SELF, &usage), 0);
  const auto seconds = std::chrono::duration<double>(std::chrono::steady_clock::now() - begin).count();
  std::cout << "S3 upload resource check: " << size << " bytes, " << seconds << " s, "
            << size / (1024.0 * 1024.0 * seconds) << " MiB/s, peak RSS " << usage.ru_maxrss << " KiB\n";
  // Fixed overhead allowance for the SDK, TLS, threads and dynamic libraries;
  // this subprocess never starts a restore or maps the uploaded file.
  EXPECT_LT(static_cast<std::uint64_t>(usage.ru_maxrss) * 1024, limits_.buffer_budget + 80 * 1024 * 1024);
}

TEST_F(S3RoundTripTest, MetadataMemoryProbe)
{
  if (Environment("PAGEBROKER_S3_TEST_MEMORY_MIB").empty())
    GTEST_SKIP() << "selected in a separate process by the resource check";
  struct rusage before {}, after {};
  ASSERT_EQ(getrusage(RUSAGE_SELF, &before), 0);
  limits_.buffer_budget = 250 * 1024 * 1024;
  S3Client client(connection_, limits_);
  EXPECT_TRUE(client.Get({bucket_, "fixture/small"}, 1024));
  ASSERT_EQ(getrusage(RUSAGE_SELF, &after), 0);
  EXPECT_LT(after.ru_maxrss - before.ru_maxrss, 64 * 1024);
}
}  // namespace
}  // namespace snapshot::pagebroker
