// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "pagebroker.hpp"

#include <cstring>
#include <cstdio>
#include <cstdlib>
#include <algorithm>
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
struct CopyEntry {
  fs::path source;
  fs::path destination;
  std::string relative;
  std::uint64_t bytes;
};
struct CopyPlan {
  std::vector<CopyEntry> files;
  std::uint64_t bytes{};
};
CopyPlan plan_copy_tree(const fs::path &from, const fs::path &to) {
  CopyPlan plan;
  fs::create_directories(to);
  for (const auto &entry : fs::recursive_directory_iterator(from)) {
    auto relative = fs::relative(entry.path(), from);
    auto destination = to / relative;
    if (entry.is_directory()) {
      fs::create_directories(destination);
    } else if (entry.is_regular_file()) {
      auto bytes = entry.file_size();
      fs::create_directories(destination.parent_path());
      plan.files.push_back(
          {entry.path(), destination, relative.generic_string(), bytes});
      plan.bytes += bytes;
    }
  }
  std::sort(plan.files.begin(), plan.files.end(),
            [](const CopyEntry &left, const CopyEntry &right) {
              if (left.bytes != right.bytes)
                return left.bytes < right.bytes;
              return left.relative < right.relative;
            });
  return plan;
}
unsigned copy_worker_count() {
  auto cpus = std::max(1u, std::thread::hardware_concurrency());
  return std::min(32u, std::max(8u, cpus * 2));
}
constexpr std::uint64_t copy_task_bytes = 16ULL << 20;
constexpr std::size_t copy_buffer_bytes = 1ULL << 20;
void set_staging_error(const std::shared_ptr<StagingState> &state, int error,
                       const std::string &message) {
  {
    std::lock_guard lock(state->mutex);
    if (state->error.empty()) {
      state->error_code = error ? error : EIO;
      state->error = message;
    }
  }
  state->changed.notify_all();
}
void finish_copy_task(const CopyEntry &entry,
                      const std::string &transaction_id,
                      const fs::path &staging,
                      const std::shared_ptr<StagingState> &state,
                      std::chrono::steady_clock::time_point started,
                      bool copied) {
  bool complete = false;
  bool file_ready = false;
  std::uint64_t copied_bytes = 0;
  std::string error;
  bool cancelled = false;
  {
    std::lock_guard lock(state->mutex);
    if (copied) {
      auto remaining = state->remaining_chunks.find(entry.relative);
      if (remaining != state->remaining_chunks.end() &&
          --remaining->second == 0) {
        auto partial = entry.destination.string() + ".partial";
        std::error_code rename_error;
        fs::rename(partial, entry.destination, rename_error);
        if (rename_error) {
          if (state->error.empty()) {
            state->error_code = rename_error.value();
            state->error = rename_error.message();
          }
        } else {
          state->copied_bytes += entry.bytes;
          state->ready_files.insert(entry.relative);
          file_ready = true;
        }
      }
    }
    if (state->remaining_tasks > 0)
      --state->remaining_tasks;
    if (state->remaining_tasks == 0) {
      state->complete = true;
      complete = true;
      copied_bytes = state->copied_bytes;
      error = state->error;
      cancelled = state->cancelled;
    }
  }
  state->changed.notify_all();
  if (file_ready)
    std::osyncstream(std::cerr)
        << "pagebroker stage file transaction="
        << std::quoted(transaction_id) << " path="
        << std::quoted(entry.relative) << " bytes=" << entry.bytes
        << std::endl;
  if (!complete)
    return;
  auto elapsed = std::chrono::duration_cast<std::chrono::milliseconds>(
      std::chrono::steady_clock::now() - started);
  if (error.empty() && !cancelled) {
    std::osyncstream(std::cerr)
        << "pagebroker stage complete transaction="
        << std::quoted(transaction_id) << " staging="
        << std::quoted(staging.string()) << " bytes=" << copied_bytes
        << " duration_ms=" << elapsed.count() << std::endl;
  } else if (!cancelled) {
    std::osyncstream(std::cerr)
        << "pagebroker stage failed transaction="
        << std::quoted(transaction_id) << " error="
        << std::quoted(error) << std::endl;
  }
}
void copy_chunk(CopyEntry entry, std::uint64_t offset, std::size_t bytes,
                const std::string &transaction_id, const fs::path &staging,
                const std::shared_ptr<StagingState> &state,
                std::chrono::steady_clock::time_point started) {
  bool skip_copy = false;
  {
    std::lock_guard lock(state->mutex);
    skip_copy = state->cancelled || !state->error.empty();
  }
  bool copied = false;
  if (!skip_copy) {
    auto partial = entry.destination.string() + ".partial";
    int source = open(entry.source.c_str(), O_RDONLY);
    int destination = source < 0 ? -1 : open(partial.c_str(), O_WRONLY);
    int error = source < 0 || destination < 0 ? errno : 0;
    std::array<char, copy_buffer_bytes> buffer;
    std::size_t done = 0;
    while (!error && done < bytes) {
      auto wanted = std::min(buffer.size(), bytes - done);
      auto read_bytes = pread(source, buffer.data(), wanted,
                              static_cast<off_t>(offset + done));
      if (read_bytes <= 0) {
        error = read_bytes < 0 ? errno : EIO;
        break;
      }
      std::size_t written = 0;
      while (written < static_cast<std::size_t>(read_bytes)) {
        auto n = pwrite(destination, buffer.data() + written,
                        static_cast<std::size_t>(read_bytes) - written,
                        static_cast<off_t>(offset + done + written));
        if (n <= 0) {
          error = n < 0 ? errno : EIO;
          break;
        }
        written += static_cast<std::size_t>(n);
      }
      done += written;
    }
    if (source >= 0)
      close(source);
    if (destination >= 0)
      close(destination);
    if (error) {
      std::error_code ignored;
      fs::remove(partial, ignored);
      set_staging_error(state, error,
                        std::system_error(error, std::generic_category(),
                                          "copy checkpoint chunk")
                            .what());
    } else {
      copied = true;
    }
  }
  finish_copy_task(entry, transaction_id, staging, state, started, copied);
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
                    std::uint64_t &length, std::uint64_t &shared_id) {
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
            !nested_varint(p, n, 2, vaddr) ||
            !nested_varint(p, n, 3, length)) return false;
      } else {
        if (!nested_varint(p, n, 1, shared_id) ||
            !nested_varint(p, n, 2, length)) return false;
      }
      p += n;
    } else if (!skip(p, end, tag & 7)) return false;
  }
  return true;
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
    } else if ((field == 2 || field == 3) && wire == 2) {
      if (!get_varint(p, end, n) || n > static_cast<std::uint64_t>(end - p))
        return false;
      std::string v(p, p + n);
      p += n;
      if (field == 2)
        request.transaction_id = v;
      else
        request.checkpoint_path = v;
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
      budget_(budget), copy_pool_(std::make_unique<CopyPool>(copy_worker_count())) {
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
  auto path = tx_path(staging_root_, r.transaction_id);
  try {
    auto submit_started = std::chrono::steady_clock::now();
    fs::remove_all(path);
    auto plan = plan_copy_tree(r.checkpoint_path, path);
    if (staged_bytes_ > budget_ || plan.bytes > budget_ - staged_bytes_) {
      fs::remove_all(path);
      return fail(r.transaction_id, "staging budget exceeded");
    }
    auto workers = copy_pool_->worker_count();
    auto files = plan.files.size();
    auto staging = std::make_shared<StagingState>();
    staging->prioritize_file =
        [pool = copy_pool_.get(), id = r.transaction_id](const std::string &key) {
          return pool->prioritize(id, key);
        };
    for (const auto &entry : plan.files) {
      staging->planned_files.insert(entry.relative);
      auto chunks = static_cast<std::size_t>(
          std::max<std::uint64_t>(1, (entry.bytes + copy_task_bytes - 1) /
                                         copy_task_bytes));
      staging->remaining_chunks.emplace(entry.relative, chunks);
      staging->remaining_tasks += chunks;
      auto partial = entry.destination.string() + ".partial";
      std::ofstream output(partial, std::ios::binary | std::ios::trunc);
      if (!output)
        throw std::system_error(errno ? errno : EIO,
                                std::generic_category(),
                                "create checkpoint partial");
      output.close();
      fs::resize_file(partial, entry.bytes);
    }
    auto [transaction, inserted] = transactions_.emplace(
        std::piecewise_construct, std::forward_as_tuple(r.transaction_id),
        std::forward_as_tuple());
    auto &state = transaction->second;
    state.staged_bytes = plan.bytes;
    state.staging = staging;
    auto tasks = staging->remaining_tasks;
    staged_bytes_ += plan.bytes;
    auto copy_started = std::chrono::steady_clock::now();
    if (plan.files.empty()) {
      staging->complete = true;
    } else {
      for (const auto &entry : plan.files) {
        auto chunks = staging->remaining_chunks.at(entry.relative);
        for (std::size_t chunk = 0; chunk < chunks; ++chunk) {
          auto offset = static_cast<std::uint64_t>(chunk) * copy_task_bytes;
          auto bytes = static_cast<std::size_t>(std::min<std::uint64_t>(
              copy_task_bytes, entry.bytes > offset ? entry.bytes - offset : 0));
          copy_pool_->submit(
              r.transaction_id, entry.relative,
              [entry, offset, bytes, id = r.transaction_id, path, staging,
               copy_started] {
                copy_chunk(entry, offset, bytes, id, path, staging,
                           copy_started);
              });
        }
      }
    }
    std::osyncstream(std::cerr)
        << "pagebroker stage scheduled transaction="
        << std::quoted(r.transaction_id) << " source="
        << std::quoted(r.checkpoint_path) << " staging="
        << std::quoted(path.string()) << " bytes=" << state.staged_bytes
        << " files=" << files << " tasks=" << tasks
        << " chunk_bytes=" << copy_task_bytes << " workers=" << workers
        << " submit_duration_ms="
        << std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now() - submit_started)
               .count()
        << std::endl;
    return {true, r.transaction_id, path, scratch_root_ / r.transaction_id,
            {}};
  } catch (const std::exception &e) {
    std::cerr << "pagebroker stage failed transaction="
              << std::quoted(r.transaction_id) << " error="
              << std::quoted(e.what()) << std::endl;
    std::error_code cleanup_error;
    fs::remove_all(path, cleanup_error);
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
          tx_path(staging_root_, r.transaction_id),
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
        auto transaction_id = request.transaction_id;
        std::thread([provider_root, client, transaction_id, staging_state] {
          std::osyncstream(std::cerr)
              << "pagebroker provider session start transaction="
              << std::quoted(transaction_id) << " root="
              << std::quoted(provider_root.string()) << std::endl;
          auto status = serve_provider(provider_root, client, -1,
                                       staging_state);
          std::osyncstream(std::cerr)
              << "pagebroker provider session stop transaction="
              << std::quoted(transaction_id) << " status=" << status
              << std::endl;
          close(client);
          {
            std::lock_guard lock(staging_state->mutex);
            staging_state->provider_running = false;
          }
          staging_state->changed.notify_all();
        }).detach();
        continue;
      }
    }
    close(client);
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
  std::array<ProviderTiming, 7> timings{};
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

  std::map<std::uint64_t, int> shared_fds;
  std::uint64_t request_counts[7] = {};
  char buffer[1 << 20];
  for (;;) {
    auto n = recv(socket_fd, buffer, sizeof(buffer), 0);
    if (n == 0) {
      print_timings();
      for (const auto &[_, fd] : shared_fds) close(fd);
      return 0;
    }
    if (n < 0) {
      print_timings();
      return provider_failure(diagnostic_fd, "recv", errno);
    }
    std::uint64_t op = 0, flags = 0, pid = 0, vaddr = 0;
    std::string name;
    std::string response;
    int fd = -1;
    std::uint64_t length = 0, shared_id = 0;
    auto request_started = std::chrono::steady_clock::now();
    auto decode_started = std::chrono::steady_clock::now();
    auto decoded = request_fields(buffer, n, op, name, flags, pid, vaddr,
                                  length, shared_id);
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
    } else if (op < 7) {
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
        request_log << "GET_VMA pid=" << pid << " vaddr=0x" << std::hex
                    << vaddr << std::dec << " length=" << length;
      else if (op == 4)
        request_log << "GET_SHARED shm_id=" << shared_id
                    << " length=" << length;
      else if (op == 5)
        request_log << "COMMIT";
      else if (op == 6)
        request_log << "ABORT";
      std::osyncstream(std::cerr) << request_log.str() << std::endl;
      timing->log_ns += elapsed_ns(log_started);
      if (op == 1 || op == 5 || op == 6) {
        response_status(response, 0);
      } else if (op == 3) {
        auto phase_started = std::chrono::steady_clock::now();
        fd = syscall(SYS_memfd_create, "pagebroker-extmem", MFD_ALLOW_SEALING);
        timing->memfd_ns += elapsed_ns(phase_started);
        int truncate_result = -1;
        if (fd >= 0) {
          phase_started = std::chrono::steady_clock::now();
          truncate_result = ftruncate(fd, static_cast<off_t>(length));
          timing->truncate_ns += elapsed_ns(phase_started);
        }
        if (fd < 0 || truncate_result < 0) {
          response_status(response, -errno);
          if (fd >= 0) close(fd);
          fd = -1;
        } else {
          response_status(response, 0);
          phase_started = std::chrono::steady_clock::now();
          fcntl(fd, F_ADD_SEALS, F_SEAL_GROW | F_SEAL_SHRINK);
          timing->seal_ns += elapsed_ns(phase_started);
        }
      } else if (op == 4) {
        auto existing = shared_fds.find(shared_id);
        if (existing == shared_fds.end()) {
          int shared_fd = syscall(SYS_memfd_create, "pagebroker-extmem-shared",
                                  MFD_ALLOW_SEALING);
          if (shared_fd < 0 ||
              ftruncate(shared_fd, static_cast<off_t>(length)) < 0) {
            response_status(response, -errno);
            if (shared_fd >= 0) close(shared_fd);
          } else {
            existing = shared_fds.emplace(shared_id, shared_fd).first;
          }
        } else {
          struct stat shared_stat {};
          if (fstat(existing->second, &shared_stat) < 0 ||
              (shared_stat.st_size < static_cast<off_t>(length) &&
               ftruncate(existing->second, static_cast<off_t>(length)) < 0)) {
            response_status(response, -errno);
            existing = shared_fds.end();
          }
        }
        if (existing != shared_fds.end()) {
          fd = dup(existing->second);
          response_status(response, fd < 0 ? -errno : 0);
        }
      } else if (op == 2) {
        fs::path relative(name);
        if (name.empty() || relative.is_absolute() ||
            std::find(relative.begin(), relative.end(), fs::path("..")) != relative.end()) {
          response_status(response, -EINVAL);
        } else {
          int ready = 0;
          if (staging) {
            auto wait_started = std::chrono::steady_clock::now();
            auto key = relative.generic_string();
            std::unique_lock lock(staging->mutex);
            if (!staging->planned_files.contains(key)) {
              ready = -ENOENT;
            } else {
              auto prioritize = staging->prioritize_file;
              lock.unlock();
              auto prioritized = prioritize && prioritize(key);
              lock.lock();
              if (prioritized)
                std::osyncstream(std::cerr)
                    << "pagebroker stage prioritized path="
                    << std::quoted(name) << std::endl;
              staging->changed.wait(lock, [&] {
                return staging->ready_files.contains(key) ||
                       staging->complete || staging->cancelled ||
                       !staging->error.empty();
              });
            }
            if (ready == 0 && !staging->ready_files.contains(key)) {
              if (!staging->error.empty())
                ready = -(staging->error_code ? staging->error_code : EIO);
              else if (staging->cancelled)
                ready = -ECANCELED;
              else
                ready = -ENOENT;
            }
            auto waited = std::chrono::duration_cast<std::chrono::milliseconds>(
                std::chrono::steady_clock::now() - wait_started);
            timing->readiness_ns += elapsed_ns(wait_started);
            if (waited.count() > 0)
              std::osyncstream(std::cerr)
                  << "pagebroker provider file wait path="
                  << std::quoted(name) << " duration_ms=" << waited.count()
                  << " status=" << ready << std::endl;
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
                << " shared=" << request_counts[4] << std::endl;
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
    if (op == 5 || op == 6)
      print_timings();
  }
}

} // namespace pagebroker

#ifndef PAGEBROKER_TEST
int main(int argc, char **argv) {
  if (argc == 5 && std::string(argv[1]) == "provider")
    return pagebroker::serve_provider(argv[2], std::stoi(argv[3]), std::stoi(argv[4]));
  if (argc != 2 || std::string(argv[1]) != "serve")
    return 2;
  return pagebroker::serve("/run/pagebroker/pagebroker.sock", "/staging",
                           "/scratch", 1ULL << 40);
}
#endif
