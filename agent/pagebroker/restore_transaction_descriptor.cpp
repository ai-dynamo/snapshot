// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "restore_transaction_descriptor.hpp"

#include <utility>

namespace snapshot::pagebroker {
RestoreTransactionDescriptor::RestoreTransactionDescriptor(Path staging_directory)
    : staging_directory_(std::move(staging_directory))
{
}

const Path&
RestoreTransactionDescriptor::staging_directory() const
{
  return staging_directory_;
}
}  // namespace snapshot::pagebroker
