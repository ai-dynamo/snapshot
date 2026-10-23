// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_client.hpp"

#include <gtest/gtest.h>
#include <sys/resource.h>
#include <sys/stat.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdlib>
#include <fstream>
#include <future>
#include <iostream>
#include <set>
#include <thread>
#include <unistd.h>
#include <vector>

#include "tests/file_contents.hpp"
#include "tests/temporary_directory.hpp"
#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
Cancellation
Deadline(std::chrono::milliseconds duration = std::chrono::seconds(30))
{
  return Cancellation(Cancellation::Clock::now() + duration);
}

std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

// Sample parent plus every direct child, including children forked by other
// threads. RUSAGE_SELF excludes rclone and RUSAGE_CHILDREN is not an aggregate
// of simultaneously active children. Isolated resource probes avoid old peaks.
class ProcessMemory {
 public:
  ProcessMemory()
      : sampler_([this](std::stop_token stop) {
          while (!stop.stop_requested()) {
            std::set<unsigned> children;
            for (const auto& task : fs::directory_iterator("/proc/self/task")) {
              std::ifstream list(task.path() / "children");
              unsigned pid;
              while (list >> pid)
                children.insert(pid);
            }
            auto rss = Rss("/proc/self/statm");
            for (auto pid : children)
              rss += Rss("/proc/" + std::to_string(pid) + "/statm");
            peak_.store(std::max(peak_.load(), rss));
            std::this_thread::sleep_for(std::chrono::milliseconds(5));
          }
        })
  {
  }
  std::uint64_t Peak() const { return peak_.load(); }

 private:
  static std::uint64_t Rss(const fs::path& path)
  {
    std::ifstream stat(path);
    std::uint64_t virtual_pages = 0, resident_pages = 0;
    stat >> virtual_pages >> resident_pages;
    return resident_pages * sysconf(_SC_PAGESIZE);
  }
  std::atomic<std::uint64_t> peak_{0};
  std::jthread sampler_;
};

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
    limits_.active_files = 2;
    limits_.request_retries = 2;
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

  Path SingleFileTree(const std::string& name, bool large = false)
  {
    const auto source = temporary_.path() / name;
    fs::create_directory(source);
    fs::copy_file(fixture_ / (large ? "nested/large" : "small"), source / "data");
    return source;
  }

  CheckpointInventory FileInventory(std::uint64_t size, const S3ObjectLocation& object) const
  {
    CheckpointInventory inventory;
    inventory.root_permissions = fs::perms::owner_all;
    inventory.files.push_back({"s3://" + object.bucket + "/" + object.key, "data", size, fs::perms::owner_read});
    return inventory;
  }

  void VerifyFile(std::uint64_t size, const S3ObjectLocation& object, const Path& source, const std::string& name)
  {
    ASSERT_EQ(size, fs::file_size(source));
    ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
    const auto destination = temporary_.path() / name;
    restore_reader.Stage(FileInventory(size, object), destination);
    EXPECT_TRUE(test::FilesEqual(source, destination / "data"));
    EXPECT_EQ(fs::status(destination / "data").permissions(), fs::perms::owner_read);
  }

  S3Error FailedUpload(S3Client& uploader, const std::string& scenario, bool large = true)
  {
    try {
      uploader.UploadFile(fixture_ / (large ? "nested/large" : "small"), Destination(scenario), Deadline());
      ADD_FAILURE() << "upload unexpectedly succeeded: " << scenario;
    } catch (const S3Error& error) {
      return error;
    }
    return S3Error();
  }

  // Test-only metadata requests synchronize with the local fault service.
  // The caller keeps an uploader (or an engine that uploaded) alive here.
  bool Barrier(const std::string& operation, const std::string& scenario)
  {
    auto limits = limits_;
    limits.request_timeout = std::chrono::seconds(12);
    S3Client client(connection_, limits);
    return client.Head({bucket_, "roundtrip/" + operation + "/" + scenario}, Deadline()).has_value();
  }

  test::TemporaryDirectory temporary_;
  Path fixture_;
  std::string bucket_;
  S3Connection connection_;
  S3Limits limits_;
};

TEST_F(S3RoundTripTest, CancelsConcurrentReadsWithoutCrashing)
{
  S3Client client(connection_, limits_);
  const auto object = Destination("concurrent-cancellation");
  client.Put(object, "{}", 2, Deadline());
  std::vector<std::future<void>> requests;
  for (unsigned i = 0; i < 16; ++i) {
    requests.push_back(std::async(std::launch::async, [&] {
      Cancellation cancellation(Cancellation::Clock::now() + std::chrono::milliseconds(50));
      try {
        EXPECT_EQ(client.Get(object, 2, cancellation), "{}");
      } catch (const CancellationError& error) {
        EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
      }
    }));
  }
  for (auto& request : requests)
    request.get();
  EXPECT_EQ(client.Get(object, 2, Deadline()), "{}");
}

