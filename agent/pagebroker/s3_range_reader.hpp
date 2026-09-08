// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace snapshot::pagebroker {
bool S3RangeReaderEnabled();
int ReadS3Range(const std::string& prefix, const char* image, uint64_t offset,
                void* buffer, size_t length);
}  // namespace snapshot::pagebroker
