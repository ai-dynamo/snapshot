// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "pagebroker.hpp"

#include <cstring>
#include <cstdio>
#include <cstdlib>
#include <algorithm>
#include <atomic>
#include <array>
#include <cassert>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <deque>
#include <fstream>
#include <functional>
#include <iomanip>
#include <iostream>
#include <limits>
#include <sstream>
#include <linux/un.h>
#include <linux/memfd.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <fcntl.h>
#include <thread>
#include <syncstream>
#include <unistd.h>
#include <vector>

namespace fs = std::filesystem;
namespace pagebroker {

#ifdef PAGEBROKER_TEST
static std::atomic<unsigned> test_fill_delay_ms{};
void test_set_fill_delay(unsigned milliseconds) {
  test_fill_delay_ms.store(milliseconds);
}
#endif

StagingState::MaterializedObject::~MaterializedObject() {
  if (fd >= 0)
    close(fd);
}

class CopyPool {
public:
  explicit CopyPool(unsigned workers) : worker_count_(workers) {
    workers_.reserve(workers);
    for (unsigned i = 0; i < workers; ++i) {
      workers_.emplace_back([this] {
        for (;;) {
          Task task;
          {
            std::unique_lock lock(mutex_);
            changed_.wait(lock, [this] { return stopping_ || !tasks_.empty(); });
            if (stopping_ && tasks_.empty())
              return;
            task = std::move(tasks_.front());
            tasks_.pop_front();
          }
          task.run();
        }
      });
    }
  }
  ~CopyPool() {
    {
      std::lock_guard lock(mutex_);
      stopping_ = true;
    }
    changed_.notify_all();
    for (auto &worker : workers_)
      worker.join();
  }
  void submit(std::string transaction_id, std::string relative,
              std::function<void()> task) {
    {
      std::lock_guard lock(mutex_);
      tasks_.push_back(
          {std::move(transaction_id), std::move(relative), std::move(task)});
    }
    changed_.notify_one();
  }
  bool prioritize(const std::string &transaction_id,
                  const std::string &relative) {
    std::lock_guard lock(mutex_);
    std::deque<Task> prioritized;
    for (auto task = tasks_.begin(); task != tasks_.end();) {
      if (task->transaction_id == transaction_id &&
          task->relative == relative) {
        prioritized.push_back(std::move(*task));
        task = tasks_.erase(task);
      } else {
        ++task;
      }
    }
    if (prioritized.empty())
      return false;
    while (!prioritized.empty()) {
      tasks_.push_front(std::move(prioritized.back()));
      prioritized.pop_back();
    }
    return true;
  }
  unsigned worker_count() const { return worker_count_; }

private:
  struct Task {
    std::string transaction_id;
    std::string relative;
    std::function<void()> run;
  };
  unsigned worker_count_;
  std::vector<std::thread> workers_;
  std::deque<Task> tasks_;
  std::mutex mutex_;
  std::condition_variable changed_;
  bool stopping_{};
};

#ifdef PAGEBROKER_TEST
bool test_copy_pool_priority() {
  CopyPool pool(1);
  std::mutex mutex;
  std::condition_variable changed;
  bool first_started = false;
  bool release_first = false;
  std::vector<std::string> order;
  pool.submit("tx", "first", [&] {
    std::unique_lock lock(mutex);
    first_started = true;
    changed.notify_one();
    changed.wait(lock, [&] { return release_first; });
    order.push_back("first");
  });
  {
    std::unique_lock lock(mutex);
    changed.wait(lock, [&] { return first_started; });
  }
  pool.submit("tx", "second", [&] {
    std::lock_guard lock(mutex);
    order.push_back("second");
  });
  pool.submit("tx", "requested", [&] {
    std::lock_guard lock(mutex);
    order.push_back("requested-1");
  });
  pool.submit("tx", "requested", [&] {
    std::lock_guard lock(mutex);
    order.push_back("requested-2");
  });
  assert(pool.prioritize("tx", "requested"));
  {
    std::lock_guard lock(mutex);
    release_first = true;
  }
  changed.notify_one();
  for (;;) {
    {
      std::lock_guard lock(mutex);
      if (order.size() == 4)
        return order == std::vector<std::string>{
                            "first", "requested-1", "requested-2", "second"};
    }
    std::this_thread::yield();
  }
}
#endif

namespace {
void put_varint(std::string &out, std::uint64_t value) {
  while (value >= 128) {
    out.push_back(static_cast<char>((value & 127) | 128));
    value >>= 7;
  }
  out.push_back(static_cast<char>(value));
}
void field_varint(std::string &out, int field, std::uint64_t value) {
  put_varint(out, static_cast<std::uint64_t>(field * 8));
  put_varint(out, value);
}
void field_string(std::string &out, int field, const std::string &value) {
  put_varint(out, static_cast<std::uint64_t>(field * 8 + 2));
  put_varint(out, value.size());
  out += value;
}
bool get_varint(const char *&p, const char *end, std::uint64_t &value) {
  value = 0;
  int shift = 0;
  while (p < end && shift < 64) {
    auto b = static_cast<unsigned char>(*p++);
    value |= static_cast<std::uint64_t>(b & 127) << shift;
    if (!(b & 128))
      return true;
    shift += 7;
  }
  return false;
}
bool skip(const char *&p, const char *end, std::uint64_t wire) {
  if (wire == 0) {
    std::uint64_t x;
    return get_varint(p, end, x);
  }
  if (wire == 2) {
    std::uint64_t n;
    return get_varint(p, end, n) && n <= static_cast<std::uint64_t>(end - p) &&
           (p += n);
  }
  return false;
}
Response fail(const std::string &id, const std::string &message) {
  return {false, id, {}, {}, message};
}
fs::path tx_path(const fs::path &root, const std::string &id) {
  return root / "tx" / id;
}
bool safe_id(const std::string &id) {
  return !id.empty() &&
         id.find_first_not_of("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV"
                              "WXYZ0123456789-_") == std::string::npos;
}
std::uint64_t tree_size(const fs::path &path) {
  std::uint64_t total = 0;
  for (auto &e : fs::recursive_directory_iterator(path))
    if (e.is_regular_file())
      total += e.file_size();
  return total;
}
unsigned copy_worker_count() {
  auto cpus = std::max(1u, std::thread::hardware_concurrency());
  return std::min(32u, std::max(8u, cpus * 2));
}
constexpr std::size_t copy_buffer_bytes = 4ULL << 20;
constexpr std::size_t direct_io_alignment = 4096;

char *copy_buffer() {
  thread_local std::unique_ptr<char, decltype(&std::free)> buffer{
      [] {
        void *storage = nullptr;
        if (posix_memalign(&storage, direct_io_alignment, copy_buffer_bytes) != 0)
          throw std::bad_alloc();
        return static_cast<char *>(storage);
      }(),
      &std::free};
  return buffer.get();
}

void coalesce_ranges(HostMemoryObject &object) {
  std::vector<SourceRange> coalesced;
  coalesced.reserve(object.source_ranges.size());
  for (auto &range : object.source_ranges) {
    if (!coalesced.empty()) {
      auto &previous = coalesced.back();
      if (previous.object == range.object &&
          previous.source_offset + previous.length == range.source_offset &&
          previous.dst_offset + previous.length == range.dst_offset) {
        previous.length += range.length;
        continue;
      }
    }
    coalesced.push_back(std::move(range));
  }
  object.source_ranges = std::move(coalesced);
}
void set_staging_error(const std::shared_ptr<StagingState> &state,
                       const std::string &message) {
  {
    std::lock_guard lock(state->mutex);
    if (state->error.empty()) {
      state->error = message;
    }
  }
  state->changed.notify_all();
}
std::string vma_key(std::uint64_t pid, std::uint64_t vma_id,
                    std::uint64_t address, std::uint64_t length) {
  return std::to_string(pid) + ":" + std::to_string(vma_id) + ":" +
         std::to_string(address) + ":" + std::to_string(length);
}

void finish_range(const std::string &transaction_id,
                  const std::shared_ptr<StagingState> &state,
                  std::chrono::steady_clock::time_point transaction_started) {
  bool complete = false;
  bool cancelled = false;
  std::string error;
  {
    std::lock_guard lock(state->mutex);
    if (state->remaining_tasks > 0)
      --state->remaining_tasks;
    if (state->remaining_tasks == 0) {
      state->complete = true;
      complete = true;
      cancelled = state->cancelled;
      error = state->error;
    }
  }
  state->changed.notify_all();
  if (!complete)
    return;
  auto duration = std::chrono::duration_cast<std::chrono::milliseconds>(
      std::chrono::steady_clock::now() - transaction_started);
  std::osyncstream output(std::cerr);
  output << "pagebroker readiness transaction=" << std::quoted(transaction_id)
         << " state=" << (!error.empty() ? "failed" : cancelled ? "cancelled" : "ready")
         << " duration_ms=" << duration.count();
  if (!error.empty())
    output << " error=" << std::quoted(error);
  output << std::endl;
}

void fill_range(const std::string &transaction_id,
                const std::shared_ptr<StagingState> &state,
                const std::shared_ptr<StagingState::MaterializedObject> &object,
                SourceRange range,
                std::chrono::steady_clock::time_point transaction_started) {
  auto started = std::chrono::steady_clock::now();
#ifdef PAGEBROKER_TEST
  if (auto delay = test_fill_delay_ms.load())
    std::this_thread::sleep_for(std::chrono::milliseconds(delay));
#endif
  bool skip_fill;
  {
    std::lock_guard lock(state->mutex);
    skip_fill = state->cancelled || !state->error.empty();
  }
  std::uint64_t copied = 0;
  int error = 0;
  std::string message;
  auto image = state->images.find(range.object);
  if (!skip_fill && image == state->images.end()) {
    error = ENOENT;
    message = "manifest source image is missing";
  }
  int source = -1;
  if (!skip_fill && !error) {
    auto direct = range.source_offset % direct_io_alignment == 0 &&
                  range.length % direct_io_alignment == 0;
    source = open((state->checkpoint_root / image->second.uri).c_str(),
                  O_RDONLY | O_CLOEXEC | (direct ? O_DIRECT : 0));
    if (source < 0) {
      error = errno;
      message = "open manifest source image";
    }
  }
  auto *buffer = copy_buffer();
  while (!skip_fill && !error && copied < range.length) {
    {
      std::lock_guard lock(state->mutex);
      skip_fill = state->cancelled || !state->error.empty();
    }
    if (skip_fill)
      break;
    auto wanted = static_cast<std::size_t>(
        std::min<std::uint64_t>(copy_buffer_bytes, range.length - copied));
    auto count = pread(source, buffer, wanted,
                       static_cast<off_t>(range.source_offset + copied));
    if (count <= 0) {
      error = count < 0 ? errno : EIO;
      message = "read manifest source range";
      break;
    }
    std::size_t written = 0;
    while (!error && written < static_cast<std::size_t>(count)) {
      auto n = pwrite(object->fd, buffer + written,
                      static_cast<std::size_t>(count) - written,
                      static_cast<off_t>(range.dst_offset + copied + written));
      if (n <= 0) {
        error = n < 0 ? errno : EIO;
        message = "write materialized memory range";
      } else {
        written += static_cast<std::size_t>(n);
      }
    }
    copied += written;
  }
  if (source >= 0)
    close(source);
  if (error)
    set_staging_error(state,
                      std::system_error(error, std::generic_category(), message).what());
  auto duration = std::chrono::duration_cast<std::chrono::milliseconds>(
      std::chrono::steady_clock::now() - started);
  std::osyncstream(std::cerr)
      << "pagebroker range transaction=" << std::quoted(transaction_id)
      << " pid=" << object->spec.pid << " vma_id=" << object->spec.vma_id
      << " address=0x" << std::hex << object->spec.dst_addr << std::dec
      << " length=" << object->spec.length
      << " source_image=" << std::quoted(range.object)
      << " source_offset=" << range.source_offset
      << " dst_offset=" << range.dst_offset << " bytes=" << copied
      << " state=" << (skip_fill ? "cancelled" : error ? "failed" : "ready")
      << " duration_ms=" << duration.count() << std::endl;
  finish_range(transaction_id, state, transaction_started);
}

bool start_fills(CopyPool &pool, const std::string &transaction_id,
                 const std::shared_ptr<StagingState> &staging) {
  auto started = std::chrono::steady_clock::now();
  {
    std::lock_guard lock(staging->mutex);
    if (staging->fill_started || staging->cancelled || !staging->error.empty())
      return false;
    staging->fill_started = true;
    if (staging->remaining_tasks == 0) {
      staging->complete = true;
      staging->changed.notify_all();
      return true;
    }
  }
  for (const auto &[key, object] : staging->vmas)
    for (const auto &range : object->spec.source_ranges)
      pool.submit(transaction_id, key,
                  [id = transaction_id, staging, object, range, started] {
                    fill_range(id, staging, object, range, started);
                  });
  for (const auto &[shmid, object] : staging->shared)
    for (const auto &range : object->spec.source_ranges)
      pool.submit(transaction_id, std::to_string(shmid),
                  [id = transaction_id, staging, object, range, started] {
                    fill_range(id, staging, object, range, started);
                  });
  return true;
}

void response_status(std::string &out, std::int32_t status) {
  field_varint(out, 1, static_cast<std::uint64_t>(static_cast<std::int64_t>(status)));
}
bool nested_string(const char *data, std::size_t size, int wanted, std::string &value) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag, n;
    if (!get_varint(p, end, tag)) return false;
    if ((tag >> 3) == static_cast<std::uint64_t>(wanted) && (tag & 7) == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      value.assign(p, p + n);
      return true;
    }
    if (!skip(p, end, tag & 7)) return false;
  }
  return false;
}
bool nested_varint(const char *data, std::size_t size, int wanted,
                   std::uint64_t &value) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag;
    if (!get_varint(p, end, tag)) return false;
    if ((tag >> 3) == static_cast<std::uint64_t>(wanted) && (tag & 7) == 0)
      return get_varint(p, end, value);
    if (!skip(p, end, tag & 7)) return false;
  }
  return false;
}
bool request_fields(const char *data, std::size_t size, std::uint64_t &op,
                    std::string &name, std::uint64_t &flags,
                    std::uint64_t &pid, std::uint64_t &vaddr,
                    std::uint64_t &length, std::uint64_t &shared_id,
                    std::uint64_t &vma_id) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag, n;
    if (!get_varint(p, end, tag)) return false;
    if ((tag >> 3) == 1 && (tag & 7) == 0) {
      if (!get_varint(p, end, op)) return false;
    } else if ((tag >> 3) >= 2 && (tag >> 3) <= 4 && (tag & 7) == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      if ((tag >> 3) == 2) {
        if (!nested_string(p, n, 1, name) || !nested_varint(p, n, 2, flags)) return false;
      } else if ((tag >> 3) == 3) {
        if (!nested_varint(p, n, 1, pid) ||
            !nested_varint(p, n, 3, vaddr) ||
            !nested_varint(p, n, 4, length)) return false;
        // protobuf-c may omit a scalar whose required value is zero.
        // Missing vma_id therefore denotes CRIU's first VMA, ID 0.
        (void)nested_varint(p, n, 2, vma_id);
      } else {
        if (!nested_varint(p, n, 1, shared_id) ||
            !nested_varint(p, n, 2, length)) return false;
      }
      p += n;
    } else if (!skip(p, end, tag & 7)) return false;
  }
  return true;
}

