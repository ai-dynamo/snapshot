// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"

#include <gtest/gtest.h>
#include <sys/resource.h>
#include <sys/stat.h>

#include <chrono>
#include <cstdlib>
#include <future>
#include <iostream>
#include <string>

#include "tests/temporary_directory.hpp"
#include "tests/file_contents.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
std::string
Environment(const char* name)
{
  const char* value = std::getenv(name);
  return value == nullptr ? "" : value;
}

class ModelStreamerS3Test : public ::testing::Test {
 protected:
  void SetUp() override
  {
    const auto bucket = Environment("PAGEBROKER_S3_TEST_BUCKET");
    const auto prefix = Environment("PAGEBROKER_S3_TEST_PREFIX");
    const auto fixture = Environment("PAGEBROKER_S3_TEST_FIXTURE");
    ASSERT_FALSE(bucket.empty()) << "run tests/s3_integration.py to prepare the S3 fixture";
    ASSERT_FALSE(prefix.empty());
    ASSERT_FALSE(fixture.empty());
    options_.region = Environment("AWS_DEFAULT_REGION");
    options_.endpoint = Environment("AWS_ENDPOINT_URL");
    inventory_.root_permissions = fs::status(fixture).permissions();
    for (const auto& entry : fs::recursive_directory_iterator(fixture)) {
      const auto relative = entry.path().lexically_relative(fixture);
      if (entry.is_directory()) {
        inventory_.directories.push_back({relative, entry.status().permissions()});
      } else {
        ASSERT_TRUE(entry.is_regular_file());
        inventory_.files.push_back({"s3://" + bucket + "/" + prefix + relative.generic_string(), relative,
                               entry.file_size(), entry.status().permissions()});
      }
    }
    ASSERT_FALSE(inventory_.files.empty());
  }

  CheckpointInventory NonemptyFile() const
  {
    CheckpointInventory inventory;
    for (const auto& file : inventory_.files) {
      if (file.size_bytes > 0) {
        inventory.files.push_back(file);
        inventory.files.back().relative_path = "data";
        break;
      }
    }
    return inventory;
  }

  void CheckTree(const Path& destination)
  {
    EXPECT_EQ(fs::status(destination).permissions(), inventory_.root_permissions);
    std::size_t entries = 0;
    for ([[maybe_unused]] const auto& entry : fs::recursive_directory_iterator(destination))
      ++entries;
    EXPECT_EQ(entries, inventory_.files.size() + inventory_.directories.size());
    for (const auto& directory : inventory_.directories) {
      EXPECT_TRUE(fs::is_directory(destination / directory.relative_path));
      EXPECT_EQ(fs::status(destination / directory.relative_path).permissions(), directory.permissions);
    }
    for (const auto& file : inventory_.files) {
      const auto path = destination / file.relative_path;
      EXPECT_EQ(fs::file_size(path), file.size_bytes);
      EXPECT_EQ(fs::status(path).permissions(), file.permissions);
      EXPECT_TRUE(test::FilesEqual(Path(Environment("PAGEBROKER_S3_TEST_FIXTURE")) / file.relative_path, path));
    }
  }

  test::TemporaryDirectory root_;
  ModelStreamerSessionOptions options_;
  CheckpointInventory inventory_;
};

TEST_F(ModelStreamerS3Test, EngineReturnsOwnedStaging)
{
  ModelStreamerTransferEngine engine(options_, std::chrono::seconds(30));
  PreparedRestore prepared;
  prepared.inventory = inventory_;
  const auto destination = root_.path() / "engine";
  const auto access = engine.StageRestore(std::move(prepared), destination);
  EXPECT_EQ(access.image_directory, destination);
  EXPECT_EQ(access.staging_directory, destination);
  struct stat expected{}, actual{};
  ASSERT_EQ(stat(destination.c_str(), &expected), 0);
  ASSERT_EQ(fstat(access.source.get(), &actual), 0);
  EXPECT_EQ(expected.st_dev, actual.st_dev);
  EXPECT_EQ(expected.st_ino, actual.st_ino);
  CheckTree(destination);
}

