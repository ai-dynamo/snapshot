// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>
#include <google/protobuf/struct.pb.h>
#include <google/protobuf/util/json_util.h>

#include <fstream>
#include <iterator>
#include <string>

#include "identity.hpp"

namespace {

google::protobuf::Struct
ReadFixtures()
{
  std::ifstream file("testdata/storage-contract/identity.json");
  if (!file.is_open())
    ADD_FAILURE() << "could not open testdata/storage-contract/identity.json";
  const std::string json((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
  google::protobuf::Struct fixtures;
  const auto status = google::protobuf::util::JsonStringToMessage(json, &fixtures);
  EXPECT_TRUE(status.ok()) << status.ToString();
  return fixtures;
}

const std::string&
Field(const google::protobuf::Struct& fields, const char* key)
{
  return fields.fields().at(key).string_value();
}

}  // namespace

// Shared vectors with the Go side (api/storage/testdata/identity.json, copied
// here) pin the exact preimage encoding. A round trip through this type alone
// would miss a coordinated encoding change between the two languages.
TEST(Identity, StoreIDMatchesGoVectors)
{
  const auto fixtures = ReadFixtures();
  ASSERT_TRUE(fixtures.fields().contains("stores"));
  const auto& stores = fixtures.fields().at("stores").list_value().values();
  ASSERT_GT(stores.size(), 0);

  for (const auto& store : stores) {
    const auto& fields = store.struct_value().fields();
    const std::string name = Field(store.struct_value(), "name");
    const auto& pvc = fields.at("pvc").struct_value();
    const std::string want = Field(store.struct_value(), "storeID");
    const std::string got = snapshot::pagebroker::ComputeStoreID(
        Field(pvc, "namespace"), Field(pvc, "claimName"), Field(pvc, "basePath"));
    EXPECT_EQ(got, want) << "vector: " << name;
  }
}

TEST(Identity, CommitIDMatchesGoVectors)
{
  const auto fixtures = ReadFixtures();
  ASSERT_TRUE(fixtures.fields().contains("commits"));
  const auto& commits = fixtures.fields().at("commits").list_value().values();
  ASSERT_GT(commits.size(), 0);

  for (const auto& commit : commits) {
    const auto& fields = commit.struct_value();
    const std::string name = Field(fields, "name");
    const std::string want = Field(fields, "commitID");
    const std::string got = snapshot::pagebroker::ComputeCommitID(
        Field(fields, "storeID"), Field(fields, "artifactUID"), Field(fields, "containerName"));
    EXPECT_EQ(got, want) << "vector: " << name;
  }
}

TEST(Identity, RejectsRelativeBasePath)
{
  EXPECT_THROW(snapshot::pagebroker::ComputeStoreID("snapshot", "snapshot-pvc", "checkpoints"), std::invalid_argument);
}

TEST(Identity, RejectsParentTraversal)
{
  EXPECT_THROW(snapshot::pagebroker::ComputeStoreID("snapshot", "snapshot-pvc", "/a/../b"), std::invalid_argument);
}

TEST(Identity, RejectsMalformedStoreIDForCommit)
{
  EXPECT_THROW(snapshot::pagebroker::ComputeCommitID("not-a-store-id", "uid", "main"), std::invalid_argument);
}

TEST(Identity, RejectsPathSeparatorInArtifactUID)
{
  const std::string store_id = snapshot::pagebroker::ComputeStoreID("snapshot", "snapshot-pvc", "/");
  EXPECT_THROW(snapshot::pagebroker::ComputeCommitID(store_id, "a/b", "main"), std::invalid_argument);
}
