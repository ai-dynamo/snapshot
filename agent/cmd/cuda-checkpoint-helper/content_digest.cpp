/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "content_digest.h"

#include <openssl/evp.h>

#include <array>

namespace cuda_checkpoint_storage {

ContentDigest::ContentDigest() : context_(EVP_MD_CTX_new()) {
  if (context_ != nullptr &&
      EVP_DigestInit_ex(context_, EVP_sha256(), nullptr) != 1) {
    EVP_MD_CTX_free(context_);
    context_ = nullptr;
  }
}

ContentDigest::~ContentDigest() { EVP_MD_CTX_free(context_); }

bool ContentDigest::Update(const void *data, size_t size, std::string *error) {
  if (error == nullptr) {
    return false;
  }
  if (context_ == nullptr || finalized_) {
    *error = "SHA-256 digest context is unavailable";
    return false;
  }
  if ((data == nullptr && size != 0) ||
      EVP_DigestUpdate(context_, data, size) != 1) {
    *error = "update SHA-256 digest failed";
    return false;
  }
  return true;
}

bool ContentDigest::Finalize(std::string *hex_digest, std::string *error) {
  if (hex_digest == nullptr || error == nullptr) {
    return false;
  }
  if (context_ == nullptr || finalized_) {
    *error = "SHA-256 digest context is unavailable";
    return false;
  }
  std::array<unsigned char, EVP_MAX_MD_SIZE> digest{};
  unsigned int digest_size = 0;
  if (EVP_DigestFinal_ex(context_, digest.data(), &digest_size) != 1 ||
      digest_size != 32) {
    *error = "finalize SHA-256 digest failed";
    return false;
  }
  static constexpr char kHex[] = "0123456789abcdef";
  hex_digest->clear();
  hex_digest->reserve(digest_size * 2);
  for (unsigned int index = 0; index < digest_size; ++index) {
    hex_digest->push_back(kHex[digest[index] >> 4]);
    hex_digest->push_back(kHex[digest[index] & 0x0f]);
  }
  finalized_ = true;
  return true;
}

bool IsSHA256Hex(const std::string &value) {
  if (value.size() != 64) {
    return false;
  }
  for (const char character : value) {
    if (!((character >= '0' && character <= '9') ||
          (character >= 'a' && character <= 'f'))) {
      return false;
    }
  }
  return true;
}

} // namespace cuda_checkpoint_storage