TEST_F(S3RoundTripTest, AppliesRetryLimitToEveryMetadataOperation)
{
  for (unsigned retries : {0U, 2U}) {
    limits_.request_retries = retries;
    S3Client client(connection_, limits_);
    const auto suffix = std::to_string(retries);
    EXPECT_THROW(client.Head(Destination("metadata-head-" + suffix), Deadline()), S3Error);
    const auto object = Destination("metadata-get-" + suffix);
    client.Put(object, "{}", 2, Deadline());
    EXPECT_THROW(client.Get(object, 2, Deadline()), S3Error);
    EXPECT_THROW(client.Put(Destination("metadata-put-" + suffix), "{}", 2, Deadline()), S3Error);
  }
  // The fixture independently checks the exact number of HTTP attempts for
  // all metadata operations, including the initial zero-retry call.
}

TEST_F(S3RoundTripTest, BoundsStalledMetadataRequests)
{
  limits_.request_retries = 0;
  limits_.request_timeout = std::chrono::milliseconds(200);
  S3Client client(connection_, limits_);
  const auto started = std::chrono::steady_clock::now();
  EXPECT_THROW(client.Head(Destination("metadata-stalled"), Deadline()), S3Error);
  EXPECT_LT(std::chrono::steady_clock::now() - started, std::chrono::milliseconds(1500));
}

TEST_F(S3RoundTripTest, PreservesLiteralObjectKeysAndNeverListsForMetadata)
{
  S3Client client(connection_, limits_);
  for (const auto* key : {"space name", "quotes'\"", "brackets[*]?", "$(false);`false`", "unicode-α"}) {
    const auto object = Destination("literal", key);
    client.Put(object, "literal payload", 64, Deadline());
    EXPECT_EQ(client.Head(object, Deadline()), 15U);
    EXPECT_EQ(client.Get(object, 64, Deadline()), "literal payload");
  }
}

TEST_F(S3RoundTripTest, PublishesAndReadsBoundedMetadata)
{
  S3Client client(connection_, limits_);
  const S3ObjectLocation object{bucket_, "metadata/index.json"};
  EXPECT_FALSE(client.Head(object, Deadline()));
  EXPECT_FALSE(client.Get(object, 1024, Deadline()));
  client.Put(object, "{}", 2, Deadline());
  EXPECT_EQ(client.Head(object, Deadline()), 2U);
  EXPECT_EQ(client.Get(object, 2, Deadline()), "{}");
  EXPECT_THROW(client.Get(object, 1, Deadline()), S3ResponseError);
  try {
    client.Put(object, "different", 1024, Deadline());
    FAIL() << "conditional metadata write overwrote an object";
  } catch (const S3Error& error) {
    EXPECT_EQ(error.http_status, 412);
  }
  EXPECT_EQ(client.Get(object, 2, Deadline()), "{}");
  const auto empty = Destination("metadata", "empty-index");
  client.Put(empty, "", 0, Deadline());
  EXPECT_EQ(client.Get(empty, 0, Deadline()), "");
}

TEST_F(S3RoundTripTest, ReportsUncertainMetadataWriteAfterLostResponse)
{
  limits_.request_retries = 0;
  S3Client client(connection_, limits_);
  const auto object = Destination("lost-metadata-put");
  EXPECT_THROW(client.Put(object, "{}", 2, Deadline()), S3Error);
  EXPECT_EQ(client.Get(object, 2, Deadline()), "{}");
}

TEST_F(S3RoundTripTest, RejectsMetadataSizeChangesAfterHead)
{
  limits_.active_files = 1; // Follow-up HEAD must not retain the GET permit.
  S3Client client(connection_, limits_);
  for (const auto* scenario : {"metadata-grow", "metadata-shrink"}) {
    SCOPED_TRACE(scenario);
    const auto object = Destination(scenario);
    client.Put(object, "{}", 2, Deadline());
    EXPECT_THROW(client.Get(object, 16, Deadline()), std::runtime_error);
  }
}

