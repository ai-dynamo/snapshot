// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <filesystem>
#include <string>

#include "pagebroker_types.hpp"
#include "transfer/engine/transfer_engine.hpp"

namespace snapshot::pagebroker::test {
// Construct requests on the test thread before starting workers.
class RequestBuilder {
 public:
  // Each call creates a fresh request ID; reuse the request to retry it unchanged.
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
Configure(StorageBackend* storage, IOEngine* engine, const std::filesystem::path& directory,
    IoEngine engine_type = IoEngine::POSIX_COPY)
{
  storage->mutable_filesystem()->set_directory(directory.string());
  if (engine_type == IoEngine::MODEL_STREAMER)
    engine->mutable_model_streamer();
  else
    engine->mutable_posix_copy();
}
}  // namespace snapshot::pagebroker::test
