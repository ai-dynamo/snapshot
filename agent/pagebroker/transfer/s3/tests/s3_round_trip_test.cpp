// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_client.hpp"

#include <gtest/gtest.h>
#include <sys/resource.h>
#include <sys/stat.h>

#include <aws/core/auth/AWSCredentialsProvider.h>
#include <aws/core/utils/threading/Executor.h>
#include <aws/s3-crt/S3CrtClient.h>
#include <aws/s3-crt/model/HeadObjectRequest.h>
#include <aws/s3-crt/model/GetObjectRequest.h>

#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <chrono>
#include <cstdlib>
#include <fstream>
#include <future>
#include <functional>
#include <mutex>
#include <thread>
#include <iostream>
#include <vector>

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"
#include "transfer/filesystem/filesystem_storage.hpp"
#include "transfer/s3/s3_storage_backend.hpp"
#include "tests/temporary_directory.hpp"
#include "tests/file_contents.hpp"

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
    limits_.part_size = 8 * 1024 * 1024;
    limits_.target_gbps = 2;
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

  S3Config Config(const std::string& scenario) const
  {
    S3Config config;
    config.transfer = {connection_, limits_, std::chrono::seconds(30)};
    config.store_id = "test-store";
    config.bucket = bucket_;
    config.prefix = "roundtrip/" + scenario;
    return config;
  }

  PublishedArtifact Artifact(char id = 'a') const
  {
    PublishedArtifact artifact;
    artifact.set_store_id("test-store");
    artifact.set_artifact_handle(std::string(64, id));
    return artifact;
  }

  // Exercise the same inspection, publication and index read used by the broker.
  RestorePlan PublishTree(S3StorageBackend& storage, const Path& source, char id = 'a') const
  {
    CheckpointPublication publication{Artifact(id), {}, false};
    auto plan = storage.InspectCheckpoint(source, publication.artifact, {});
    storage.PublishCheckpoint(source, std::move(plan), publication, TransferMode::Staged, {});
    EXPECT_TRUE(publication.published);
    EXPECT_FALSE(publication.pending_index.empty());
    return storage.LoadRestorePlan(publication.artifact, false, {});
  }

  Path SingleFileTree(const std::string& name, bool large = false)
  {
    const auto source = temporary_.path() / name;
    fs::create_directory(source);
    fs::copy_file(fixture_ / (large ? "nested/large" : "small"), source / "data");
    std::ofstream(source / "manifest.yaml") << "version: 1\n";
    return source;
  }

  RestorePlan FilePlan(const S3UploadResult& result) const
  {
    RestorePlan plan;
    plan.root_permissions = fs::perms::owner_all;
    plan.files.push_back({"s3://" + result.destination.bucket + "/" + result.destination.key,
                          "data", result.size_bytes, fs::perms::owner_read});
    return plan;
  }

  void VerifyFile(const S3UploadResult& result, const Path& source, const std::string& name)
  {
    ASSERT_EQ(result.size_bytes, fs::file_size(source));
    ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
    const auto destination = temporary_.path() / name;
    restore_reader.Stage(FilePlan(result), destination);
    EXPECT_TRUE(test::FilesEqual(source, destination / "data"));
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
    Aws::S3Crt::S3CrtClientConfiguration config(Aws::Client::ClientConfigurationInitValues{true});
    config.region = connection_.region;
    const auto scheme_end = connection_.endpoint.find("://");
    config.scheme = connection_.endpoint.starts_with("https://") ? Aws::Http::Scheme::HTTPS : Aws::Http::Scheme::HTTP;
    config.endpointOverride = connection_.endpoint.substr(scheme_end + 3);
    config.useVirtualAddressing = false;
    config.caFile = connection_.ca_file;
    config.httpRequestTimeoutMs = 12000;
    Aws::S3Crt::S3CrtClient client(Aws::Auth::AWSCredentials(connection_.access_key_id, connection_.secret_access_key), config);
    Aws::S3Crt::Model::HeadObjectRequest request;
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
  S3StorageBackend storage(Config("tree"));
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = temporary_.path() / "tree-source";
  fs::copy(fixture_, source, fs::copy_options::recursive);
  std::ofstream(source / "manifest.yaml") << "version: 1\n";
  const auto plan = PublishTree(storage, source);
  EXPECT_EQ(plan.root_permissions, fs::status(source).permissions());
  std::size_t source_count = 0;
  for (const auto& entry : fs::recursive_directory_iterator(source)) {
    ++source_count;
    const auto relative = entry.path().lexically_relative(source);
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
      EXPECT_EQ(file->source_locator, "s3://" + bucket_ + "/roundtrip/tree/" + Artifact().artifact_handle() + "/data/" + relative.generic_string());
      EXPECT_EQ(file->size_bytes, entry.file_size());
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
    EXPECT_EQ(fs::file_size(destination / file.relative_path), fs::file_size(source / file.relative_path));
    EXPECT_TRUE(test::FilesEqual(source / file.relative_path, destination / file.relative_path));
    EXPECT_EQ(fs::status(destination / file.relative_path).permissions(), file.permissions);
  }
}

