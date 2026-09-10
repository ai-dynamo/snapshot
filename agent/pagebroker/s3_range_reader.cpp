// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_range_reader.hpp"

#include <dlfcn.h>
#include <sys/mman.h>
#include <unistd.h>

#include <algorithm>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <map>
#include <limits>
#include <stdexcept>
#include <string_view>
#include <system_error>
#include <vector>

namespace snapshot::pagebroker {
namespace {
constexpr size_t kChunkBytes = 16 << 20;
constexpr size_t kRangesPerRequest = 64;
constexpr size_t kRequestsInFlight = 32;

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

class MappedRanges {
 public:
  explicit MappedRanges(const std::vector<S3WriteRange>& ranges)
  {
    const long page_size = sysconf(_SC_PAGESIZE);
    for (const auto& range : ranges) {
      if (range.destination_offset % page_size || range.length % page_size ||
          range.length > SIZE_MAX - size_)
        throw std::runtime_error("invalid S3 destination range");
      offsets_.push_back(size_);
      size_ += static_cast<size_t>(range.length);
    }
    if (!size_) return;
    address_ = mmap(nullptr, size_, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS,
                    -1, 0);
    if (address_ == MAP_FAILED)
      throw std::system_error(errno, std::generic_category(),
                              "reserve S3 destination mapping");
    for (size_t index = 0; index < ranges.size(); ++index) {
      const auto& range = ranges[index];
      void* mapped = mmap(static_cast<char*>(address_) + offsets_[index],
                          range.length, PROT_READ | PROT_WRITE,
                          MAP_SHARED | MAP_FIXED, range.destination_fd,
                          range.destination_offset);
      if (mapped == MAP_FAILED) {
        const int error = errno;
        munmap(address_, size_);
        address_ = MAP_FAILED;
        throw std::system_error(error, std::generic_category(),
                                "map S3 destination memfd");
      }
    }
  }

  ~MappedRanges()
  {
    if (address_ != MAP_FAILED) munmap(address_, size_);
  }

  void* At(size_t index) const
  {
    return static_cast<char*>(address_) + offsets_.at(index);
  }

