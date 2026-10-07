// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <fstream>
#include <optional>

#include "artifact_store.hpp"
#include "identity.hpp"

namespace {
namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

constexpr char kStoreID[] = "store-v1-57e06c9609c92e973d048143531baadb9271a9571f1d1dbc21af2c6cd13d24c2";

class ArtifactStoreTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / "pagebroker-artifact-store-tests" /
            ::testing::UnitTest::GetInstance()->current_test_info()->name();
    fs::remove_all(root_);
    storage_root_ = root_ / "storage";
    fs::create_directories(storage_root_);
    store_.emplace(kStoreID, storage_root_);
  }

  void TearDown() override { fs::remove_all(root_); }

  ArtifactTarget Target(const std::string& uid, const std::string& container) const
  {
    ArtifactTarget target;
    target.set_store_id(kStoreID);
    target.mutable_artifact()->set_artifact_uid(uid);
    target.mutable_artifact()->set_container_name(container);
    return target;
  }

  Path StagedCheckpoint(const std::string& name) const
  {
    const Path staging = root_ / "staging" / name;
    fs::create_directories(staging);
    std::ofstream(staging / "manifest.yaml") << "artifact:\n  contentUID: " << name << "\n";
    std::ofstream(staging / "pages-1.img") << "criu image bytes";
    return staging;
  }

  fs::path root_;
  fs::path storage_root_;
  std::optional<PVCArtifactStore> store_;
};

TEST_F(ArtifactStoreTest, PublishesUnderTheExistingLayout)
{
  const auto staging = StagedCheckpoint("uid-1");
  PublishedArtifact artifact;
  store_->PublishCheckpoint(staging, Target("uid-1", "main"), &artifact);

  EXPECT_EQ(artifact.store_id(), kStoreID);
  EXPECT_EQ(artifact.artifact_handle(), "artifacts/uid-1/containers/main");
  EXPECT_EQ(artifact.artifact_format_version(), kFilesystemFormatVersion);
  EXPECT_TRUE(fs::exists(staging)) << "the store copies staging into place; the caller (broker.cpp) removes it";
  EXPECT_TRUE(fs::is_directory(storage_root_ / "artifacts/uid-1/containers/main"));
  EXPECT_TRUE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main/manifest.yaml"));
  EXPECT_TRUE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main/pages-1.img"));
  EXPECT_TRUE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main/publication.json"));
}

TEST_F(ArtifactStoreTest, RepublishingTheSameTargetReplacesItAtomically)
{
  PublishedArtifact first;
  store_->PublishCheckpoint(StagedCheckpoint("first"), Target("uid-2", "main"), &first);
  std::ofstream(storage_root_ / "artifacts/uid-2/containers/main/sentinel") << "first";

  PublishedArtifact second;
  store_->PublishCheckpoint(StagedCheckpoint("second"), Target("uid-2", "main"), &second);

  EXPECT_EQ(first.artifact_handle(), second.artifact_handle());
  EXPECT_FALSE(fs::exists(storage_root_ / "artifacts/uid-2/containers/main/sentinel"))
      << "republish must replace the previous payload, not merge with it";
  EXPECT_FALSE(fs::exists(storage_root_ / "artifacts/uid-2/containers/main.pagebroker-previous"));
}

TEST_F(ArtifactStoreTest, RefusesATargetNamingAnotherStore)
{
  ArtifactTarget target = Target("uid-3", "main");
  target.set_store_id("store-v1-b4939fa5238a98746407fde26742f0b29cfe3c6852fafb3cf7c091a33fe3475b");
  PublishedArtifact out;
  try {
    store_->PublishCheckpoint(StagedCheckpoint("mismatch"), target, &out);
    FAIL() << "expected ArtifactError";
  }
  catch (const ArtifactError& error) {
    EXPECT_EQ(error.code(), Failure::STORE_MISMATCH);
  }
}

TEST_F(ArtifactStoreTest, ResolvesAndStagesAPublishedRestore)
{
  PublishedArtifact artifact;
  store_->PublishCheckpoint(StagedCheckpoint("uid-4"), Target("uid-4", "main"), &artifact);

  const auto plan = store_->ResolveRestorePlan(artifact);
  const Path metadata_destination = root_ / "metadata-staging";
  store_->StageMetadata(plan, metadata_destination);
  EXPECT_TRUE(fs::exists(metadata_destination / "manifest.yaml"));
  EXPECT_FALSE(fs::exists(metadata_destination / "pages-1.img")) << "metadata staging must not copy the payload";

  const Path restore_destination = root_ / "restore-staging";
  store_->StageRestore(plan, restore_destination);
  EXPECT_TRUE(fs::exists(restore_destination / "manifest.yaml"));
  EXPECT_TRUE(fs::exists(restore_destination / "pages-1.img"));
}

TEST_F(ArtifactStoreTest, RefusesResolvingAnUnpublishedArtifact)
{
  PublishedArtifact artifact;
  artifact.set_store_id(kStoreID);
  artifact.set_artifact_handle("artifacts/never-published/containers/main");
  artifact.set_artifact_format_version(kFilesystemFormatVersion);
  try {
    store_->ResolveRestorePlan(artifact);
    FAIL() << "expected ArtifactError";
  }
  catch (const ArtifactError& error) {
    EXPECT_EQ(error.code(), Failure::ARTIFACT_NOT_FOUND);
  }
}

TEST_F(ArtifactStoreTest, RefusesAHandleThatEscapesTheStorageRoot)
{
  PublishedArtifact artifact;
  artifact.set_store_id(kStoreID);
  artifact.set_artifact_handle("../escape");
  artifact.set_artifact_format_version(kFilesystemFormatVersion);
  try {
    store_->ResolveRestorePlan(artifact);
    FAIL() << "expected ArtifactError";
  }
  catch (const ArtifactError& error) {
    EXPECT_EQ(error.code(), Failure::ARTIFACT_CORRUPT);
  }
}

TEST_F(ArtifactStoreTest, RefusesCorruptedEvidence)
{
  PublishedArtifact artifact;
  store_->PublishCheckpoint(StagedCheckpoint("uid-5"), Target("uid-5", "main"), &artifact);
  std::ofstream(storage_root_ / "artifacts/uid-5/containers/main/publication.json") << "not json";

  try {
    store_->ResolveRestorePlan(artifact);
    FAIL() << "expected ArtifactError";
  }
  catch (const ArtifactError& error) {
    EXPECT_EQ(error.code(), Failure::ARTIFACT_CORRUPT);
  }
}

TEST_F(ArtifactStoreTest, DeterministicCommitIDMatchesIdentityComputation)
{
  PublishedArtifact artifact;
  store_->PublishCheckpoint(StagedCheckpoint("uid-6"), Target("uid-6", "main"), &artifact);
  const std::string want = ComputeCommitID(kStoreID, "uid-6", "main");

  std::ifstream file(storage_root_ / "artifacts/uid-6/containers/main/publication.json");
  const std::string json((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
  EXPECT_NE(json.find(want), std::string::npos) << "publication evidence must carry the deterministic commitID";
}

}  // namespace