TEST_F(ModelStreamerS3Test, RestoresRestrictivePermissions)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  auto inventory = NonemptyFile();
  const auto source = inventory.files.front().source_locator;
  Path fixture_file;
  for (const auto& file : inventory_.files) {
    if (file.source_locator == source)
      fixture_file = Path(Environment("PAGEBROKER_S3_TEST_FIXTURE")) / file.relative_path;
  }
  ASSERT_FALSE(fixture_file.empty());
  for (const auto mode : {fs::perms::none, fs::perms::owner_read}) {
    SCOPED_TRACE(static_cast<unsigned>(mode));
    inventory.files.front().permissions = mode;
    const auto destination = root_.path() / std::to_string(static_cast<unsigned>(mode));
    ASSERT_NO_THROW(restore.Stage(inventory, destination));
    EXPECT_EQ(fs::status(destination / "data").permissions(), mode);
    fs::permissions(destination / "data", fs::perms::owner_read, fs::perm_options::add);
    EXPECT_TRUE(test::FilesEqual(fixture_file, destination / "data"));
  }
}

TEST_F(ModelStreamerS3Test, RestoresTreeThroughNativePlugin)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  const auto begin = std::chrono::steady_clock::now();
  restore.Stage(inventory_, root_.path() / "restored");
  const double seconds = std::chrono::duration<double>(std::chrono::steady_clock::now() - begin).count();
  CheckTree(root_.path() / "restored");
  uintmax_t bytes = 0;
  for (const auto& file : inventory_.files)
    bytes += file.size_bytes;
  struct rusage usage {};
  ASSERT_EQ(getrusage(RUSAGE_SELF, &usage), 0);
  std::cout << "Native S3 restore: " << bytes << " bytes, " << seconds << " s, "
            << bytes / (1024.0 * 1024.0 * seconds) << " MiB/s, peak RSS " << usage.ru_maxrss << " KiB\n";
}

TEST_F(ModelStreamerS3Test, ReusesCoordinatorAcrossSequentialRestores)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  // Exceed the concurrent submission limit without recreating the coordinator.
  for (unsigned i = 0; i < 12; ++i) {
    const auto destination = root_.path() / std::to_string(i);
    ASSERT_NO_THROW(restore.Stage(inventory_, destination));
    CheckTree(destination);
    fs::remove_all(destination);
  }
}

TEST_F(ModelStreamerS3Test, SupportsConcurrentRestores)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  auto first = std::async(std::launch::async, [&] { restore.Stage(inventory_, root_.path() / "first"); });
  auto second = std::async(std::launch::async, [&] { restore.Stage(inventory_, root_.path() / "second"); });
  EXPECT_NO_THROW(first.get());
  EXPECT_NO_THROW(second.get());
  CheckTree(root_.path() / "first");
  CheckTree(root_.path() / "second");
}

TEST_F(ModelStreamerS3Test, RejectsTruncatedObject)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  auto truncated = NonemptyFile();
  ++truncated.files.front().size_bytes;
  EXPECT_THROW(restore.Stage(truncated, root_.path() / "truncated"), std::runtime_error);
}

TEST_F(ModelStreamerS3Test, MissingObjectFailsAndSessionRemainsUsable)
{
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  auto missing = NonemptyFile();
  missing.files.front().source_locator += "-does-not-exist";
  EXPECT_THROW(restore.Stage(missing, root_.path() / "missing"), std::runtime_error);
  EXPECT_NO_THROW(restore.Stage(NonemptyFile(), root_.path() / "retry"));
}

TEST_F(ModelStreamerS3Test, SupportsExplicitSessionCredentials)
{
  options_.access_key_id = Environment("AWS_ACCESS_KEY_ID");
  options_.secret_access_key = Environment("AWS_SECRET_ACCESS_KEY");
  options_.session_token = Environment("AWS_SESSION_TOKEN");
  ASSERT_FALSE(options_.access_key_id.empty());
  ModelStreamerRestore restore(options_, std::chrono::seconds(30));
  EXPECT_NO_THROW(restore.Stage(NonemptyFile(), root_.path() / "explicit"));
}

TEST_F(ModelStreamerS3Test, UnavailableEndpointFails)
{
  options_.endpoint = "http://127.0.0.1:1";
  ModelStreamerRestore restore(options_, std::chrono::seconds(5));
  EXPECT_THROW(restore.Stage(NonemptyFile(), root_.path() / "unavailable"), std::runtime_error);
}

TEST_F(ModelStreamerS3Test, RejectsUntrustedTLS)
{
  if (Environment("PAGEBROKER_S3_TEST_TLS_REJECT") != "1")
    GTEST_SKIP() << "run in a separate process without the fixture CA";
  ASSERT_TRUE(options_.endpoint.starts_with("https://"));
  ModelStreamerRestore restore(options_, std::chrono::seconds(5));
  EXPECT_THROW(restore.Stage(NonemptyFile(), root_.path() / "untrusted"), std::runtime_error);
}
}  // namespace
}  // namespace snapshot::pagebroker