TEST_F(S3RoundTripTest, CancelsMetadataAndPreservesCompletedWrites)
{
  S3Client client(connection_, limits_);
  for (const auto* operation : {"head", "get", "put"}) {
    SCOPED_TRACE(operation);
    const auto scenario = std::string("metadata-cancel-") + operation;
    const auto object = Destination(scenario);
    if (std::string(operation) != "put")
      client.Put(object, "{}", 2, Deadline());
    Cancellation cancellation = Deadline();
    auto pending = std::async(std::launch::async, [&] {
      try {
        if (std::string(operation) == "head")
          client.Head(object, cancellation);
        else if (std::string(operation) == "get")
          client.Get(object, 2, cancellation);
        else
          client.Put(object, "{}", 2, cancellation);
        ADD_FAILURE() << "cancelled metadata request succeeded";
        return CancellationError(CancellationError::Reason::CANCELLED);
      } catch (const CancellationError& error) {
        return error;
      }
    });
    EXPECT_TRUE(Barrier("wait", scenario));
    cancellation.Cancel();
    EXPECT_TRUE(Barrier("release", scenario));
    const auto error = pending.get();
    EXPECT_EQ(error.reason, CancellationError::Reason::CANCELLED);

    if (std::string(operation) == "put") {
      // The service completed the write before withholding its response. Process
      // cancellation may still make the remote outcome unknown to the caller.

      EXPECT_EQ(client.Get(object, 2, Deadline()), "{}");
    }
  }
}

TEST_F(S3RoundTripTest, IndependentClientsRemainUsable)
{
  S3Client client(connection_, limits_);
  {
    S3Client other(connection_, limits_);
    EXPECT_TRUE(other.Get({bucket_, "fixture/small"}, 1024, Deadline()));
  }
  EXPECT_TRUE(client.Get({bucket_, "fixture/small"}, 1024, Deadline()));
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
    VerifyFile(uploader.UploadFile(path, Destination("boundary", name), Deadline()), Destination("boundary", name),
               path, name);
  }
}

TEST_F(S3RoundTripTest, RetriesTransientErrorsAndRejectsAccessDenial)
{
  S3Client uploader(connection_, limits_);
  VerifyFile(uploader.UploadFile(fixture_ / "small", Destination("retry"), Deadline()), Destination("retry"),
             fixture_ / "small", "retry");
  const auto error = FailedUpload(uploader, "denied", false);

  EXPECT_EQ(error.http_status, 403);
}

TEST_F(S3RoundTripTest, AbortsFailedMultipartAndRemainsUsable)
{
  S3Client uploader(connection_, limits_);
  FailedUpload(uploader, "part-failure");

  VerifyFile(uploader.UploadFile(fixture_ / "small", Destination("after-failure"), Deadline()),
             Destination("after-failure"), fixture_ / "small", "after-failure");
}

TEST_F(S3RoundTripTest, PreservesPrimaryErrorWhenAbortFails)
{
  S3Client uploader(connection_, limits_);
  const auto error = FailedUpload(uploader, "abort-failure");

  EXPECT_EQ(error.http_status, 403);
}

TEST_F(S3RoundTripTest, ReportsFailureWhenAbortCannotBeVerified)
{
  S3Client uploader(connection_, limits_);
  FailedUpload(uploader, "verify-failure");
}

TEST_F(S3RoundTripTest, RejectsSourceTruncationDuringUpload)
{
  S3Client uploader(connection_, limits_);
  const auto source = temporary_.path() / "source";
  fs::copy_file(fixture_ / "nested/large", source);
  auto upload = std::async(std::launch::async, [&] {
    try {
      uploader.UploadFile(source, Destination("source-truncated"), Deadline());
      ADD_FAILURE() << "upload unexpectedly succeeded";
      return S3Error();
    } catch (const S3Error& error) {
      return error;
    }
  });
  EXPECT_TRUE(Barrier("wait", "source-truncated"));
  fs::resize_file(source, 0);
  EXPECT_TRUE(Barrier("release", "source-truncated"));
  upload.get();
}

TEST_F(S3RoundTripTest, RejectsErrorEmbeddedInSuccessfulHttpResponse)
{
  S3Client uploader(connection_, limits_);
  FailedUpload(uploader, "complete-error");
}

TEST_F(S3RoundTripTest, ReportsUncertainCompletionWithoutDeletingObjects)
{
  S3Client uploader(connection_, limits_);
  for (const auto& scenario : {"lost-complete", "lost-put"}) {
    FailedUpload(uploader, scenario, std::string(scenario) == "lost-complete");
  }
  // The host independently verifies both completed objects still exist.
}

TEST_F(S3RoundTripTest, ReportsLostInitiation)
{
  S3Client uploader(connection_, limits_);
  FailedUpload(uploader, "lost-create");
}

