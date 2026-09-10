// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer_engine.hpp"

namespace snapshot::pagebroker {
bool S3TransferEnabled();
void PublishToS3(const Path& source);
void StageLocalImagesFromS3(const Path& destination);
}  // namespace snapshot::pagebroker
