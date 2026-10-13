// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <fstream>
#include <iterator>
#include <optional>
#include <string>

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

  PublishedArtifact Descriptor(const std::string& handle) const
  {
    PublishedArtifact artifact;
    artifact.set_store_id(kStoreID);
    artifact.set_artifact_handle(handle);
    return artifact;
  }

  static std::string ReadFile(const Path& path)
  {
    std::ifstream file(path);
    return std::string((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
  }

  template <typename Operation>
  static void ExpectArtifactError(Failure::Code code, Operation&& operation)
  {
    try {
      operation();
      FAIL() << "expected ArtifactError";
    }
    catch (const ArtifactError& error) {
      EXPECT_EQ(error.code(), code) << error.what();
    }
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
  EXPECT_TRUE(fs::exists(staging)) << "the store copies staging into place; the caller (broker.cpp) removes it";
  EXPECT_TRUE(fs::is_directory(storage_root_ / "artifacts/uid-1/containers/main"));
  EXPECT_TRUE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main/manifest.yaml"));
  EXPECT_TRUE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main/pages-1.img"));
  EXPECT_FALSE(fs::exists(storage_root_ / "artifacts/uid-1/containers/main.pagebroker-partial"));
}

TEST_F(ArtifactStoreTest, RefusesASecondPublicationOfTheSameTarget)
{
  PublishedArtifact first;
  store_->PublishCheckpoint(StagedCheckpoint("first"), Target("uid-2", "main"), &first);
  const Path published = storage_root_ / "artifacts/uid-2/containers/main";
  std::ofstream(published / "sentinel") << "first";

  PublishedArtifact second;
  ExpectArtifactError(Failure::TRANSACTION_CONFLICT, [&] {
    store_->PublishCheckpoint(StagedCheckpoint("second"), Target("uid-2", "main"), &second);
  });
  EXPECT_EQ(ReadFile(published / "sentinel"), "first") << "the existing publication must be left untouched";
  EXPECT_NE(ReadFile(published / "manifest.yaml").find("contentUID: first"), std::string::npos);
  EXPECT_FALSE(fs::exists(storage_root_ / "artifacts/uid-2/containers/main.pagebroker-partial"));
}

TEST_F(ArtifactStoreTest, RefusesToPublishOverAnInProgressPartial)
{
  const Path partial = storage_root_ / "artifacts/uid-9/containers/main.pagebroker-partial";
  fs::create_directories(partial);
  std::ofstream(partial / "keep") << "keep";

  PublishedArtifact out;
  ExpectArtifactError(Failure::TRANSACTION_CONFLICT, [&] {
    store_->PublishCheckpoint(StagedCheckpoint("uid-9"), Target("uid-9", "main"), &out);
  });
  EXPECT_EQ(ReadFile(partial / "keep"), "keep");
  EXPECT_FALSE(fs::exists(storage_root_ / "artifacts/uid-9/containers/main"));
}

TEST_F(ArtifactStoreTest, RefusesAnOccupiedTargetBeforeCapture)
{
  PublishedArtifact published;
  store_->PublishCheckpoint(StagedCheckpoint("uid-11"), Target("uid-11", "main"), &published);
  ExpectArtifactError(Failure::TRANSACTION_CONFLICT, [&] { store_->ValidateTarget(Target("uid-11", "main")); });

  fs::create_directories(storage_root_ / "artifacts/uid-12/containers/main.pagebroker-partial");
  ExpectArtifactError(Failure::TRANSACTION_CONFLICT, [&] { store_->ValidateTarget(Target("uid-12", "main")); });

  EXPECT_NO_THROW(store_->ValidateTarget(Target("uid-13", "main")));
}

TEST_F(ArtifactStoreTest, ReportsAnUnreachableStoreAsUnavailableNotAbsent)
{
  const PVCArtifactStore unmounted(kStoreID, root_ / "unmounted");
  ExpectArtifactError(Failure::STORAGE_UNAVAILABLE, [&] { unmounted.ValidateTarget(Target("uid-14", "main")); });
  ExpectArtifactError(Failure::STORAGE_UNAVAILABLE, [&] {
    unmounted.ResolveRestorePlan(Descriptor("artifacts/uid-14/containers/main"));
  });

  std::ofstream(root_ / "not-a-directory") << "x";
  const PVCArtifactStore misconfigured(kStoreID, root_ / "not-a-directory");
  ExpectArtifactError(Failure::STORAGE_UNAVAILABLE, [&] {
    misconfigured.ResolveRestorePlan(Descriptor("artifacts/uid-14/containers/main"));
  });
}

TEST_F(ArtifactStoreTest, RefusesATargetNamingAnotherStore)
{
  ArtifactTarget target = Target("uid-3", "main");
  target.set_store_id("store-v1-b4939fa5238a98746407fde26742f0b29cfe3c6852fafb3cf7c091a33fe3475b");
  PublishedArtifact out;
  ExpectArtifactError(Failure::STORE_MISMATCH, [&] {
    store_->PublishCheckpoint(StagedCheckpoint("mismatch"), target, &out);
  });
}

TEST_F(ArtifactStoreTest, RefusesATargetWithAnInvalidIdentity)
{
  PublishedArtifact out;
  ExpectArtifactError(Failure::INVALID_REQUEST, [&] { store_->ValidateTarget(Target("uid-3", "Main_1")); });
  ExpectArtifactError(Failure::INVALID_REQUEST, [&] { store_->ValidateTarget(Target("..", "main")); });
  ExpectArtifactError(Failure::INVALID_REQUEST, [&] { store_->ValidateTarget(Target("uid 3", "main")); });
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
  ExpectArtifactError(Failure::ARTIFACT_NOT_FOUND, [&] {
    store_->ResolveRestorePlan(Descriptor("artifacts/never-published/containers/main"));
  });
}

TEST_F(ArtifactStoreTest, RefusesAHandleThatEscapesTheStorageRoot)
{
  ExpectArtifactError(Failure::ARTIFACT_CORRUPT, [&] { store_->ResolveRestorePlan(Descriptor("../escape")); });
}

TEST_F(ArtifactStoreTest, RefusesAHandleOutsideTheArtifactLayout)
{
  const Path stray = storage_root_ / "stray";
  fs::create_directories(stray);
  std::ofstream(stray / "manifest.yaml") << "artifact: {}\n";
  ExpectArtifactError(Failure::ARTIFACT_CORRUPT, [&] { store_->ResolveRestorePlan(Descriptor("stray")); });
}

TEST_F(ArtifactStoreTest, RefusesAHandleNamingAnInvalidIdentity)
{
  ExpectArtifactError(Failure::ARTIFACT_CORRUPT, [&] {
    store_->ResolveRestorePlan(Descriptor("artifacts/uid-5/containers/Main_1"));
  });
}

TEST_F(ArtifactStoreTest, RefusesAPublicationWithoutAManifest)
{
  PublishedArtifact artifact;
  store_->PublishCheckpoint(StagedCheckpoint("uid-6"), Target("uid-6", "main"), &artifact);
  fs::remove(storage_root_ / "artifacts/uid-6/containers/main/manifest.yaml");
  ExpectArtifactError(Failure::ARTIFACT_CORRUPT, [&] { store_->ResolveRestorePlan(artifact); });
}

}  // namespace
