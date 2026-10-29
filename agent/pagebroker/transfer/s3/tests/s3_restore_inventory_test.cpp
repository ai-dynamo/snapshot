// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/s3_storage_backend.hpp"

#include <gtest/gtest.h>
#include <nlohmann/json.hpp>

#include <chrono>
#include <cstddef>
#include <optional>
#include <stdexcept>
#include <string>

namespace snapshot::pagebroker {
namespace {
// Only the index read is intercepted. Any payload lookup would attempt to run
// rclone against the unreachable endpoint and fail these preparation tests.
std::optional<std::string> index_bytes;
Cancellation* cancel_after_read = nullptr;
std::size_t index_reads = 0;

class S3RestoreInventoryTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    index = {{"format", "snapshot.s3-checkpoint/v1"}, {"rootMode", 448},
             {"directories", nlohmann::json::array()},
             {"files", {{{"path", "empty"}, {"mode", 384}, {"size", 0}},
                        {{"path", "manifest.yaml"}, {"mode", 384}, {"size", 11}}}}};
    index_reads = 0;
    cancel_after_read = nullptr;
    config.store_id = "test-store";
    config.bucket = "test-bucket";
    config.transfer.connection.endpoint = "http://127.0.0.1:1";
    config.transfer.connection.region = "us-east-1";
    config.transfer.connection.access_key_id = "fake-access-key";
    config.transfer.connection.secret_access_key = "fake-secret-key";
    artifact.set_store_id(config.store_id);
    artifact.set_artifact_handle("artifacts/content/containers/main");
  }

  PreparedRestore Prepare(bool metadata_only = false,
      Cancellation cancellation = Cancellation(Cancellation::Clock::now() + std::chrono::seconds(5)))
  {
    index_bytes = index.dump();
    return S3StorageBackend(config).PrepareRestore(artifact, metadata_only, TransferMode::Staged, cancellation);
  }

  nlohmann::json index;
  S3Config config;
  PublishedArtifact artifact;
};

TEST_F(S3RestoreInventoryTest, PreparesFromTheIndexWithoutReadingPayloadObjects)
{
  const auto prepared = Prepare();
  EXPECT_EQ(index_reads, 1u);
  ASSERT_EQ(prepared.inventory.files.size(), 2u);
  EXPECT_EQ(prepared.inventory.files[0].size_bytes, 0u);
  EXPECT_EQ(prepared.inventory.files[1].size_bytes, 11u);
  EXPECT_EQ(prepared.inventory.files[1].source_locator,
            "s3://test-bucket/artifacts/content/containers/main/data/manifest.yaml");
}

TEST_F(S3RestoreInventoryTest, SelectsOnlyTheManifestForMetadata)
{
  const auto prepared = Prepare(true);
  EXPECT_EQ(index_reads, 1u);
  ASSERT_EQ(prepared.inventory.files.size(), 1u);
  EXPECT_EQ(prepared.inventory.files.front().relative_path, "manifest.yaml");
}

TEST_F(S3RestoreInventoryTest, StillRejectsInvalidIndexSizes)
{
  index["files"][0]["size"] = -1;
  EXPECT_THROW(Prepare(), CheckpointError);
}

TEST_F(S3RestoreInventoryTest, StillRejectsOversizedFullObjectKeys)
{
  config.prefix = std::string(384, 'p');
  artifact.set_artifact_handle("artifacts/" + std::string(256, 'a') + "/containers/" + std::string(63, 'c'));
  index["files"][0]["path"] = std::string(300, 'a');
  EXPECT_THROW(Prepare(), std::invalid_argument);
}

TEST_F(S3RestoreInventoryTest, ObservesCancellationAfterReadingTheIndex)
{
  Cancellation cancellation;
  cancel_after_read = &cancellation;
  EXPECT_THROW(Prepare(false, cancellation), CancellationError);
  cancel_after_read = nullptr;
}
}  // namespace

// GNU ld's method wrapper receives the implicit this pointer first.
extern "C" std::optional<std::string>
__wrap__ZN8snapshot10pagebroker8S3Client3GetB5cxx11ERKNS0_16S3ObjectLocationEmNS0_12CancellationE(
    S3Client*, const S3ObjectLocation& object, std::size_t max_bytes, Cancellation)
{
  EXPECT_EQ(object.key.substr(object.key.rfind('/') + 1), "index.json");
  EXPECT_EQ(max_bytes, kMaxIndexBytes);
  ++index_reads;
  if (cancel_after_read)
    cancel_after_read->Cancel();
  return index_bytes;
}
}  // namespace snapshot::pagebroker
