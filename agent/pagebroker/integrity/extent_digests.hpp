/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include "../storage_manifest.hpp"

namespace cuda_checkpoint_storage {

// Optional integrity metadata, indexed in manifest extent order. The caller
// owns its persistence; the core manifest and GPU transfer ring need no hashes.
// Checkpoint fills a pre-sized digest vector. Restore verifies every extent.
bool ApplyOrVerifyExtentDigests(bool checkpoint,
                               const std::vector<TransferJob> &jobs,
                               const std::vector<std::string> &digests,
                               std::vector<std::string> *extent_digests,
                               std::string *error);

} // namespace cuda_checkpoint_storage