TEST_F(S3RoundTripTest, DeadlineDrainsAndReleasesResources)
{
  limits_.request_timeout = std::chrono::milliseconds(300);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  Cancellation parent = Deadline();
  const auto begin = std::chrono::steady_clock::now();
  try {
    uploader.UploadFile(fixture_ / "small", Destination("stalled"),
                        parent.WithDeadline(begin + std::chrono::milliseconds(150)));
    ADD_FAILURE() << "upload unexpectedly succeeded";
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
  }
  EXPECT_LT(std::chrono::steady_clock::now() - begin, std::chrono::seconds(10));
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-deadline"), parent));
}

TEST_F(S3RoundTripTest, MultipartDeadlineDrainsBeforeReturning)
{
  limits_.request_timeout = std::chrono::milliseconds(700);
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  try {
    uploader.UploadFile(fixture_ / "nested/large", Destination("multipart-deadline"),
                        Deadline(std::chrono::milliseconds(500)));
    ADD_FAILURE() << "upload unexpectedly succeeded";
  } catch (const CancellationError& error) {
    EXPECT_EQ(error.reason, CancellationError::Reason::DEADLINE_EXCEEDED);
  }
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-multipart-deadline"), Deadline()));
}

TEST_F(S3RoundTripTest, ZeroRetriesDisablesRetries)
{
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  FailedUpload(uploader, "no-retry", false);

  // rclone can report a transport failure instead of the final HTTP response.
  // The fixture independently requires exactly one request at the server.
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-no-retry"), Deadline()));
}

TEST_F(S3RoundTripTest, CancellationDrainsAndLeavesClientUsable)
{
  limits_.request_retries = 0;
  S3Client uploader(connection_, limits_);
  Cancellation cancellation = Deadline();
  auto upload = std::async(std::launch::async, [&] {
    try {
      uploader.UploadFile(fixture_ / "nested/large", Destination("cancel-upload"), cancellation);
      ADD_FAILURE() << "cancelled operation succeeded";
      return CancellationError(CancellationError::Reason::CANCELLED);
    } catch (const CancellationError& error) {
      return error;
    }
  });
  EXPECT_TRUE(Barrier("wait", "cancel-upload"));
  cancellation.Cancel();
  EXPECT_TRUE(Barrier("release", "cancel-upload"));
  const auto error = upload.get();
  EXPECT_EQ(error.reason, CancellationError::Reason::CANCELLED);

  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-cancel"), Deadline()));
}

TEST_F(S3RoundTripTest, CancellationWakesUploadWaitingForSlot)
{
  limits_.active_files = 1;
  S3Client uploader(connection_, limits_);
  auto owner = std::async(std::launch::async,
                          [&] { uploader.UploadFile(fixture_ / "small", Destination("slot-owner"), Deadline()); });
  const bool held = Barrier("wait", "slot-owner");
  EXPECT_TRUE(held);

  Cancellation cancellation = Deadline();
  std::promise<void> started;
  auto started_future = started.get_future();
  auto waiter = std::async(std::launch::async, [&] {
    started.set_value();
    try {
      uploader.UploadFile(fixture_ / "missing", Destination("slot-waiter"), cancellation);
      ADD_FAILURE() << "cancelled operation succeeded";
      return CancellationError(CancellationError::Reason::CANCELLED);
    } catch (const CancellationError& error) {
      return error;
    }
  });
  started_future.wait();
  EXPECT_EQ(waiter.wait_for(std::chrono::milliseconds(100)), std::future_status::timeout);
  cancellation.Cancel();
  const auto stopped = waiter.wait_for(std::chrono::seconds(3));
  EXPECT_EQ(stopped, std::future_status::ready);
  // Always release the owner, including on a failed assertion, before joining either future.
  EXPECT_TRUE(Barrier("release", "slot-owner"));
  const auto error = waiter.get();
  EXPECT_EQ(error.reason, CancellationError::Reason::CANCELLED);

  EXPECT_NO_THROW(owner.get());
  EXPECT_NO_THROW(uploader.UploadFile(fixture_ / "small", Destination("after-slot-cancel"), Deadline()));
}

