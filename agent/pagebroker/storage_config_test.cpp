// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include "storage_config.hpp"

namespace {
using snapshot::pagebroker::ParseStorageConfig;

TEST(StorageConfig, ParsesTheChartsExactShape)
{
  const std::string yaml =
      "pvc:\n"
      "  namespace: \"snapshot\"\n"
      "  claimName: \"snapshot-pvc\"\n"
      "  basePath: \"/\"\n";
  const auto config = ParseStorageConfig(yaml);
  EXPECT_EQ(config.pvc_namespace, "snapshot");
  EXPECT_EQ(config.pvc_claim_name, "snapshot-pvc");
  EXPECT_EQ(config.pvc_base_path, "/");
}

TEST(StorageConfig, IgnoresUnknownTopLevelKeys)
{
  const std::string yaml =
      "pvc:\n"
      "  namespace: \"snapshot\"\n"
      "  claimName: \"snapshot-pvc\"\n"
      "  basePath: \"/\"\n"
      "extra:\n"
      "  key: \"ignored\"\n";
  const auto config = ParseStorageConfig(yaml);
  EXPECT_EQ(config.pvc_claim_name, "snapshot-pvc");
}

TEST(StorageConfig, RejectsAMissingBackendBlock)
{
  EXPECT_THROW(ParseStorageConfig("extra:\n  key: \"value\"\n"), std::runtime_error);
}

TEST(StorageConfig, RejectsTwoBackendBlocks)
{
  const std::string yaml =
      "pvc:\n"
      "  namespace: \"snapshot\"\n"
      "  claimName: \"snapshot-pvc\"\n"
      "  basePath: \"/\"\n"
      "s3:\n"
      "  bucket: \"checkpoints\"\n";
  EXPECT_THROW(ParseStorageConfig(yaml), std::runtime_error);
}

TEST(StorageConfig, RejectsAnS3OnlyConfig)
{
  EXPECT_THROW(ParseStorageConfig("s3:\n  bucket: \"checkpoints\"\n"), std::runtime_error);
}

TEST(StorageConfig, RejectsIncompletePvcBlock)
{
  const std::string yaml =
      "pvc:\n"
      "  namespace: \"snapshot\"\n";
  EXPECT_THROW(ParseStorageConfig(yaml), std::runtime_error);
}
}  // namespace