bool decode_image(const char *data, std::size_t size, ImageSpec &image) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag, n;
    if (!get_varint(p, end, tag)) return false;
    auto field = tag >> 3;
    auto wire = tag & 7;
    if ((field == 1 || field == 2) && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      std::string value(p, p + n);
      p += n;
      if (field == 1) image.name = std::move(value);
      else image.uri = std::move(value);
    } else if (field == 3 && wire == 0) {
      if (!get_varint(p, end, image.size)) return false;
    } else if (!skip(p, end, wire)) {
      return false;
    }
  }
  return !image.name.empty() && !image.uri.empty();
}

bool decode_source_range(const char *data, std::size_t size,
                         SourceRange &range) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag, n;
    if (!get_varint(p, end, tag)) return false;
    auto field = tag >> 3;
    auto wire = tag & 7;
    if (field == 1 && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      std::string value(p, p + n);
      p += n;
      range.object = std::move(value);
    } else if (field >= 2 && field <= 4 && wire == 0) {
      std::uint64_t value;
      if (!get_varint(p, end, value)) return false;
      if (field == 2) range.source_offset = value;
      else if (field == 3) range.dst_offset = value;
      else range.length = value;
    } else if (!skip(p, end, wire)) {
      return false;
    }
  }
  return !range.object.empty() && range.length;
}

