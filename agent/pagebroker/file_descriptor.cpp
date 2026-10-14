// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "file_descriptor.hpp"

#include <unistd.h>
#include <fcntl.h>
#include <cerrno>
#include <system_error>

#include <utility>

FileDescriptor::FileDescriptor(int value) : value_(value) {}

FileDescriptor::~FileDescriptor() noexcept
{
  if (value_ >= 0)
    close(value_);
}

FileDescriptor::FileDescriptor(FileDescriptor&& other) noexcept : value_(std::exchange(other.value_, -1)) {}

FileDescriptor&
FileDescriptor::operator=(FileDescriptor&& other) noexcept
{
  if (this != &other) {
    if (value_ >= 0)
      close(value_);
    value_ = std::exchange(other.value_, -1);
  }
  return *this;
}

int
FileDescriptor::get() const
{
  return value_;
}

FileDescriptor
FileDescriptor::Duplicate(int value)
{
  FileDescriptor copy(fcntl(value, F_DUPFD_CLOEXEC, 0));
  if (copy.get() < 0) {
    throw std::system_error(errno, std::generic_category(), "duplicate file descriptor");
  }
  return copy;
}