// Deliberately run cancellation after the synchronous request has finished.
// This exercises the pinned SDK's ownership of queued tasks, including rejection.
class DeferredCancellationExecutor : public Aws::Utils::Threading::Executor {
 public:
  explicit DeferredCancellationExecutor(bool accept) : accept_(accept) {}

  std::size_t Drain()
  {
    std::vector<std::function<void()>> tasks;
    {
      std::lock_guard lock(mutex_);
      tasks.swap(tasks_);
    }
    for (auto& task : tasks)
      task();
    return tasks.size();
  }

  bool WaitForSubmission()
  {
    std::unique_lock lock(mutex_);
    return changed_.wait_for(lock, std::chrono::seconds(5), [&] { return submitted > 0; });
  }

  std::atomic<unsigned> submitted{0};

 protected:
  bool SubmitToThread(std::function<void()>&& task) override
  {
    std::lock_guard lock(mutex_);
    ++submitted;
    if (accept_)
      tasks_.push_back(std::move(task));
    changed_.notify_all();
    return accept_;
  }

 private:
  bool accept_;
  std::mutex mutex_;
  std::condition_variable changed_;
  std::vector<std::function<void()>> tasks_;
};

TEST_F(S3RoundTripTest, CancellationTaskOutlivesCompletedRequest)
{
  S3Client uploader(connection_, limits_);  // Holds the SDK runtime for both clients.
  const auto object = Destination("delayed-cancellation");
  uploader.Put(object, "{}", 2);
  for (bool accept : {true, false}) {
    auto executor = std::make_shared<DeferredCancellationExecutor>(accept);
    Aws::S3Crt::S3CrtClientConfiguration config(Aws::Client::ClientConfigurationInitValues{true});
    config.region = connection_.region;
    config.scheme = connection_.endpoint.starts_with("https://") ? Aws::Http::Scheme::HTTPS : Aws::Http::Scheme::HTTP;
    config.endpointOverride = connection_.endpoint.substr(connection_.endpoint.find("://") + 3);
    config.useVirtualAddressing = false;
    config.caFile = connection_.ca_file;
    config.httpRequestTimeoutMs = 3000;
    config.executor = executor;
    Aws::S3Crt::S3CrtClient client(Aws::Auth::AWSCredentials(connection_.access_key_id, connection_.secret_access_key), config);
    std::atomic<bool> received{false};
    Aws::S3Crt::Model::GetObjectRequest request;
    request.WithBucket(object.bucket).WithKey(object.key);
    request.SetDataReceivedEventHandler([&](const Aws::Http::HttpRequest*, const Aws::Http::HttpResponse*, long long) {
      received = true;
    });
    request.SetContinueRequestHandler([&](const Aws::Http::HttpRequest*) { return !received.load(); });
    // Completion releases the original reference while cancellation is queued.
    // GetObject itself waits for request destruction, so it runs on another thread.
    auto result = std::async(std::launch::async, [&] { return client.GetObject(request).IsSuccess(); });
    EXPECT_TRUE(executor->WaitForSubmission());
    std::this_thread::sleep_for(std::chrono::milliseconds(100));
    const auto drained = executor->Drain();
    EXPECT_TRUE(result.get());
    EXPECT_TRUE(received);
    EXPECT_GT(executor->submitted, 0U);
    EXPECT_EQ(drained, accept ? executor->submitted.load() : 0U);
  }
}

