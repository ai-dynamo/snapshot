// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "file_descriptor.hpp"

#include <unistd.h>

FileDescriptor::FileDescriptor(int value) : value_(value) {}

FileDescriptor::~FileDescriptor() noexcept
{
  if (value_ >= 0)
    close(value_);
}

int
FileDescriptor::get() const
{
  return value_;
}