TEST_F(S3RoundTripTest, RejectsUntrustedTLS)
{
  if (Environment("PAGEBROKER_S3_TEST_TLS_REJECT") != "1")
    GTEST_SKIP() << "run in a separate process without the fixture CA";
  ASSERT_TRUE(connection_.endpoint.starts_with("https://"));
  limits_.request_retries = 0;
  S3Client client(connection_, limits_);
  ModelStreamerRestore restore_reader(ReaderOptions(), std::chrono::seconds(30));
  EXPECT_THROW(client.UploadFile(fixture_ / "small", Destination("untrusted"), Deadline()), S3Error);
  CheckpointInventory inventory;
  inventory.files.push_back(
      {"s3://" + bucket_ + "/fixture/small", "data", fs::file_size(fixture_ / "small"), fs::perms::owner_read});
  EXPECT_THROW(restore_reader.Stage(inventory, temporary_.path() / "untrusted-restore"), std::runtime_error);
}

TEST_F(S3RoundTripTest, UploadMemoryProbe)
{
  const auto requested = Environment("PAGEBROKER_S3_TEST_MEMORY_MIB");
  if (requested.empty())
    GTEST_SKIP() << "selected in a separate process by the resource check";
  const auto size = std::stoull(requested) * 1024 * 1024;
  ASSERT_LE(size, 512ULL * 1024 * 1024);
  const auto tree = temporary_.path() / "memory-tree";
  fs::create_directory(tree);
  const auto source = tree / "data";
  std::ofstream file(source, std::ios::binary);
  const std::string chunk(128 * 1024, 'm');
  for (std::uint64_t offset = 0; offset < size; offset += chunk.size())
    file.write(chunk.data(), std::min<std::uint64_t>(chunk.size(), size - offset));
  file.close();
  ProcessMemory memory;
  const auto begin = std::chrono::steady_clock::now();
  S3Client client(connection_, limits_);
  const auto uploaded = client.UploadFile(source, Destination("memory", requested), Deadline());
  EXPECT_EQ(uploaded, size);
  const auto seconds = std::chrono::duration<double>(std::chrono::steady_clock::now() - begin).count();
  std::cout << "S3 upload resource check: " << size << " bytes, " << seconds << " s, "
            << size / (1024.0 * 1024.0 * seconds) << " MiB/s, peak RSS " << memory.Peak() / 1024 << " KiB\n";
  // Fixed overhead allowance for the child Go runtime, TLS and threads;
  // this subprocess never starts a restore or maps the uploaded file.
  EXPECT_LT(memory.Peak(), limits_.buffer_budget + 80 * 1024 * 1024);
  // The runner uses 512 MiB to exceed rclone's working set. A 336 MiB RSS bound
  // allows multipart buffering and process overhead but rejects a whole-file copy.
  EXPECT_LT(memory.Peak(), size / 2 + 80 * 1024 * 1024);
}

TEST_F(S3RoundTripTest, ConcurrentUploadMemoryProbe)
{
  if (Environment("PAGEBROKER_S3_TEST_MEMORY_MIB").empty())
    GTEST_SKIP() << "selected in a separate process by the resource check";
  const auto path = temporary_.path() / "concurrent-memory";
  std::ofstream file(path, std::ios::binary);
  const std::string chunk(1024 * 1024, 'm');
  for (unsigned i = 0; i < 256; ++i)
    file.write(chunk.data(), chunk.size());
  file.close();
  limits_.active_files = 2;
  limits_.buffer_budget = 2 * limits_.part_size;
  ProcessMemory memory;
  S3Client client(connection_, limits_);
  auto first = std::async(std::launch::async,
                          [&] { return client.UploadFile(path, Destination("memory", "first"), Deadline()); });
  auto second = std::async(std::launch::async,
                           [&] { return client.UploadFile(path, Destination("memory", "second"), Deadline()); });
  EXPECT_EQ(first.get(), 256ULL * 1024 * 1024);
  EXPECT_EQ(second.get(), 256ULL * 1024 * 1024);
  // Allow runtime overhead for the parent and both children, but not whole-file
  // buffering. The multipart buffers share the configured 16 MiB budget.
  EXPECT_LT(memory.Peak(), limits_.buffer_budget + 3 * 80ULL * 1024 * 1024);
}

TEST_F(S3RoundTripTest, MetadataMemoryProbe)
{
  if (Environment("PAGEBROKER_S3_TEST_MEMORY_MIB").empty())
    GTEST_SKIP() << "selected in a separate process by the resource check";
  ProcessMemory memory;
  limits_.buffer_budget = 2ULL * 1024 * 1024 * 1024;
  S3Client client(connection_, limits_);
  // A generous metadata limit must not allocate that limit for a small object.
  EXPECT_TRUE(client.Get({bucket_, "fixture/small"}, 256 * 1024 * 1024, Deadline()));
  EXPECT_LT(memory.Peak(), 128ULL * 1024 * 1024);
}
} // namespace
} // namespace snapshot::pagebroker
