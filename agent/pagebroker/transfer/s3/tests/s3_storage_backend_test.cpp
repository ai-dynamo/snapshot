// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_storage_backend.hpp"

#include <gtest/gtest.h>

#include <fstream>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace {
namespace fs = std::filesystem;

TEST(S3StorageBackendTest, BindsIdentityAndVersionBeforeStorageIO)
{
  const auto config = ParseS3Config(R"({"storeId":"test-store","endpoint":"https://unused.invalid","region":"us-east-1","bucket":"test-bucket"})");
  S3StorageBackend backend(config);
  ArtifactTarget target;
  target.set_store_id(config.store_id);
  target.mutable_artifact()->set_artifact_uid("content-1");
  target.mutable_artifact()->set_container_name("main");
  const auto artifact = backend.ResolveTarget(target);
  EXPECT_EQ(artifact.SerializeAsString(), backend.ResolveTarget(target).SerializeAsString());
  EXPECT_EQ(artifact.artifact_handle().size(), 64);
  target.mutable_artifact()->set_container_name("sidecar");
  EXPECT_NE(artifact.artifact_handle(), backend.ResolveTarget(target).artifact_handle());
  target.set_store_id("another-store");
  try {
    backend.ResolveTarget(target);
    FAIL() << "foreign store accepted";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::STORE_MISMATCH);
  }
  auto future = artifact;
  future.set_artifact_format_version("future/v2");
  try {
    backend.LoadRestorePlan(future, false, {});
    FAIL() << "unknown reader accepted";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::UNSUPPORTED_ARTIFACT);
  }
}

TEST(S3StorageBackendTest, PreflightsCompleteCaptureWithoutStorageIO)
{
  test::TemporaryDirectory root;
  const auto config = ParseS3Config(R"({"storeId":"test-store","endpoint":"https://unused.invalid","region":"us-east-1","bucket":"test-bucket"})");
  S3StorageBackend backend(config);
  PublishedArtifact destination;
  destination.set_store_id("test-store");
  destination.set_artifact_format_version(kCheckpointFormat);
  destination.set_artifact_handle(std::string(64, 'a'));
  EXPECT_THROW(backend.InspectCheckpoint(root.path(), destination, {}), std::invalid_argument);
  std::ofstream(root.path() / "manifest.yaml") << "version: 1\n";
  const auto plan = backend.InspectCheckpoint(root.path(), destination, {});
  ASSERT_EQ(plan.files.size(), 1);
  EXPECT_FALSE(plan.files.front().expected_sha256);
  EXPECT_EQ(plan.files.front().source_locator, "s3://test-bucket/" + std::string(64, 'a') + "/data/manifest.yaml");
  fs::create_symlink(root.path() / "manifest.yaml", root.path() / "link");
  EXPECT_THROW(backend.InspectCheckpoint(root.path(), destination, {}), std::runtime_error);
}

TEST(S3StorageBackendTest, IndexedCheckpointsRequireAConfiguredBucket)
{
  auto config = ParseS3Config(R"({"storeId":"test-store","endpoint":"https://unused.invalid","region":"us-east-1","bucket":"test-bucket"})");
  config.bucket.clear();
  S3StorageBackend backend(config);
  PublishedArtifact checkpoint;
  checkpoint.set_artifact_handle(std::string(64, 'a'));
  EXPECT_THROW(backend.ValidateCheckpoint(checkpoint), std::invalid_argument);
}

TEST(S3StorageBackendTest, OwnsConfigurationAndUsesItsPrefix)
{
  test::TemporaryDirectory root;
  std::ofstream(root.path() / "manifest.yaml") << "version: 1\n";
  auto config = ParseS3Config(R"({"storeId":"test-store","endpoint":"https://unused.invalid","region":"us-east-1","bucket":"test-bucket","prefix":"snapshots/nested"})");
  S3StorageBackend backend(config);
  config.bucket = "changed-bucket";
  config.prefix = "changed-prefix";
  config.transfer.connection.region = "changed-region";

  PublishedArtifact destination;
  destination.set_store_id("test-store");
  destination.set_artifact_format_version(kCheckpointFormat);
  destination.set_artifact_handle(std::string(64, 'b'));
  const auto plan = backend.InspectCheckpoint(root.path(), destination, {});
  ASSERT_EQ(plan.files.size(), 1);
  EXPECT_EQ(plan.files.front().source_locator,
            "s3://test-bucket/snapshots/nested/" + std::string(64, 'b') + "/data/manifest.yaml");
  EXPECT_EQ(backend.config().transfer.connection.region, "us-east-1");
}
}  // namespace
}  // namespace snapshot::pagebroker
