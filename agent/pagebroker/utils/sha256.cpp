// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "sha256.hpp"

#include <fcntl.h>
#include <openssl/evp.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <memory>
#include <stdexcept>
#include <system_error>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker::utils {
namespace {
class DigestContext {
 public:
  DigestContext() : value_(EVP_MD_CTX_new(), EVP_MD_CTX_free)
  {
    if (!value_ || EVP_DigestInit_ex(value_.get(), EVP_sha256(), nullptr) != 1)
      throw std::runtime_error("initialize SHA-256");
  }

  void Update(std::span<const std::byte> bytes)
  {
    if (!bytes.empty() && EVP_DigestUpdate(value_.get(), bytes.data(), bytes.size()) != 1)
      throw std::runtime_error("update SHA-256");
  }

  Sha256Digest Finish()
  {
    Sha256Digest result;
    unsigned length = 0;
    if (EVP_DigestFinal_ex(value_.get(), result.data(), &length) != 1 || length != result.size())
      throw std::runtime_error("finish SHA-256");
    return result;
  }

 private:
  std::unique_ptr<EVP_MD_CTX, decltype(&EVP_MD_CTX_free)> value_;
};
}  // namespace

Sha256Digest
ComputeSha256(std::span<const std::byte> bytes, const std::function<void()>& check)
{
  DigestContext digest;
  while (!bytes.empty()) {
    if (check)
      check();
    const auto length = std::min<std::size_t>(bytes.size(), 128 * 1024);
    digest.Update(bytes.first(length));
    bytes = bytes.subspan(length);
  }
  if (check)
    check();
  return digest.Finish();
}

Sha256Digest
ComputeFileSha256(const std::filesystem::path& path)
{
  // Nonblocking open permits rejecting a FIFO without waiting for a writer.
  FileDescriptor source(open(path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK));
  if (source.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open SHA-256 source");
  return ComputeFileSha256(source.get());
}

Sha256Digest
ComputeFileSha256(int descriptor, const std::function<void()>& check)
{
  struct stat status;
  if (fstat(descriptor, &status) < 0)
    throw std::system_error(errno, std::generic_category(), "stat SHA-256 source");
  if (!S_ISREG(status.st_mode))
    throw std::invalid_argument("SHA-256 source must be a regular file");

  DigestContext digest;
  std::array<std::byte, 128 * 1024> buffer;
  off_t offset = 0;
  while (offset < status.st_size) {
    if (check)
      check();
    const auto length = std::min<uintmax_t>(buffer.size(), status.st_size - offset);
    const auto count = pread(descriptor, buffer.data(), length, offset);
    if (count < 0) {
      if (errno == EINTR)
        continue;
      throw std::system_error(errno, std::generic_category(), "read SHA-256 source");
    }
    if (count == 0)
      throw std::runtime_error("SHA-256 source shortened during read");
    digest.Update(std::span(buffer.data(), static_cast<std::size_t>(count)));
    offset += count;
  }
  if (check)
    check();
  return digest.Finish();
}
}  // namespace snapshot::pagebroker::utils
