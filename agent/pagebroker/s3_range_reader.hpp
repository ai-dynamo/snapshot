// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace snapshot::pagebroker {
struct S3WriteRange {
  std::string image;
  uint64_t source_offset;
  uint64_t length;
  int destination_fd;
  size_t destination_offset;
};

bool S3RangeReaderEnabled();
int WriteS3Ranges(const std::string& prefix,
                  const std::vector<S3WriteRange>& ranges);
}  // namespace snapshot::pagebroker