TEST_F(S3RoundTripTest, AppliesRetryLimitToEveryMetadataOperation)
{
  for (unsigned retries : {0U, 2U}) {
    limits_.request_retries = retries;
    S3Client client(connection_, limits_);
    const auto suffix = std::to_string(retries);
    EXPECT_THROW(client.Head(Destination("metadata-head-" + suffix)), S3Error);
    const auto object = Destination("metadata-get-" + suffix);
    client.Put(object, "{}", 2);
    EXPECT_THROW(client.Get(object, 2), S3Error);
    EXPECT_THROW(client.Put(Destination("metadata-put-" + suffix), "{}", 2), S3Error);
  }
  // The fixture independently checks the exact number of HTTP attempts for
  // both the SDK metadata path and CRT, including the initial zero-retry call.
}

TEST_F(S3RoundTripTest, BoundsStalledMetadataRequests)
{
  limits_.request_retries = 0;
  limits_.request_timeout = std::chrono::milliseconds(200);
  S3Client client(connection_, limits_);
  const auto started = std::chrono::steady_clock::now();
  EXPECT_THROW(client.Head(Destination("metadata-stalled")), S3Error);
  EXPECT_LT(std::chrono::steady_clock::now() - started, std::chrono::milliseconds(1500));
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

TEST_F(S3RoundTripTest, ReportsUncertainMetadataWriteAfterLostResponse)
{
  limits_.request_retries = 0;
  S3Client client(connection_, limits_);
  const auto object = Destination("lost-metadata-put");
  try {
    client.Put(object, "{}", 2);
    FAIL() << "lost metadata response unexpectedly succeeded";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.primary.operation, "PutObject");
    EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
    EXPECT_EQ(error.cleanup_state, S3CleanupState::NOT_NEEDED);
    EXPECT_EQ(error.destination.bucket, object.bucket);
    EXPECT_EQ(error.destination.key, object.key);
  }
  EXPECT_EQ(client.Get(object, 2), "{}");
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
  S3StorageBackend storage(Config("preflight"));
  const auto source = SingleFileTree("preflight-source");
  std::ofstream(source / "last") << "last file";
  const auto artifact = Artifact();
  EXPECT_NO_THROW(storage.InspectCheckpoint(source, artifact, {}));

  fs::remove(source / "manifest.yaml");
  EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::invalid_argument);
  std::ofstream(source / "manifest.yaml") << "version: 1\n";
  for (const auto& name : {"bad\nname", "bad\\name"}) {
    std::ofstream(source / name) << "bad path";
    EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::invalid_argument);
    fs::remove(source / name);
  }
  const auto deep = source / std::string(250, 'a') / std::string(250, 'b');
  fs::create_directories(deep);
  std::ofstream(deep / "overlong-path") << "bad path";
  EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::invalid_argument);
  fs::remove_all(source / std::string(250, 'a'));
  for (const auto& prefix : {std::string("bad\nkey"), std::string("bad\0key", 7), std::string(1024, 'x')}) {
    auto config = Config("preflight");
    config.prefix = prefix;
    S3StorageBackend invalid(config);
    EXPECT_THROW(invalid.InspectCheckpoint(source, artifact, {}), std::invalid_argument);
  }
  auto config = Config("preflight");
  config.bucket = "INVALID";
  EXPECT_THROW(S3StorageBackend{config}, std::invalid_argument);

  fs::create_symlink(source / "data", source / "link");
  EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::runtime_error);
  fs::remove(source / "link");
  const auto link = temporary_.path() / "linked-root";
  fs::create_directory_symlink(source, link);
  EXPECT_THROW(storage.InspectCheckpoint(link / "", artifact, {}), std::invalid_argument);
  ASSERT_EQ(mkfifo((source / "fifo").c_str(), 0600), 0);
  EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::runtime_error);
  fs::remove(source / "fifo");

  // A sparse last file exercises the part-count limit without allocating it.
  fs::resize_file(source / "last", limits_.part_size * 10000 + 1);
  EXPECT_THROW(storage.InspectCheckpoint(source, artifact, {}), std::invalid_argument);
  fs::resize_file(source / "last", 0);
  // The host asserts no request at all used the preflight prefix.
}

