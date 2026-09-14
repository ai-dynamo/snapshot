// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker {
struct DirectRestoreCarrier {
  std::string filename;
  FileDescriptor descriptor{-1};
  uintmax_t size = 0;
  uint64_t device = 0;
  uint64_t inode = 0;
};

struct DirectRestoreProcess {
  uint32_t namespace_pid = 0;
  std::string directory_name;
  FileDescriptor directory{-1};
  FileDescriptor manifest{-1};
  std::vector<DirectRestoreCarrier> carriers;
};

using DirectRestoreProcesses = std::vector<DirectRestoreProcess>;
}  // namespace snapshot::pagebroker
