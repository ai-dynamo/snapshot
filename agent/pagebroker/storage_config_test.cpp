// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include "storage_config.hpp"

namespace {
using snapshot::pagebroker::ParseStorageConfig;

TEST(StorageConfig, ParsesTheChartsExactShape)
{
  const std::string yaml =
      "type: \"pvc\"\n"
      "pvc:\n"
      "  namespace: \"snapshot\"\n"
      "  claimName: \"snapshot-pvc\"\n"
      "  basePath: \"/\"\n";
  const auto config = ParseStorageConfig(yaml);
  EXPECT_EQ(config.type, "pvc");
  EXPECT_EQ(config.pvc_namespace, "snapshot");
  EXPECT_EQ(config.pvc_claim_name, "snapshot-pvc");
  EXPECT_EQ(config.pvc_base_path, "/");
}

TEST(StorageConfig, IgnoresUnknownTopLevelKeys)
{
  const std::string yaml =
      "type: \"pvc\"\n"
      "pvc:\n"
      "  namespace: \"snapshot\"\n"
      "  claimName: \"snapshot-pvc\"\n"
      "  basePath: \"/\"\n"
      "s3:\n"
      "  bucket: \"ignored-until-09-a\"\n";
  const auto config = ParseStorageConfig(yaml);
  EXPECT_EQ(config.pvc_claim_name, "snapshot-pvc");
}

TEST(StorageConfig, RejectsMissingType)
{
  EXPECT_THROW(ParseStorageConfig("pvc:\n  namespace: \"snapshot\"\n"), std::runtime_error);
}

TEST(StorageConfig, RejectsIncompletePvcBlock)
{
  const std::string yaml =
      "type: \"pvc\"\n"
      "pvc:\n"
      "  namespace: \"snapshot\"\n";
  EXPECT_THROW(ParseStorageConfig(yaml), std::runtime_error);
}
}  // namespace