bool decode_memory_object(const char *data, std::size_t size,
                          HostMemoryObject &object) {
  const char *p = data, *end = data + size;
  while (p < end) {
    std::uint64_t tag, n;
    if (!get_varint(p, end, tag)) return false;
    auto field = tag >> 3;
    auto wire = tag & 7;
    if ((field == 2 || field == 8 || field == 9) && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      std::string value(p, p + n);
      p += n;
      if (field == 2) object.name = std::move(value);
      else if (field == 8) object.semantics = std::move(value);
      else if (field == 9) object.map_mode = std::move(value);
    } else if (field >= 1 && field <= 7 && wire == 0) {
      std::uint64_t value;
      if (!get_varint(p, end, value)) return false;
      if (field == 1) object.memory_id = value;
      else if (field == 3) object.pid = static_cast<std::uint32_t>(value);
      else if (field == 4) object.vma_id = static_cast<std::uint32_t>(value);
      else if (field == 5) object.shmid = value;
      else if (field == 6) object.dst_addr = value;
      else if (field == 7) object.length = value;
    } else if (field == 10 && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p)) return false;
      SourceRange range;
      if (!decode_source_range(p, n, range)) return false;
      object.source_ranges.push_back(std::move(range));
      p += n;
    } else if (!skip(p, end, wire)) {
      return false;
    }
  }
  return object.memory_id && !object.name.empty() && object.length &&
         !object.semantics.empty() && !object.map_mode.empty();
}
} // namespace

bool decode_request(const void *data, std::size_t size, Request &request,
                    std::string &error) {
  const char *p = static_cast<const char *>(data);
  const char *end = p + size;
  bool operation = false;
  while (p < end) {
    std::uint64_t tag;
    if (!get_varint(p, end, tag)) {
      error = "invalid protobuf tag";
      return false;
    }
    int field = tag >> 3;
    auto wire = tag & 7;
    std::uint64_t n;
    if (field == 1 && wire == 0) {
      if (!get_varint(p, end, n))
        return false;
      request.operation = static_cast<Request::Operation>(n);
      operation = true;
    } else if ((field >= 2 && field <= 4) && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p))
        return false;
      std::string v(p, p + n);
      p += n;
      if (field == 2)
        request.transaction_id = v;
      else if (field == 3)
        request.checkpoint_path = v;
      else
        request.manifest_ref = v;
    } else if (field == 5 && wire == 0) {
      if (!get_varint(p, end, request.resident_bytes))
        return false;
    } else if ((field == 6 || field == 7) && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p))
        return false;
      if (field == 6) {
        ImageSpec image;
        if (!decode_image(p, n, image)) {
          error = "invalid manifest image";
          return false;
        }
        request.images.push_back(std::move(image));
      } else {
        HostMemoryObject object;
        if (!decode_memory_object(p, n, object)) {
          error = "invalid host memory object";
          return false;
        }
        request.host_memory_objects.push_back(std::move(object));
      }
      p += n;
    } else if (!skip(p, end, wire)) {
      error = "invalid protobuf field";
      return false;
    }
  }
  if (!operation) {
    error = "operation is required";
    return false;
  }
  return true;
}

