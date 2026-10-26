// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_storage_backend.hpp"

#include <gtest/gtest.h>

#include <fstream>
#include <stdexcept>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace {
namespace fs = std::filesystem;

S3Config
TestConfig()
{
  S3Config config;
  config.store_id = "test-store";
  config.bucket = "test-bucket";
  config.transfer.connection.endpoint = "https://unused.invalid";
  config.transfer.connection.region = "us-east-1";
  return config;
}

TEST(S3StorageBackendTest, ValidatesTransferOptionsAtConstruction)
{
  auto config = TestConfig();
  config.transfer.connection.endpoint = "invalid-endpoint";
  EXPECT_THROW(S3StorageBackend{config}, std::invalid_argument);
  config = TestConfig();
  config.transfer.upload_limits.active_files = 0;
  EXPECT_THROW(S3StorageBackend{config}, std::invalid_argument);
  for (const auto timeout : {std::chrono::milliseconds(0), std::chrono::milliseconds(-1),
                            std::chrono::milliseconds(std::chrono::hours(24)) + std::chrono::milliseconds(1)}) {
    config = TestConfig();
    config.transfer.restore_timeout = timeout;
    EXPECT_THROW(S3StorageBackend{config}, std::invalid_argument);
  }
  config = TestConfig();
  config.transfer.restore_timeout = std::chrono::hours(24);
  EXPECT_NO_THROW(S3StorageBackend{config});
}

TEST(S3StorageBackendTest, RejectsDirectRestoreBeforeRemoteIO)
{
  const auto config = TestConfig();
  S3StorageBackend backend(config);
  ArtifactTarget target;
  target.set_store_id(config.store_id);
  target.mutable_artifact()->set_artifact_uid("direct");
  target.mutable_artifact()->set_container_name("main");
  const auto artifact = backend.ResolveTarget(target);
  EXPECT_THROW(backend.PrepareRestore(artifact, false, TransferMode::Direct, {}), std::invalid_argument);
}

TEST(S3StorageBackendTest, BindsIdentityBeforeStorageIO)
{
  const auto config = TestConfig();
  S3StorageBackend backend(config);
  ArtifactTarget target;
  target.set_store_id(config.store_id);
  target.mutable_artifact()->set_artifact_uid("content-1");
  target.mutable_artifact()->set_container_name("main");
  const auto artifact = backend.ResolveTarget(target);
  EXPECT_EQ(artifact.SerializeAsString(), backend.ResolveTarget(target).SerializeAsString());
  EXPECT_EQ(artifact.artifact_handle(), "artifacts/content-1/containers/main");
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
}

TEST(S3StorageBackendTest, ValidatesIncomingArtifactDescriptorsBeforeStorageIO)
{
  S3StorageBackend backend(TestConfig());
  PublishedArtifact artifact;
  artifact.set_store_id("test-store");
  artifact.set_artifact_handle("artifacts/content-1/containers/main");
  EXPECT_NO_THROW(backend.ValidateArtifactDescriptor(artifact));
  for (const auto* handle : {"", "../foreign", "short", "artifacts/../containers/main",
      "artifacts/content/containers/main/extra", "/artifacts/content/containers/main",
      "artifacts/content/containers/main/", "artifacts/content/other/main",
      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}) {
    artifact.set_artifact_handle(handle);
    EXPECT_THROW(backend.ValidateArtifactDescriptor(artifact), std::invalid_argument);
  }
  artifact.set_artifact_handle("artifacts/content-1/containers/main");
  artifact.clear_store_id();
  EXPECT_THROW(backend.ValidateArtifactDescriptor(artifact), std::invalid_argument);
  artifact.set_store_id("another-store");
  try {
    backend.ValidateArtifactDescriptor(artifact);
    FAIL() << "foreign artifact accepted";
  }
  catch (const TransferError& error) {
    EXPECT_EQ(error.code, Failure::STORE_MISMATCH);
  }
}

TEST(S3StorageBackendTest, SeparatesIdentityTupleBoundaries)
{
  const auto config = TestConfig();
  S3StorageBackend backend(config);
  ArtifactTarget target;
  target.set_store_id(config.store_id);
  target.mutable_artifact()->set_artifact_uid("ab");
  target.mutable_artifact()->set_container_name("c");
  const auto first = backend.ResolveTarget(target);
  target.mutable_artifact()->set_artifact_uid("a");
  target.mutable_artifact()->set_container_name("bc");
  EXPECT_NE(first.artifact_handle(), backend.ResolveTarget(target).artifact_handle());
}