TEST_F(S3RoundTripTest, RestoresAnEmptyDirectoryTreeWithoutObjects)
{
  const auto source = temporary_.path() / "empty-source";
  fs::create_directories(source / "nested/empty");
  fs::permissions(source / "nested/empty", fs::perms::owner_read | fs::perms::owner_exec);
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  // Empty plans are supported by the native reader; published artifacts require a manifest.
  const auto plan = filesystem_storage::BuildRestorePlan(source);
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
  S3StorageBackend storage(Config("engine-partial"));
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  try {
    PublishTree(storage, source);
    FAIL() << "second object should be denied";
  }
  catch (const S3Error& error) {
    EXPECT_EQ(error.destination.key, Destination("engine-partial", Artifact().artifact_handle() + "/data/last").key);
    EXPECT_EQ(error.primary.operation, "PutObject");
    EXPECT_EQ(error.primary.http_status, 403);
    EXPECT_EQ(error.cleanup_state, S3CleanupState::NOT_NEEDED);
  }
  EXPECT_TRUE(Barrier("engine-partial", Artifact().artifact_handle() + "/data/data"));
  EXPECT_FALSE(storage.CheckpointExists(Artifact(), {}));
  const auto plan = PublishTree(storage, source, 'b');
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
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  VerifyFile(uploader.UploadFile(fixture_ / "small", Destination("after-failure")), fixture_ / "small", "after-failure");
}

TEST_F(S3RoundTripTest, PreservesPrimaryErrorWhenAbortFails)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "abort-failure");
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
}

TEST_F(S3RoundTripTest, DoesNotClaimCleanupItCannotVerify)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "verify-failure");
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
}

TEST_F(S3RoundTripTest, RejectsSourceTruncationDuringUpload)
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
  EXPECT_EQ(error.primary.code, "SourceChanged");
  EXPECT_NE(error.remote_state, S3RemoteState::NO_WRITES_ISSUED);
}

TEST_F(S3RoundTripTest, RejectsErrorEmbeddedInSuccessfulHttpResponse)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "complete-error");
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
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

TEST_F(S3RoundTripTest, ReportsLostInitiationWithoutClaimingCleanup)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "lost-create");
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
}

TEST_F(S3RoundTripTest, RejectsTruncatedReadAndKeepsSessionUsable)
{
  S3StorageBackend storage(Config("truncated-read"));
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = SingleFileTree("truncation-source");
  const auto good = PublishTree(storage, source, 'b');
  auto truncated = good;
  ++truncated.files.front().size_bytes;
  EXPECT_THROW(restore_reader.Stage(truncated, temporary_.path() / "truncated"), std::runtime_error);
  EXPECT_EQ(fs::status(temporary_.path() / "truncated/data").permissions(), fs::perms::owner_read | fs::perms::owner_write);
  EXPECT_NO_THROW(restore_reader.Stage(good, temporary_.path() / "read-recovered"));
  EXPECT_TRUE(test::FilesEqual(source / "data", temporary_.path() / "read-recovered/data"));
}

TEST_F(S3RoundTripTest, BoundsConcurrentUploadsAndIsolatesFailure)
{
  S3StorageBackend storage(Config("bounded"));
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto source = SingleFileTree("concurrent-source", true);
  const auto small = SingleFileTree("concurrent-small");
  fs::rename(small / "data", small / "bad");
  std::vector<std::future<RestorePlan>> uploads;
  for (unsigned i = 0; i < 6; ++i)
    uploads.push_back(std::async(std::launch::async, [&, i] {
      return PublishTree(storage, source, 'a' + i);
    }));
  auto failing = std::async(std::launch::async, [&] {
    EXPECT_THROW(PublishTree(storage, small, '0'), S3Error);
  });
  std::vector<std::future<void>> restores;
  for (unsigned i = 0; i < uploads.size(); ++i) {
    const auto destination = temporary_.path() / ("concurrent-" + std::to_string(i));
    const auto plan = uploads[i].get();
    restores.push_back(std::async(std::launch::async, [&, plan, destination] {
      restore_reader.Stage(plan, destination);
      EXPECT_TRUE(test::FilesEqual(source / "data", destination / "data"));
    }));
  }
  failing.get();
  for (auto& restore : restores)
    EXPECT_NO_THROW(restore.get());
  // The host independently checks file admission; CRT schedules the HTTP parts.
}