std::string encode_response(const Response &r) {
  std::string out;
  field_varint(out, 1, r.ok);
  if (!r.transaction_id.empty())
    field_string(out, 2, r.transaction_id);
  if (!r.staging_path.empty())
    field_string(out, 3, r.staging_path);
  if (!r.scratch_path.empty())
    field_string(out, 4, r.scratch_path);
  if (!r.error.empty())
    field_string(out, 5, r.error);
  return out;
}

TransactionManager::TransactionManager(fs::path staging, fs::path scratch,
                                       std::uint64_t budget)
    : staging_root_(std::move(staging)), scratch_root_(std::move(scratch)),
      budget_(budget),
      defer_fill_(std::getenv("PAGEBROKER_DEFER_FILL") != nullptr),
      copy_pool_(std::make_unique<CopyPool>(copy_worker_count())) {
  cleanup();
}
TransactionManager::~TransactionManager() {
  for (auto &[_, state] : transactions_)
    stop_staging(state, true);
}
void TransactionManager::stop_staging(TransactionState &state, bool cancel) {
  if (state.staging && cancel) {
    {
      std::lock_guard lock(state.staging->mutex);
      state.staging->cancelled = true;
      if (!state.staging->fill_started) {
        state.staging->remaining_tasks = 0;
        state.staging->complete = true;
      }
    }
    state.staging->changed.notify_all();
  }
  if (state.staging) {
    std::unique_lock lock(state.staging->mutex);
    state.staging->changed.wait(lock, [&] {
      return state.staging->complete && !state.staging->provider_running;
    });
  }
}
void TransactionManager::cleanup() {
  for (auto &[_, state] : transactions_)
    stop_staging(state, true);
  auto transaction_root = staging_root_ / "tx";
  if (fs::is_directory(transaction_root)) {
    for (const auto &entry : fs::directory_iterator(transaction_root)) {
      if (!entry.is_regular_file() ||
          entry.path().filename().string().rfind(".checkpoint-", 0) != 0)
        continue;
      std::ifstream metadata(entry.path());
      std::string destination;
      std::getline(metadata, destination);
      if (!destination.empty())
        fs::remove_all(destination);
    }
  }
  fs::remove_all(staging_root_ / "tx");
  if (fs::is_directory(scratch_root_)) {
    for (const auto &entry : fs::directory_iterator(scratch_root_))
      fs::remove_all(entry.path());
  }
  fs::create_directories(staging_root_ / "tx");
  fs::create_directories(scratch_root_);
  transactions_.clear();
  staged_bytes_ = 0;
}
Response TransactionManager::submit(const Request &r) {
  std::lock_guard lock(mutex_);
  if (transactions_.contains(r.transaction_id))
    return fail(r.transaction_id, "transaction is already active");
  if (!safe_id(r.transaction_id))
    return fail(r.transaction_id, "invalid transaction id");
  if (!fs::is_directory(r.checkpoint_path))
    return fail(r.transaction_id, "checkpoint path is not a directory");
  if (r.manifest_ref.empty() || !fs::is_regular_file(r.manifest_ref))
    return fail(r.transaction_id, "manifest_ref is not a regular file");
  std::error_code canonical_error;
  auto checkpoint = fs::weakly_canonical(r.checkpoint_path, canonical_error);
  auto manifest = fs::weakly_canonical(r.manifest_ref, canonical_error);
  if (canonical_error || manifest.parent_path() != checkpoint)
    return fail(r.transaction_id, "manifest_ref is outside the checkpoint directory");
  try {
    auto submit_started = std::chrono::steady_clock::now();
    auto wire_manifest = manifest;
    wire_manifest.replace_extension(".pb");
    std::ifstream wire(wire_manifest, std::ios::binary);
    if (!wire)
      return fail(r.transaction_id, "PageBroker wire manifest is missing");
    std::string wire_data((std::istreambuf_iterator<char>(wire)),
                          std::istreambuf_iterator<char>());
    Request admitted;
    std::string decode_error;
    if (!decode_request(wire_data.data(), wire_data.size(), admitted,
                        decode_error) ||
        admitted.operation != Request::Operation::Submit)
      return fail(r.transaction_id,
                  "invalid PageBroker wire manifest: " + decode_error);
    std::uint64_t required = 0;
    for (const auto &object : admitted.host_memory_objects)
      for (const auto &range : object.source_ranges) {
        if (range.length > std::numeric_limits<std::uint64_t>::max() - required)
          return fail(r.transaction_id, "manifest resident bytes overflow");
        required += range.length;
      }
    if (required != admitted.resident_bytes)
      return fail(r.transaction_id, "manifest resident bytes do not match object total");
    if (staged_bytes_ > budget_ || required > budget_ - staged_bytes_)
      return fail(r.transaction_id, "PageBroker memory budget exceeded");

    auto workers = copy_pool_->worker_count();
    auto staging = std::make_shared<StagingState>();
    staging->checkpoint_root = checkpoint;
    staging->manifest_ref = manifest.string();
    for (const auto &image : admitted.images) {
      fs::path uri(image.uri);
      if (image.name.empty() || image.uri.empty() || uri.is_absolute() ||
          std::find(uri.begin(), uri.end(), fs::path("..")) != uri.end())
        return fail(r.transaction_id, "invalid manifest image path");
      auto path = checkpoint / uri;
      if (!fs::is_regular_file(path) || fs::file_size(path) != image.size)
        return fail(r.transaction_id, "manifest image size mismatch: " + image.name);
      if (!staging->images.emplace(image.name, image).second)
        return fail(r.transaction_id, "duplicate manifest image: " + image.name);
    }

    for (auto &spec : admitted.host_memory_objects) {
      if ((spec.map_mode != "private" && spec.map_mode != "shared") ||
          (spec.map_mode == "private" && spec.semantics != "private_anon") ||
          (spec.map_mode == "shared" && spec.semantics != "shared_anon" &&
           spec.semantics != "shared_memfd"))
        return fail(r.transaction_id, "unsupported manifest memory semantics");
      for (const auto &range : spec.source_ranges) {
        auto image = staging->images.find(range.object);
        if (image == staging->images.end() || !range.length ||
            range.source_offset > image->second.size ||
            range.length > image->second.size - range.source_offset ||
            range.dst_offset > spec.length ||
            range.length > spec.length - range.dst_offset)
          return fail(r.transaction_id, "invalid manifest source range");
      }
      coalesce_ranges(spec);
      auto object = std::make_shared<StagingState::MaterializedObject>();
      object->spec = spec;
      object->fd = syscall(SYS_memfd_create, "pagebroker-host-memory",
                           MFD_ALLOW_SEALING);
      if (object->fd < 0 ||
          ftruncate(object->fd, static_cast<off_t>(spec.length)) < 0)
        throw std::system_error(errno, std::generic_category(),
                                "allocate host memory object");
      if (spec.semantics != "shared_memfd" &&
          fcntl(object->fd, F_ADD_SEALS, F_SEAL_GROW | F_SEAL_SHRINK) < 0)
        throw std::system_error(errno, std::generic_category(),
                                "seal host memory object size");
      if (spec.map_mode == "shared") {
        if (!spec.shmid || !staging->shared.emplace(spec.shmid, object).second)
          return fail(r.transaction_id, "duplicate or missing shared-memory id");
      } else {
        auto key = vma_key(spec.pid, spec.vma_id, spec.dst_addr, spec.length);
        if (!spec.pid || !staging->vmas.emplace(key, object).second)
          return fail(r.transaction_id, "duplicate or missing private VMA identity");
      }
      staging->remaining_tasks += spec.source_ranges.size();
    }

    auto [transaction, inserted] = transactions_.emplace(
        std::piecewise_construct, std::forward_as_tuple(r.transaction_id),
        std::forward_as_tuple());
    (void)inserted;
    auto &state = transaction->second;
    state.checkpoint = checkpoint;
    state.staged_bytes = required;
    state.staging = staging;
    auto tasks = staging->remaining_tasks;
    staged_bytes_ += required;
    if (!defer_fill_)
      start_fills(*copy_pool_, r.transaction_id, staging);
    std::osyncstream(std::cerr)
        << "pagebroker manifest loaded transaction="
        << std::quoted(r.transaction_id) << " manifest_ref="
        << std::quoted(r.manifest_ref) << " checkpoint="
        << std::quoted(checkpoint.string()) << " images=" << admitted.images.size()
        << " host_memory_objects=" << admitted.host_memory_objects.size()
        << " resident_bytes=" << required << " tasks=" << tasks
        << " workers=" << workers << " submit_duration_ms="
        << std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now() - submit_started)
               .count()
        << " fill_mode=" << (defer_fill_ ? "pre-resume" : "eager")
        << std::endl;
    for (const auto &[name, image] : staging->images) {
      if (name.rfind("pages-", 0) != 0)
        std::osyncstream(std::cerr)
            << "pagebroker image direct transaction="
            << std::quoted(r.transaction_id) << " name=" << std::quoted(name)
            << " path=" << std::quoted((checkpoint / image.uri).string())
            << " size=" << image.size << std::endl;
      }
    return {true, r.transaction_id, checkpoint, scratch_root_ / r.transaction_id,
            {}};
  } catch (const std::exception &e) {
    std::cerr << "pagebroker submit failed transaction="
              << std::quoted(r.transaction_id) << " error="
              << std::quoted(e.what()) << std::endl;
    return fail(r.transaction_id, e.what());
  }
}
Response TransactionManager::prepare_checkpoint(const Request &r) {
  std::lock_guard lock(mutex_);
  if (transactions_.contains(r.transaction_id))
    return fail(r.transaction_id, "transaction is already active");
  if (!safe_id(r.transaction_id))
    return fail(r.transaction_id, "invalid transaction id");
  fs::path destination = fs::path(r.checkpoint_path);
  if (!destination.is_absolute() || destination.filename().empty() ||
      destination.filename() == "." || destination.filename() == "..")
    return fail(r.transaction_id, "checkpoint path must be an absolute directory path");
  for (const auto &part : destination)
    if (part == "..")
      return fail(r.transaction_id, "checkpoint path must not contain '..'");
  try {
    fs::create_directories(destination.parent_path());
    auto path = destination.parent_path() /
                ("." + destination.filename().string() + ".pagebroker-" +
                 r.transaction_id);
    if (fs::exists(path))
      return fail(r.transaction_id, "checkpoint transaction path already exists");
    fs::create_directory(path);
    std::ofstream metadata(staging_root_ / "tx" /
                           (".checkpoint-" + r.transaction_id));
    metadata << path.string() << '\n';
    if (!metadata) {
      fs::remove_all(path);
      return fail(r.transaction_id, "failed to record checkpoint transaction");
    }
    transactions_.emplace(r.transaction_id,
                          TransactionState{destination, 0, true, {}});
    std::osyncstream(std::cerr)
        << "pagebroker checkpoint prepared transaction="
        << std::quoted(r.transaction_id) << " destination="
        << std::quoted(destination.string()) << " staging="
        << std::quoted(path.string()) << std::endl;
    return {true, r.transaction_id, path, scratch_root_ / r.transaction_id, {}};
  } catch (const fs::filesystem_error &e) {
    return fail(r.transaction_id, e.what());
  }
}
Response TransactionManager::wait_ready(const Request &r) {
  std::shared_ptr<StagingState> staging;
  {
    std::lock_guard lock(mutex_);
    auto transaction = transactions_.find(r.transaction_id);
    if (transaction == transactions_.end())
      return fail(r.transaction_id, "transaction is not active");
    staging = transaction->second.staging;
  }
  if (staging) {
    if (defer_fill_ && start_fills(*copy_pool_, r.transaction_id, staging))
      std::osyncstream(std::cerr)
          << "pagebroker deferred fill released at pre-resume transaction="
          << std::quoted(r.transaction_id) << std::endl;
    std::unique_lock lock(staging->mutex);
    staging->changed.wait(lock, [&] {
      return staging->complete || staging->cancelled ||
             !staging->error.empty();
    });
    if (!staging->error.empty())
      return fail(r.transaction_id, staging->error);
    if (staging->cancelled)
      return fail(r.transaction_id, "staging cancelled");
  }
  return {true,
          r.transaction_id,
          staging ? staging->checkpoint_root : fs::path{},
          scratch_root_ / r.transaction_id,
          {}};
}
std::shared_ptr<StagingState>
TransactionManager::staging_state(const std::string &transaction_id) {
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(transaction_id);
  return transaction == transactions_.end() ? nullptr
                                             : transaction->second.staging;
}
Response TransactionManager::commit(const Request &r) {
  auto started = std::chrono::steady_clock::now();
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(r.transaction_id);
  if (transaction == transactions_.end())
    return fail(r.transaction_id, "transaction is not active");
  try {
    auto &state = transaction->second;
    if (!state.promote) {
      stop_staging(state, true);
      std::lock_guard staging_lock(state.staging->mutex);
      if (!state.staging->error.empty())
        return fail(r.transaction_id, state.staging->error);
    }
    if (state.promote) {
      auto staged = state.checkpoint.parent_path() /
                    ("." + state.checkpoint.filename().string() +
                     ".pagebroker-" + r.transaction_id);
      auto bytes = tree_size(staged);
      if (bytes >= state.staged_bytes)
        staged_bytes_ += bytes - state.staged_bytes;
      else
        staged_bytes_ -= state.staged_bytes - bytes;
      state.staged_bytes = bytes;
      if (staged_bytes_ > budget_)
        return fail(r.transaction_id, "staging budget exceeded");
      auto backup = state.checkpoint.parent_path() /
                    ("." + state.checkpoint.filename().string() +
                     ".pagebroker-old-" + r.transaction_id);
      if (fs::exists(backup))
        fs::remove_all(backup);
      if (fs::exists(state.checkpoint))
        fs::rename(state.checkpoint, backup);
      try {
        fs::rename(staged, state.checkpoint);
      } catch (...) {
        if (fs::exists(backup) && !fs::exists(state.checkpoint))
          fs::rename(backup, state.checkpoint);
        throw;
      }
      fs::remove_all(backup);
      fs::remove(staging_root_ / "tx" / (".checkpoint-" + r.transaction_id));
    } else {
      fs::remove_all(tx_path(staging_root_, r.transaction_id));
    }
    fs::remove_all(scratch_root_ / r.transaction_id);
  } catch (const fs::filesystem_error &e) {
    return fail(r.transaction_id, e.what());
  }
  auto promote = transaction->second.promote;
  auto checkpoint = transaction->second.checkpoint;
  auto bytes = transaction->second.staged_bytes;
  staged_bytes_ -= bytes;
  transactions_.erase(transaction);
  if (promote) {
    auto duration = std::chrono::duration<double>(
        std::chrono::steady_clock::now() - started);
    std::osyncstream(std::cerr)
        << "pagebroker checkpoint committed transaction="
        << std::quoted(r.transaction_id) << " destination="
        << std::quoted(checkpoint.string()) << " bytes=" << bytes
        << " duration_s=" << std::fixed << std::setprecision(6)
        << duration.count() << std::endl;
  }
  return {true, r.transaction_id, {}, {}, {}};
}
Response TransactionManager::abort(const Request &r) {
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(r.transaction_id);
  if (transaction != transactions_.end()) {
    try {
      stop_staging(transaction->second, true);
      if (transaction->second.promote)
        fs::remove_all(transaction->second.checkpoint.parent_path() /
                      ("." + transaction->second.checkpoint.filename().string() +
                       ".pagebroker-" + r.transaction_id));
      else
        fs::remove_all(tx_path(staging_root_, r.transaction_id));
      fs::remove(staging_root_ / "tx" / (".checkpoint-" + r.transaction_id));
      fs::remove_all(scratch_root_ / r.transaction_id);
    } catch (const fs::filesystem_error &e) {
      return fail(r.transaction_id, e.what());
    }
    staged_bytes_ -= transaction->second.staged_bytes;
    transactions_.erase(transaction);
  }
  return {true, r.transaction_id, {}, {}, {}};
}

