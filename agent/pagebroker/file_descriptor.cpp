// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "file_descriptor.hpp"

#include <unistd.h>
#include <fcntl.h>
#include <cerrno>
#include <string>
#include <system_error>
#include <sys/stat.h>

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

FileDescriptor
FileDescriptor::OpenDirectory(int parent, const char* name)
{
  FileDescriptor directory(openat(parent, name, O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  if (directory.get() < 0) {
    const auto error = errno;
    throw std::system_error(error, std::generic_category(), std::string("open directory ") + name);
  }
  return directory;
}

void
FileDescriptor::RequireDirectory() const
{
  struct stat info{};
  if (fstat(value_, &info)) {
    throw std::system_error(errno, std::generic_category(), "stat directory");
  }
  if (!S_ISDIR(info.st_mode)) {
    throw std::invalid_argument("artifact path must be a directory");
  }
}