TEST_F(S3RoundTripTest, RestoreContinuesWhileUploadIsBlocked)
{
  limits_.request_timeout = std::chrono::seconds(12);
  S3StorageBackend storage(Config("engine-overlap"));
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  const auto small = SingleFileTree("overlap-small");
  const auto large = SingleFileTree("overlap-large", true);
  const auto seed = PublishTree(storage, small);
  auto upload = std::async(std::launch::async, [&] {
    return PublishTree(storage, large, 'b');
  });
  EXPECT_TRUE(Barrier("wait", "engine-overlap/" + Artifact('b').artifact_handle() + "/data"));
  auto restore = std::async(std::launch::async, [&] {
    restore_reader.Stage(seed, temporary_.path() / "overlap-restored");
  });
  EXPECT_EQ(restore.wait_for(std::chrono::seconds(3)), std::future_status::ready);
  EXPECT_TRUE(Barrier("release", "engine-overlap/" + Artifact('b').artifact_handle() + "/data"));
  EXPECT_NO_THROW(restore.get());
  EXPECT_NO_THROW(upload.get());
}

TEST_F(S3RoundTripTest, DeadlineDrainsAndReleasesResources)
{
  limits_.operation_timeout = std::chrono::milliseconds(150);
  limits_.request_timeout = std::chrono::milliseconds(300);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  Cancellation parent;
  const auto begin = std::chrono::steady_clock::now();
  S3Error error({"test", "UnexpectedSuccess"});
  try {
    uploader.UploadFile(fixture_ / "small", Destination("stalled"), parent);
    ADD_FAILURE() << "upload unexpectedly succeeded";
  }
  catch (const S3Error& failure) {
    error = failure;
  }
  EXPECT_EQ(error.primary.code, "DeadlineExceeded");
  EXPECT_LT(std::chrono::steady_clock::now() - begin, std::chrono::seconds(10));
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-deadline"), parent));
}

TEST_F(S3RoundTripTest, MultipartDeadlineDrainsBeforeAborting)
{
  limits_.operation_timeout = std::chrono::milliseconds(500);
  limits_.request_timeout = std::chrono::milliseconds(700);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "multipart-deadline");
  EXPECT_NE(error.primary.code, "UnexpectedSuccess");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-multipart-deadline")));
}

TEST_F(S3RoundTripTest, ZeroRetriesDisablesCrtDefaults)
{
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "no-retry", false);
  EXPECT_EQ(error.primary.operation, "PutObject");
  EXPECT_FALSE(error.primary.code.empty());
  EXPECT_EQ(error.remote_state, S3RemoteState::UNKNOWN);
  // CRT can report a transport failure instead of the final HTTP response.
  // The fixture independently requires exactly one request at the server.
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-no-retry")));
}

TEST_F(S3RoundTripTest, CancellationDrainsAndLeavesClientUsable)
{
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  Cancellation control;
  auto upload = std::async(std::launch::async, [&] {
    try {
      uploader.UploadFile(fixture_ / "nested/large", Destination("cancel-upload"), control);
      return S3Error({"test", "UnexpectedSuccess"});
    }
    catch (const S3Error& error) {
      return error;
    }
  });
  EXPECT_TRUE(Barrier("wait", "cancel-upload"));
  control.Cancel();
  EXPECT_TRUE(Barrier("release", "cancel-upload"));
  const auto error = upload.get();
  EXPECT_EQ(error.primary.code, "Cancelled");
  EXPECT_EQ(error.cleanup_state, S3CleanupState::UNCONFIRMED);
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-cancel")));
}