 private:
  std::vector<size_t> offsets_;
  size_t size_ = 0;
  void* address_ = MAP_FAILED;
};

struct SourceRange {
  std::string image;
  uint64_t offset;
  uint64_t length;
  size_t first_write;
};

std::vector<SourceRange> Coalesce(
    const std::vector<S3WriteRange>& writes)
{
  std::vector<SourceRange> ranges;
  for (size_t index = 0; index < writes.size(); ++index) {
    const auto& write = writes[index];
    if (!ranges.empty() && ranges.back().image == write.image &&
        ranges.back().length <= UINT64_MAX - ranges.back().offset &&
        ranges.back().offset + ranges.back().length == write.source_offset &&
        write.length <= UINT64_MAX - ranges.back().length) {
      ranges.back().length += write.length;
      continue;
    }
    ranges.push_back(
        {write.image, write.source_offset, write.length, index});
  }
  return ranges;
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

  void Write(const std::string& prefix,
             const std::vector<S3WriteRange>& writes) const
  {
    if (writes.empty()) return;
    MappedRanges mapped(writes);
    const auto ranges = Coalesce(writes);
    void* streamer = nullptr;
    if (start_(&streamer) != 0 || !streamer) throw std::runtime_error("start Model Streamer");
    try {
      if (set_credentials_) ConfigureCredentials(streamer);
      std::map<uint64_t, Submission> submissions;
      size_t first = 0;
      while (first < ranges.size() || !submissions.empty()) {
        if (first == ranges.size()) {
          Receive(streamer, submissions);
          continue;
        }
        if (set_credentials_ && submissions.size() == kRequestsInFlight) {
          Receive(streamer, submissions);
          continue;
        }
        const size_t count = std::min(kRangesPerRequest, ranges.size() - first);
        std::vector<std::string> uris;
        std::vector<const char*> paths;
        std::vector<size_t> offsets, sizes;
        std::vector<std::vector<size_t>> chunks(count);
        std::vector<unsigned> chunk_counts;
        std::vector<size_t*> chunk_data;
        uris.reserve(count);
        paths.reserve(count);
        offsets.reserve(count);
        sizes.reserve(count);
        chunk_counts.reserve(count);
        chunk_data.reserve(count);
        size_t total_chunks = 0;
        for (size_t i = 0; i < count; ++i) {
          const auto& range = ranges[first + i];
          if (range.offset > SIZE_MAX || range.length > SIZE_MAX)
            throw std::runtime_error("invalid S3 range");
          uris.push_back(prefix + "/" + range.image);
          offsets.push_back(static_cast<size_t>(range.offset));
          sizes.push_back(static_cast<size_t>(range.length));
          for (size_t copied = 0; copied < range.length; copied += kChunkBytes)
            chunks[i].push_back(std::min<uint64_t>(
                kChunkBytes, range.length - copied));
          total_chunks += chunks[i].size();
        }
        for (size_t i = 0; i < count; ++i) {
          paths.push_back(uris[i].c_str());
          chunk_counts.push_back(static_cast<unsigned>(chunks[i].size()));
          chunk_data.push_back(chunks[i].data());
        }
        std::vector<void*> destinations(
            count, mapped.At(ranges[first].first_write));
        uint64_t submission = 0;
        if (set_credentials_) {
          if (request_(streamer, &submission, static_cast<unsigned>(count),
                       paths.data(), offsets.data(), sizes.data(),
                       destinations.data(), chunk_counts.data(),
                       chunk_data.data()) != 0 || !submission)
            throw std::runtime_error("submit Model Streamer S3 request");
          submissions.emplace(submission,
              Submission{std::move(chunk_counts), total_chunks, 0});
        } else if (legacy_request_(streamer, static_cast<unsigned>(count),
                                   paths.data(), offsets.data(), sizes.data(),
                                   destinations.data(), chunk_counts.data(),
                                   chunk_data.data(), std::getenv("AWS_ACCESS_KEY_ID"),
                                   std::getenv("AWS_SECRET_ACCESS_KEY"),
                                   std::getenv("AWS_SESSION_TOKEN"), Region(),
                                   std::getenv("AWS_ENDPOINT_URL")) != 0) {
          throw std::runtime_error("submit legacy Model Streamer S3 request");
        }
        if (!set_credentials_) {
          for (size_t completed = 0; completed < total_chunks; ++completed) {
            unsigned file = 0, chunk = 0;
            if (legacy_response_(streamer, &file, &chunk) != 0 ||
                file >= count || chunk >= chunks[file].size())
              throw std::runtime_error("receive legacy Model Streamer S3 response");
          }
        }
        first += count;
      }
      end_(streamer);
    } catch (...) {
      end_(streamer);
      throw;
    }
  }

 private:
  struct Submission {
    std::vector<unsigned> chunk_counts;
    size_t expected = 0;
    size_t completed = 0;
  };

  void Receive(void* streamer, std::map<uint64_t, Submission>& submissions) const
  {
    uint64_t submission_id = 0;
    unsigned file = 0, chunk = 0;
    int done = 0;
    if (response_(streamer, &submission_id, &file, &chunk, &done, 0) != 0)
      throw std::runtime_error("receive Model Streamer S3 response");
    auto submission = submissions.find(submission_id);
    if (submission == submissions.end() ||
        file >= submission->second.chunk_counts.size() ||
        chunk >= submission->second.chunk_counts[file])
      throw std::runtime_error("invalid Model Streamer S3 response");
    ++submission->second.completed;
    if (!done) return;
    if (submission->second.completed != submission->second.expected)
      throw std::runtime_error("Model Streamer S3 completion count mismatch");
    submissions.erase(submission);
  }

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

int WriteS3Ranges(const std::string& prefix,
                  const std::vector<S3WriteRange>& ranges)
{
  if (prefix.rfind("s3://", 0) != 0) return -EINVAL;
  try {
    Streamer().Write(prefix, ranges);
    return 0;
  } catch (const std::exception& error) {
    std::cerr << "PageBroker Streamer S3 read failed: " << error.what() << '\n';
    return -EIO;
  }
}
}  // namespace snapshot::pagebroker
