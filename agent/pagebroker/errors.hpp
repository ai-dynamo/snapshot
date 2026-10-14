// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <cstdio>
#include <exception>
#include <stdexcept>

namespace snapshot::pagebroker {
inline void
Require(bool condition, const char* message)
{
  if (!condition) {
    throw std::runtime_error(message);
  }
}

inline void
Validate(bool condition, const char* message)
{
  if (!condition) {
    throw std::invalid_argument(message);
  }
}

inline void
LogException(const char* operation, std::exception_ptr exception) noexcept
{
  try {
    std::rethrow_exception(exception);
  } catch (const std::exception& error) {
    std::fprintf(stderr, "%s: %s\n", operation, error.what());
  } catch (...) {
    std::fprintf(stderr, "%s: unknown exception\n", operation);
  }
}

}  // namespace snapshot::pagebroker
