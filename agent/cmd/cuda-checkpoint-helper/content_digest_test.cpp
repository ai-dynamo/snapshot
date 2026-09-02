/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "content_digest.h"

#include <iostream>
#include <string>

namespace storage = cuda_checkpoint_storage;

int main() {
  std::string error;
  std::string digest;
  storage::ContentDigest sha256;
  const std::string contents = "abc";
  if (!sha256.Update(contents.data(), contents.size(), &error) ||
      !sha256.Finalize(&digest, &error)) {
    std::cerr << error << "\n";
    return 1;
  }
  if (digest !=
      "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" ||
      !storage::IsSHA256Hex(digest) ||
      storage::IsSHA256Hex(std::string(64, 'A')) ||
      storage::IsSHA256Hex(std::string(63, 'a'))) {
    std::cerr << "SHA-256 digest validation failed\n";
    return 1;
  }
  std::cout << "CUDA transfer content digest tests passed\n";
  return 0;
}
