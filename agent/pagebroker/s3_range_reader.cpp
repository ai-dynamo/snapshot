// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_range_reader.hpp"

#include <dlfcn.h>

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <stdexcept>
#include <string_view>
#include <vector>

namespace snapshot::pagebroker {
namespace {
constexpr size_t kChunkBytes = 16 << 20;

template <typename Function>
Function Symbol(void* library, const char* name)
{
  void* address = dlsym(library, name);
  if (!address) throw std::runtime_error(std::string("Model Streamer is missing ") + name);
  Function function{};
  static_assert(sizeof(function) == sizeof(address));
  std::memcpy(&function, &address, sizeof(function));
  return function;
}

class Streamer {
 public:
  using Start = int (*)(void**);
  using End = void (*)(void*);
  using SetCredentialsFn = int (*)(void*, const char**, const char**, unsigned);
  using Request = int (*)(void*, uint64_t*, unsigned, const char**, size_t*,
                          size_t*, void**, unsigned*, size_t**);
  using Response = int (*)(void*, uint64_t*, unsigned*, unsigned*, int*, unsigned);
  using LegacyRequest = int (*)(void*, unsigned, const char**, size_t*,
                                size_t*, void**, unsigned*, size_t**,
                                const char*, const char*, const char*,
                                const char*, const char*);
  using LegacyResponse = int (*)(void*, unsigned*, unsigned*);

  Streamer()
  {
    const char* path = std::getenv("PAGEBROKER_MODEL_STREAMER_LIBRARY");
    library_ = dlopen(path && path[0] ? path : "libstreamer.so", RTLD_NOW | RTLD_LOCAL);
    if (!library_) throw std::runtime_error(std::string("load Model Streamer: ") + dlerror());
    start_ = Symbol<Start>(library_, "runai_start");
    end_ = Symbol<End>(library_, "runai_end");
    set_credentials_ = OptionalSymbol<SetCredentialsFn>(library_,
                                                         "runai_set_credentials");
    if (set_credentials_) {
      request_ = Symbol<Request>(library_, "runai_request");
      response_ = Symbol<Response>(library_, "runai_response");
    } else {
      legacy_request_ = Symbol<LegacyRequest>(library_, "runai_request");
      legacy_response_ = Symbol<LegacyResponse>(library_, "runai_response");
    }
  }

  ~Streamer() { if (library_) dlclose(library_); }

  void Read(const std::string& uri, uint64_t offset, void* buffer, size_t length) const
  {
    if (offset > SIZE_MAX || length > SIZE_MAX) throw std::runtime_error("S3 range exceeds address space");
    void* streamer = nullptr;
    if (start_(&streamer) != 0 || !streamer) throw std::runtime_error("start Model Streamer");
    try {
      const char* paths[] = {uri.c_str()};
      size_t source_offsets[] = {static_cast<size_t>(offset)};
      size_t sizes[] = {length};
      void* destinations[] = {buffer};
      std::vector<size_t> chunks;
      for (size_t copied = 0; copied < length; copied += kChunkBytes)
        chunks.push_back(std::min(kChunkBytes, length - copied));
      unsigned chunk_counts[] = {static_cast<unsigned>(chunks.size())};
      size_t* chunk_data[] = {chunks.data()};
      uint64_t submission = 0;
      if (set_credentials_) {
        ConfigureCredentials(streamer);
        if (request_(streamer, &submission, 1, paths, source_offsets, sizes,
                     destinations, chunk_counts, chunk_data) != 0 || !submission)
          throw std::runtime_error("submit Model Streamer S3 request");
      } else if (legacy_request_(streamer, 1, paths, source_offsets, sizes,
                                 destinations, chunk_counts, chunk_data,
                                 std::getenv("AWS_ACCESS_KEY_ID"),
                                 std::getenv("AWS_SECRET_ACCESS_KEY"),
                                 std::getenv("AWS_SESSION_TOKEN"),
                                 Region(), std::getenv("AWS_ENDPOINT_URL")) != 0) {
        throw std::runtime_error("submit legacy Model Streamer S3 request");
      }
      size_t completed = 0;
      while (completed < chunks.size()) {
        uint64_t received = 0;
        unsigned file = 0, chunk = 0;
        int done = 0;
        if (set_credentials_) {
          if (response_(streamer, &received, &file, &chunk, &done, 0) != 0 ||
              received != submission || file != 0 || chunk >= chunks.size())
            throw std::runtime_error("receive Model Streamer S3 response");
        } else if (legacy_response_(streamer, &file, &chunk) != 0 || file != 0 ||
                   chunk >= chunks.size()) {
          throw std::runtime_error("receive legacy Model Streamer S3 response");
        }
        ++completed;
        if (done && completed != chunks.size())
          throw std::runtime_error("incomplete Model Streamer S3 response");
      }
      end_(streamer);
    } catch (...) {
      end_(streamer);
      throw;
    }
  }

 private:
  template <typename Function>
  static Function OptionalSymbol(void* library, const char* name)
  {
    void* address = dlsym(library, name);
    if (!address) return {};
    Function function{};
    static_assert(sizeof(function) == sizeof(address));
    std::memcpy(&function, &address, sizeof(function));
    return function;
  }

  static const char* Region()
  {
    const char* region = std::getenv("AWS_REGION");
    return region ? region : std::getenv("AWS_DEFAULT_REGION");
  }

  void ConfigureCredentials(void* streamer) const
  {
    const char* keys[] = {"access_key_id", "secret_access_key", "session_token", "region", "endpoint"};
    const char* values[] = {std::getenv("AWS_ACCESS_KEY_ID"), std::getenv("AWS_SECRET_ACCESS_KEY"),
      std::getenv("AWS_SESSION_TOKEN"), Region(),
      std::getenv("AWS_ENDPOINT_URL")};
    std::vector<const char*> configured_keys, configured_values;
    for (size_t i = 0; i < std::size(keys); ++i) if (values[i] && values[i][0]) {
      configured_keys.push_back(keys[i]); configured_values.push_back(values[i]);
    }
    if (set_credentials_(streamer, configured_keys.data(), configured_values.data(),
                         static_cast<unsigned>(configured_keys.size())) != 0)
      throw std::runtime_error("set Model Streamer credentials");
  }

  void* library_{};
  Start start_{};
  End end_{};
  SetCredentialsFn set_credentials_{};
  Request request_{};
  Response response_{};
  LegacyRequest legacy_request_{};
  LegacyResponse legacy_response_{};
};
}

bool S3RangeReaderEnabled()
{
  const char* mover = std::getenv("PAGEBROKER_S3_MOVER");
  return mover && std::string_view(mover) == "model-streamer";
}

int ReadS3Range(const std::string& prefix, const char* image, uint64_t offset,
                void* buffer, size_t length)
{
  if (prefix.rfind("s3://", 0) != 0 || !image || !buffer || !length) return -EINVAL;
  try {
    Streamer().Read(prefix + "/" + image, offset, buffer, length);
    return 0;
  } catch (const std::exception& error) {
    std::cerr << "PageBroker Streamer S3 read failed: " << error.what() << '\n';
    return -EIO;
  }
}
}  // namespace snapshot::pagebroker