TEST_F(S3RoundTripTest, CoexistsWithReaderAndMultipleUploaderLifetimes)
{
  ModelStreamerRestore reader(ReaderOptions(), std::chrono::seconds(30));
  RestorePlan reader_fixture;
  reader_fixture.files.push_back({"s3://" + bucket_ + "/fixture/small", "data", fs::file_size(fixture_ / "small"),
                                  fs::perms::owner_read});
  reader.Stage(reader_fixture, temporary_.path() / "before");
  {
    S3Client first(connection_, limits_);
    {
      auto provider = connection_;
      provider.access_key_id.clear();
      provider.secret_access_key.clear();
      auto config = Config("provider");
      config.transfer.connection = provider;
      S3StorageBackend second(config);
      ModelStreamerRestore provider_reader({provider.region, provider.endpoint}, std::chrono::seconds(30));
      const auto source = SingleFileTree("provider-source");
      auto reading = std::async(std::launch::async, [&] { reader.Stage(reader_fixture, temporary_.path() / "during"); });
      const auto plan = PublishTree(second, source);
      EXPECT_NO_THROW(provider_reader.Stage(plan, temporary_.path() / "provider-restored"));
      EXPECT_NO_THROW(reading.get());
    }
    EXPECT_NO_THROW(first.UploadFile(fixture_ / "small", Destination("after-second")));
  }
  EXPECT_NO_THROW(reader.Stage(reader_fixture, temporary_.path() / "after"));
}
TEST_F(S3RoundTripTest, CancellationWakesUploadWaitingForSlot)
{
  limits_.active_files = 1;
  limits_.operation_timeout = std::chrono::seconds(20);
  S3Client uploader(connection_, limits_);
  auto owner = std::async(std::launch::async, [&] {
    uploader.UploadFile(fixture_ / "small", Destination("slot-owner"));
  });
  const bool held = Barrier("wait", "slot-owner");
  EXPECT_TRUE(held);

  Cancellation control;
  std::promise<void> started;
  auto started_future = started.get_future();
  auto waiter = std::async(std::launch::async, [&] {
    started.set_value();
    try {
      uploader.UploadFile(fixture_ / "missing", Destination("slot-waiter"), control);
      return S3Error({"test", "UnexpectedSuccess"});
    }
    catch (const S3Error& error) {
      return error;
    }
  });
  started_future.wait();
  EXPECT_EQ(waiter.wait_for(std::chrono::milliseconds(100)), std::future_status::timeout);
  control.Cancel();
  const auto stopped = waiter.wait_for(std::chrono::seconds(3));
  EXPECT_EQ(stopped, std::future_status::ready);
  // Always release the owner, including on a failed assertion, before joining either future.
  EXPECT_TRUE(Barrier("release", "slot-owner"));
  const auto error = waiter.get();
  EXPECT_EQ(error.primary.code, "Cancelled");
  EXPECT_EQ(error.remote_state, S3RemoteState::NO_WRITES_ISSUED);
  EXPECT_NO_THROW(owner.get());
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-slot-cancel")));
}

TEST_F(S3RoundTripTest, RejectsUntrustedTLS)
{
  if (Environment("PAGEBROKER_S3_TEST_TLS_REJECT") != "1")
    GTEST_SKIP() << "run in a separate process without the fixture CA";
  ASSERT_TRUE(connection_.endpoint.starts_with("https://"));
  limits_.request_retries = 0;
  S3Client client(connection_, limits_);
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  EXPECT_THROW(client.UploadFile(fixture_ / "small", Destination("untrusted")), S3Error);
  RestorePlan plan;
  plan.files.push_back({"s3://" + bucket_ + "/fixture/small", "data", fs::file_size(fixture_ / "small"),
                        fs::perms::owner_read});
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
  S3Client client(connection_, limits_);
  const auto uploaded = client.UploadFile(source, Destination("memory", requested));
  EXPECT_EQ(uploaded.size_bytes, size);
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
  limits_.buffer_budget = 2ULL * 1024 * 1024 * 1024;
  S3Client client(connection_, limits_);
  EXPECT_TRUE(client.Get({bucket_, "fixture/small"}, 1024));
  ASSERT_EQ(getrusage(RUSAGE_SELF, &after), 0);
  EXPECT_LT(after.ru_maxrss - before.ru_maxrss, 64 * 1024);
}
}  // namespace
}  // namespace snapshot::pagebroker
