// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

namespace snapshot::pagebroker {

// Read one request only far enough to preserve its correlation identifiers,
// then reject it before broker dispatch. The framed typed response lets CUDA
// callers distinguish admission pressure from an ambiguous post-dial failure.
bool HandleConnectionLimit(int connection);

}  // namespace snapshot::pagebroker
