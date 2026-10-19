// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>

namespace snapshot::pagebroker {
inline constexpr std::chrono::seconds kDefaultTransactionLifetime{7500};
}  // namespace snapshot::pagebroker