TEST(S3StorageBackendTest, PreflightsCompleteCaptureWithoutStorageIO)
{
  test::TemporaryDirectory root;
  const auto config = TestConfig();
  S3StorageBackend backend(config);
  PublishedArtifact destination;
  destination.set_store_id("test-store");
  destination.set_artifact_handle("artifacts/content-1/containers/main");
  backend.ValidateArtifactDescriptor(destination);
  EXPECT_THROW(backend.BuildCheckpointInventory(root.path(), destination, {}), std::invalid_argument);
  std::ofstream(root.path() / "manifest.yaml") << "version: 1\n";
  const auto inventory = backend.BuildCheckpointInventory(root.path(), destination, {});
  ASSERT_EQ(inventory.files.size(), 1);
  EXPECT_EQ(inventory.files.front().relative_path, "manifest.yaml");
  EXPECT_EQ(inventory.files.front().size_bytes, 11u);
  fs::create_symlink(root.path() / "manifest.yaml", root.path() / "link");
  EXPECT_THROW(backend.BuildCheckpointInventory(root.path(), destination, {}), std::invalid_argument);
}

TEST(S3StorageBackendTest, RequiresAConfiguredBucketAndStoreAtConstruction)
{
  auto missing_bucket = TestConfig();
  missing_bucket.bucket.clear();
  EXPECT_THROW(S3StorageBackend{missing_bucket}, std::invalid_argument);
  auto missing_store = TestConfig();
  missing_store.store_id.clear();
  EXPECT_THROW(S3StorageBackend{missing_store}, std::invalid_argument);
}

TEST(S3StorageBackendTest, RejectsInvalidPrefixesAtConstruction)
{
  auto config = TestConfig();
  // Direct backend construction enforces the same prefix contract as startup JSON.
  for (const auto& prefix : {std::string("bad\nkey"), std::string("bad\rkey"),
                            std::string("bad\0key", 7), std::string(385, 'x')}) {
    SCOPED_TRACE(prefix);
    config.prefix = prefix;
    EXPECT_THROW(S3StorageBackend{config}, std::invalid_argument);
  }
}

TEST(S3StorageBackendTest, ChecksLongerPayloadKeysDuringInspection)
{
  auto config = TestConfig();
  config.prefix = std::string(384, 'x');
  S3StorageBackend backend(config);
  ArtifactTarget target;
  target.set_store_id(config.store_id);
  target.mutable_artifact()->set_artifact_uid(std::string(256, 'u'));
  target.mutable_artifact()->set_container_name(std::string(63, 'c'));
  const auto artifact = backend.ResolveTarget(target);
  test::TemporaryDirectory source;
  std::ofstream(source.path() / "manifest.yaml") << "version: 1\n";
  const auto directory = source.path() / std::string(150, 'd');
  fs::create_directory(directory);
  // Prefix + handle + /data/ uses 732 bytes; this relative path adds exactly 292.
  std::ofstream(directory / std::string(141, 'f')) << "payload";
  EXPECT_NO_THROW(backend.BuildCheckpointInventory(source.path(), artifact, {}));
  std::ofstream(directory / std::string(142, 'f')) << "payload";
  EXPECT_THROW(backend.BuildCheckpointInventory(source.path(), artifact, {}), std::invalid_argument);
}

TEST(S3StorageBackendTest, RejectsUnsafeIdentitiesAndAcceptsOpaqueUtf8Uids)
{
  S3StorageBackend backend(TestConfig());
  ArtifactTarget target;
  target.set_store_id("test-store");
  auto* identity = target.mutable_artifact();
  identity->set_container_name("main");
  for (const std::string& uid : {std::string(), std::string("."), std::string(".."), std::string("a/b"),
       std::string("a\\b"), std::string("a b"), std::string("a\n"), std::string("a\xc2\xa0"),
       std::string("a\xe2\x80\x83"), std::string("\xff"), std::string(257, 'a'), std::string(1, '\0')}) {
    identity->set_artifact_uid(uid);
    EXPECT_THROW(backend.ResolveTarget(target), std::invalid_argument);
  }
  identity->set_artifact_uid("opaque-é");
  EXPECT_EQ(backend.ResolveTarget(target).artifact_handle(), "artifacts/opaque-é/containers/main");
  for (const auto* container : {"", "Main", "-main", "main-", "a/b", ".", "..", "a.b"}) {
    identity->set_container_name(container);
    EXPECT_THROW(backend.ResolveTarget(target), std::invalid_argument);
  }
  identity->set_container_name(std::string(64, 'a'));
  EXPECT_THROW(backend.ResolveTarget(target), std::invalid_argument);
}

TEST(S3StorageBackendTest, OwnsConfiguration)
{
  auto config = TestConfig();
  config.prefix = "snapshots/nested";
  S3StorageBackend backend(config);
  config.bucket = "changed-bucket";
  config.prefix = "changed-prefix";
  config.transfer.connection.region = "changed-region";

  EXPECT_EQ(backend.config().bucket, "test-bucket");
  EXPECT_EQ(backend.config().prefix, "snapshots/nested");
  EXPECT_EQ(backend.config().transfer.connection.region, "us-east-1");
}
}  // namespace
}  // namespace snapshot::pagebroker
