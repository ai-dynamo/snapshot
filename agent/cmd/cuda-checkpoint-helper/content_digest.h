/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstddef>
#include <memory>
#include <string>

typedef struct evp_md_ctx_st EVP_MD_CTX;

namespace cuda_checkpoint_storage {

class ContentDigest {
public:
  ContentDigest();
  ContentDigest(const ContentDigest &) = delete;
  ContentDigest &operator=(const ContentDigest &) = delete;
  ~ContentDigest();

  bool Update(const void *data, size_t size, std::string *error);
  bool Finalize(std::string *hex_digest, std::string *error);

private:
  EVP_MD_CTX *context_ = nullptr;
  bool finalized_ = false;
};

bool IsSHA256Hex(const std::string &value);

} // namespace cuda_checkpoint_storage
