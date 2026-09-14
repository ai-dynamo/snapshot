// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <chrono>

#include "transaction.hpp"

namespace snapshot::pagebroker {
namespace {

constexpr auto kPreMutationLifetime = std::chrono::minutes(10);
constexpr auto kAdvancedClock = std::chrono::hours(24 * 365);

TEST(TransactionTest, ExpiresOnlyPreMutationStates)
{
  for (const auto state : {
           Transaction::State::NEW,
           Transaction::State::PREPARING,
           Transaction::State::STAGED,
       }) {
    Transaction transaction;
    transaction.set_state(state);
    const auto now = std::chrono::steady_clock::now();

    EXPECT_FALSE(transaction.expired(now + std::chrono::minutes(9), kPreMutationLifetime));
    EXPECT_TRUE(transaction.expired(now + kPreMutationLifetime, kPreMutationLifetime));
  }
}

TEST(TransactionTest, NeverExpiresMutationOrPublishStates)
{
  for (const auto state : {
           Transaction::State::CHECKPOINT_ADMITTED,
           Transaction::State::RESTORE_ADMITTED,
           Transaction::State::CUDA_STARTED,
           Transaction::State::CUDA_COMPLETE,
           Transaction::State::PUBLISHED_CLEANUP_PENDING,
           Transaction::State::COMMITTED,
           Transaction::State::ABORTED,
       }) {
    Transaction transaction;
    transaction.set_state(state);

    EXPECT_FALSE(transaction.expired(
        std::chrono::steady_clock::now() + kAdvancedClock,
        kPreMutationLifetime));
  }
}

}  // namespace
}  // namespace snapshot::pagebroker
