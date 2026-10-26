// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "tests/broker_transfer_fixture.hpp"

#include <sys/stat.h>

#include <functional>
#include <utility>

namespace {
// Interrupt transaction directory creation without changing production code. Each
// worker owns its hook; unrelated filesystem operations remain unchanged.
thread_local std::function<void(const char*)> mkdir_hook;
}  // namespace

extern "C" int __real_mkdir(const char*, mode_t);
extern "C" int __wrap_mkdir(const char* path, mode_t mode)
{
  const int result = __real_mkdir(path, mode);
  if (result == 0 && mkdir_hook)
    mkdir_hook(path);
  return result;
}

namespace snapshot::pagebroker::test {
ScopedMkdirHook::ScopedMkdirHook(std::function<void(const char*)> hook)
{
  mkdir_hook = std::move(hook);
}

ScopedMkdirHook::~ScopedMkdirHook()
{
  mkdir_hook = {};
}
}  // namespace snapshot::pagebroker::test