int serve(const fs::path &socket_path, const fs::path &staging,
          const fs::path &scratch, std::uint64_t budget) {
  fs::create_directories(socket_path.parent_path());
  unlink(socket_path.c_str());
  int server = socket(AF_UNIX, SOCK_SEQPACKET, 0);
  if (server < 0)
    return 1;
  sockaddr_un addr{};
  addr.sun_family = AF_UNIX;
  std::strncpy(addr.sun_path, socket_path.c_str(), sizeof(addr.sun_path) - 1);
  if (bind(server, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)) < 0 ||
      listen(server, 8) < 0)
    return 1;
  chmod(socket_path.c_str(), 0660);
  std::thread health([] {
    int s = socket(AF_INET, SOCK_STREAM, 0);
    sockaddr_in a{};
    a.sin_family = AF_INET;
    a.sin_addr.s_addr = htonl(INADDR_ANY);
    const char *port = std::getenv("PAGEBROKER_HEALTH_PORT");
    a.sin_port = htons(port ? static_cast<unsigned short>(std::stoi(port)) : 8080);
    int yes = 1;
    setsockopt(s, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));
    if (bind(s, reinterpret_cast<sockaddr *>(&a), sizeof(a)) < 0 ||
        listen(s, 8) < 0)
      return;
    for (;;) {
      int c = accept(s, nullptr, nullptr);
      if (c < 0)
        continue;
      char req[256];
      auto n = read(c, req, sizeof(req) - 1);
      std::string path = n > 0 ? std::string(req, req + n) : "";
      std::string body = path.find("/metrics") != std::string::npos
                             ? "pagebroker_transactions_active 0\n"
                             : "ok\n";
      std::string status = path.find("/healthz") != std::string::npos ||
                                   path.find("/readyz") != std::string::npos ||
                                   path.find("/metrics") != std::string::npos
                               ? "200 OK"
                               : "404 Not Found";
      std::string response = "HTTP/1.1 " + status + "\r\nContent-Length: " +
                             std::to_string(body.size()) +
                             "\r\nConnection: close\r\n\r\n" + body;
      send(c, response.data(), response.size(), MSG_NOSIGNAL);
      close(c);
    }
  });
  health.detach();
  TransactionManager manager(staging, scratch, budget);
  std::cerr << "pagebroker listening on " << socket_path << " (staging="
            << staging << ", scratch=" << scratch << ", budget=" << budget
            << ")" << std::endl;
  for (;;) {
    int client = accept(server, nullptr, nullptr);
    if (client < 0)
      continue;
    std::thread([client, &manager] {
      char buffer[65536];
      auto n = recv(client, buffer, sizeof(buffer), 0);
      Request request;
      std::string error;
      Response response;
      bool provider_session = false;
      fs::path provider_root;
      if (n < 0 || !decode_request(buffer, n, request, error))
        response = fail({}, error.empty() ? "read failed" : error);
      else if (request.operation == Request::Operation::Submit) {
        response = manager.submit(request);
        if (response.ok) {
          provider_session = true;
          provider_root = response.staging_path;
        }
      } else if (request.operation == Request::Operation::WaitReady)
        response = manager.wait_ready(request);
      else if (request.operation == Request::Operation::PrepareCheckpoint)
        response = manager.prepare_checkpoint(request);
      else if (request.operation == Request::Operation::Commit)
        response = manager.commit(request);
      else if (request.operation == Request::Operation::Abort)
        response = manager.abort(request);
      else
        response = fail(request.transaction_id, "unknown operation");
      auto encoded = encode_response(response);
      auto sent = send(client, encoded.data(), encoded.size(), MSG_NOSIGNAL);
      if (provider_session && sent == static_cast<ssize_t>(encoded.size())) {
        auto staging_state = manager.staging_state(request.transaction_id);
        if (staging_state) {
          {
            std::lock_guard lock(staging_state->mutex);
            staging_state->provider_running = true;
          }
          std::osyncstream(std::cerr)
              << "pagebroker provider session start transaction="
              << std::quoted(request.transaction_id) << " root="
              << std::quoted(provider_root.string()) << std::endl;
          auto status = serve_provider(provider_root, client, -1, staging_state);
          std::osyncstream(std::cerr)
              << "pagebroker provider session stop transaction="
              << std::quoted(request.transaction_id) << " status=" << status
              << std::endl;
          {
            std::lock_guard lock(staging_state->mutex);
            staging_state->provider_running = false;
          }
          staging_state->changed.notify_all();
          if (status != 0) {
            Request abort{Request::Operation::Abort, request.transaction_id,
                          fs::path{}};
            auto aborted = manager.abort(abort);
            std::osyncstream(std::cerr)
                << "pagebroker provider disconnect abort transaction="
                << std::quoted(request.transaction_id)
                << " ok=" << aborted.ok << std::endl;
          }
        }
      }
      close(client);
    }).detach();
  }
}

