// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "sha256.hpp"

#include <openssl/evp.h>

#include <memory>
#include <stdexcept>

namespace snapshot::pagebroker::utils {
Sha256Digest
ComputeSha256(std::span<const std::byte> bytes)
{
  std::unique_ptr<EVP_MD_CTX, decltype(&EVP_MD_CTX_free)> context(EVP_MD_CTX_new(), EVP_MD_CTX_free);
  if (!context || EVP_DigestInit_ex(context.get(), EVP_sha256(), nullptr) != 1)
    throw std::runtime_error("initialize SHA-256");
  if (!bytes.empty() && EVP_DigestUpdate(context.get(), bytes.data(), bytes.size()) != 1)
    throw std::runtime_error("update SHA-256");
  Sha256Digest digest;
  unsigned length = 0;
  if (EVP_DigestFinal_ex(context.get(), digest.data(), &length) != 1 || length != digest.size())
    throw std::runtime_error("finish SHA-256");
  return digest;
}
}  // namespace snapshot::pagebroker::utils
