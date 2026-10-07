// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <filesystem>
#include <string>

#include "pagebroker_types.hpp"

namespace snapshot::pagebroker::test {
class RequestBuilder {
 public:
  Request RequestFor(const std::string& transaction_id)
  {
    Request request;
    request.set_request_id("request-" + transaction_id + "-" + std::to_string(++sequence_));
    request.set_transaction_id(transaction_id);
    return request;
  }

 private:
  unsigned sequence_ = 0;
};

inline void
Configure(StorageBackend* storage, IOEngine* engine, const std::filesystem::path& directory)
{
  storage->mutable_filesystem()->set_directory(directory.string());
  engine->mutable_posix_copy();
}
}  // namespace snapshot::pagebroker::test