int provider_failure(int diagnostic_fd, const char *operation, int err) {
  if (diagnostic_fd >= 0)
    dprintf(diagnostic_fd, "failure operation=%s errno=%d (%s)\n", operation,
            err, std::strerror(err));
  return err ? err : 1;
}

int serve_provider(const fs::path &root, int socket_fd, int diagnostic_fd,
                   std::shared_ptr<StagingState> staging) {
  struct ProviderTiming {
    std::uint64_t count{};
    std::uint64_t total_ns{};
    std::uint64_t max_ns{};
    std::uint64_t bytes{};
    std::uint64_t decode_ns{};
    std::uint64_t log_ns{};
    std::uint64_t readiness_ns{};
    std::uint64_t open_ns{};
    std::uint64_t memfd_ns{};
    std::uint64_t truncate_ns{};
    std::uint64_t seal_ns{};
    std::uint64_t send_ns{};
  };
  std::array<ProviderTiming, 8> timings{};
  bool timings_printed = false;
  auto elapsed_ns = [](auto started) {
    return static_cast<std::uint64_t>(
        std::chrono::duration_cast<std::chrono::nanoseconds>(
            std::chrono::steady_clock::now() - started)
            .count());
  };
  auto seconds = [](std::uint64_t ns) {
    return static_cast<double>(ns) / 1'000'000'000.0;
  };
  auto operation_name = [](std::uint64_t op) {
    switch (op) {
    case 1: return "INIT";
    case 2: return "OPEN_IMAGE";
    case 3: return "GET_VMA";
    case 4: return "GET_SHARED";
    case 5: return "COMMIT";
    case 6: return "ABORT";
    case 7: return "WAIT_READY";
    default: return "UNKNOWN";
    }
  };
  auto print_timings = [&] {
    if (timings_printed)
      return;
    timings_printed = true;
    for (std::size_t op = 1; op < timings.size(); ++op) {
      const auto &timing = timings[op];
      if (!timing.count)
        continue;
      std::osyncstream(std::cerr)
          << "pagebroker provider timing op=" << operation_name(op)
          << " count=" << timing.count << std::fixed << std::setprecision(6)
          << " total_s=" << seconds(timing.total_ns)
          << " avg_s=" << seconds(timing.total_ns) / timing.count
          << " max_s=" << seconds(timing.max_ns)
          << " bytes=" << timing.bytes
          << " decode_s=" << seconds(timing.decode_ns)
          << " log_s=" << seconds(timing.log_ns)
          << " readiness_path_s=" << seconds(timing.readiness_ns)
          << " open_s=" << seconds(timing.open_ns)
          << " memfd_s=" << seconds(timing.memfd_ns)
          << " truncate_s=" << seconds(timing.truncate_ns)
          << " seal_s=" << seconds(timing.seal_ns)
          << " send_s=" << seconds(timing.send_ns) << std::endl;
    }
  };
  struct stat socket_stat {};
  if (fstat(socket_fd, &socket_stat) < 0)
    return provider_failure(diagnostic_fd, "fstat socket", errno);
  if (!S_ISSOCK(socket_stat.st_mode))
    return provider_failure(diagnostic_fd, "fstat socket", ENOTSOCK);
  if (diagnostic_fd >= 0)
    dprintf(diagnostic_fd, "ready socket_fd=%d fstat=ok\n", socket_fd);

  std::uint64_t request_counts[8] = {};
  bool terminal_request = false;
  char buffer[1 << 20];
  for (;;) {
    auto n = recv(socket_fd, buffer, sizeof(buffer), 0);
    if (n == 0) {
      if (staging && !terminal_request) {
        set_staging_error(staging,
                          "provider disconnected before COMMIT or ABORT");
        print_timings();
        return ECONNRESET;
      }
      print_timings();
      return 0;
    }
    if (n < 0) {
      if (staging && !terminal_request)
        set_staging_error(staging,
                          "provider connection failed before COMMIT or ABORT");
      print_timings();
      return provider_failure(diagnostic_fd, "recv", errno);
    }
    std::uint64_t op = 0, flags = 0, pid = 0, vaddr = 0;
    std::string name;
    std::string response;
    int fd = -1;
    std::uint64_t length = 0, shared_id = 0, vma_id = 0;
    auto request_started = std::chrono::steady_clock::now();
    auto decode_started = std::chrono::steady_clock::now();
    auto decoded = request_fields(buffer, n, op, name, flags, pid, vaddr,
                                  length, shared_id, vma_id);
    auto decode_ns = elapsed_ns(decode_started);
    ProviderTiming *timing = op < timings.size() ? &timings[op] : nullptr;
    if (timing) {
      timing->count++;
      timing->decode_ns += decode_ns;
      if (op == 3 || op == 4)
        timing->bytes += length;
    }
    if (!decoded) {
      std::osyncstream(std::cerr)
          << "pagebroker provider request decode_failed root="
          << std::quoted(root.string()) << " bytes=" << n << std::endl;
      response_status(response, -EBADMSG);
    } else if (op < 8) {
      request_counts[op]++;
      auto log_started = std::chrono::steady_clock::now();
      std::ostringstream request_log;
      request_log << "pagebroker provider request root="
                  << std::quoted(root.string()) << " op=";
      if (op == 1)
        request_log << "INIT";
      else if (op == 2)
        request_log << "OPEN_IMAGE path=" << std::quoted(name) << " flags=0x"
                    << std::hex << flags << std::dec;
      else if (op == 3)
        request_log << "GET_VMA pid=" << pid << " vma_id=" << vma_id
                    << " vaddr=0x" << std::hex << vaddr << std::dec
                    << " length=" << length;
      else if (op == 4)
        request_log << "GET_SHARED shm_id=" << shared_id
                    << " length=" << length;
      else if (op == 5)
        request_log << "COMMIT";
      else if (op == 6)
        request_log << "ABORT";
      else if (op == 7)
        request_log << "WAIT_READY";
      std::osyncstream(std::cerr) << request_log.str() << std::endl;
      timing->log_ns += elapsed_ns(log_started);
      if (op == 1 || op == 5 || op == 6) {
        if (op == 5) {
          terminal_request = true;
        } else if (op == 6 && staging) {
          terminal_request = true;
          {
            std::lock_guard lock(staging->mutex);
            staging->cancelled = true;
          }
          staging->changed.notify_all();
        }
        response_status(response, 0);
      } else if (op == 3) {
        if (!staging) {
          response_status(response, -ENOTSUP);
        } else {
          std::shared_ptr<StagingState::MaterializedObject> object;
          {
            std::lock_guard lock(staging->mutex);
            auto found = staging->vmas.find(vma_key(pid, vma_id, vaddr, length));
            if (found != staging->vmas.end())
              object = found->second;
          }
          fd = object ? dup(object->fd) : -1;
          response_status(response, object ? (fd < 0 ? -errno : 0) : -ENOTSUP);
        }
      } else if (op == 4) {
        std::shared_ptr<StagingState::MaterializedObject> object;
        if (staging) {
          std::lock_guard lock(staging->mutex);
          auto found = staging->shared.find(shared_id);
          if (found != staging->shared.end() && found->second->spec.length == length)
            object = found->second;
        }
        fd = object ? dup(object->fd) : -1;
        response_status(response, object ? (fd < 0 ? -errno : 0) : -ENOTSUP);
      } else if (op == 7) {
        if (!staging) {
          response_status(response, -ENOTSUP);
        } else {
          auto readiness_started = std::chrono::steady_clock::now();
          std::unique_lock lock(staging->mutex);
          staging->changed.wait(lock, [&] {
            return staging->complete || staging->cancelled ||
                   !staging->error.empty();
          });
          timing->readiness_ns += elapsed_ns(readiness_started);
          response_status(response, !staging->error.empty()
                                        ? -EIO
                                        : staging->cancelled ? -ECANCELED : 0);
        }
      } else if (op == 2) {
        fs::path relative(name);
        if (name.empty() || relative.is_absolute() ||
            std::find(relative.begin(), relative.end(), fs::path("..")) != relative.end()) {
          response_status(response, -EINVAL);
        } else {
          int ready = 0;
          if (staging) {
            std::lock_guard lock(staging->mutex);
            auto image = staging->images.find(relative.generic_string());
            if (image == staging->images.end()) {
              ready = -ENOENT;
            } else {
              relative = image->second.uri;
            }
          }
          if (ready < 0) {
            response_status(response, ready == -ENOENT ? -ENOTSUP : ready);
            ready = -1;
          }
          if (ready == 0) {
            auto open_started = std::chrono::steady_clock::now();
            fd = open((root / relative).c_str(), static_cast<int>(flags));
            timing->open_ns += elapsed_ns(open_started);
          }
          // CRIU sends every image open through the provider, including optional
          // images that are not present in this checkpoint. ENOTSUP asks CRIU to
          // use its normal local open path, which preserves its missing-image
          // handling; ENOENT would instead terminate the restore.
          if (ready == 0)
            response_status(response,
                            fd < 0 ? (errno == ENOENT ? -ENOTSUP : -errno) : 0);
        }
      } else {
        response_status(response, -ENOTSUP);
      }
    } else {
      response_status(response, -ENOTSUP);
    }
    if (op == 5 || op == 6) {
      std::cerr << "pagebroker provider requests init=" << request_counts[1]
                << " image=" << request_counts[2]
                << " vma=" << request_counts[3]
                << " shared=" << request_counts[4]
                << " wait_ready=" << request_counts[7] << std::endl;
    }
    struct iovec iov{response.data(), response.size()};
    char control[CMSG_SPACE(sizeof(int))] = {};
    struct msghdr msg{};
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;
    if (fd >= 0) {
      msg.msg_control = control;
      msg.msg_controllen = sizeof(control);
      auto *cmsg = CMSG_FIRSTHDR(&msg);
      cmsg->cmsg_level = SOL_SOCKET;
      cmsg->cmsg_type = SCM_RIGHTS;
      cmsg->cmsg_len = CMSG_LEN(sizeof(fd));
      std::memcpy(CMSG_DATA(cmsg), &fd, sizeof(fd));
    }
    auto send_started = std::chrono::steady_clock::now();
    auto send_result = sendmsg(socket_fd, &msg, MSG_NOSIGNAL);
    if (timing) {
      timing->send_ns += elapsed_ns(send_started);
      auto total_ns = elapsed_ns(request_started);
      timing->total_ns += total_ns;
      timing->max_ns = std::max(timing->max_ns, total_ns);
    }
    if (send_result < 0) {
      print_timings();
      return provider_failure(diagnostic_fd, "sendmsg", errno);
    }
    if (fd >= 0) close(fd);
    if (op == 5 || op == 6) {
      print_timings();
      return op == 6 ? ECANCELED : 0;
    }
  }
}

} // namespace pagebroker

#ifndef PAGEBROKER_TEST
int main(int argc, char **argv) {
  if (argc == 5 && std::string(argv[1]) == "provider")
    return pagebroker::serve_provider(argv[2], std::stoi(argv[3]), std::stoi(argv[4]));
  if (argc != 2 || std::string(argv[1]) != "serve")
    return 2;
  const char *configured = std::getenv("PAGEBROKER_MEMORY_BUDGET_BYTES");
  std::uint64_t budget = configured ? std::stoull(configured) : 150323855360ULL;
  return pagebroker::serve("/run/pagebroker/pagebroker.sock", "/staging",
                           "/scratch", budget);
}
#endif
