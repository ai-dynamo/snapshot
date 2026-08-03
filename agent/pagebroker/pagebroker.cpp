// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
#include "pagebroker.hpp"

#include <cstring>
#include <fstream>
#include <iostream>
#include <linux/un.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <thread>
#include <unistd.h>

namespace fs = std::filesystem;
namespace pagebroker {
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
bool copy_tree(const fs::path &from, const fs::path &to) {
  fs::create_directories(to);
  for (auto &e : fs::recursive_directory_iterator(from)) {
    auto dest = to / fs::relative(e.path(), from);
    if (e.is_directory())
      fs::create_directories(dest);
    else if (e.is_regular_file()) {
      auto partial = dest.string() + ".partial";
      fs::create_directories(dest.parent_path());
      std::ifstream in(e.path(), std::ios::binary);
      std::ofstream out(partial, std::ios::binary);
      out << in.rdbuf();
      out.close();
      in.close();
      if (fs::file_size(partial) != e.file_size())
        return false;
      fs::rename(partial, dest);
    }
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
      budget_(budget) {
  cleanup();
}
void TransactionManager::cleanup() {
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
  try {
    auto bytes = tree_size(r.checkpoint_path);
    if (staged_bytes_ > budget_ || bytes > budget_ - staged_bytes_)
      return fail(r.transaction_id, "staging budget exceeded");
    auto path = tx_path(staging_root_, r.transaction_id);
    fs::remove_all(path);
    if (!copy_tree(r.checkpoint_path, path)) {
      fs::remove_all(path);
      return fail(r.transaction_id, "checkpoint copy exceeded staging budget");
    }
    auto copied_bytes = tree_size(path);
    if (copied_bytes > budget_ - staged_bytes_) {
      fs::remove_all(path);
      return fail(r.transaction_id, "checkpoint copy exceeded staging budget");
    }
    transactions_.emplace(r.transaction_id, TransactionState{copied_bytes});
    staged_bytes_ += copied_bytes;
    return {true, r.transaction_id, path, scratch_root_ / r.transaction_id,
            {}};
  } catch (const fs::filesystem_error &e) {
    std::error_code cleanup_error;
    fs::remove_all(tx_path(staging_root_, r.transaction_id), cleanup_error);
    return fail(r.transaction_id, e.what());
  }
}
Response TransactionManager::wait_ready(const Request &r) {
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(r.transaction_id);
  if (transaction == transactions_.end())
    return fail(r.transaction_id, "transaction is not active");
  return {true,
          r.transaction_id,
          tx_path(staging_root_, r.transaction_id),
          scratch_root_ / r.transaction_id,
          {}};
}
Response TransactionManager::commit(const Request &r) {
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(r.transaction_id);
  if (transaction == transactions_.end())
    return fail(r.transaction_id, "transaction is not active");
  try {
    fs::remove_all(tx_path(staging_root_, r.transaction_id));
    fs::remove_all(scratch_root_ / r.transaction_id);
  } catch (const fs::filesystem_error &e) {
    return fail(r.transaction_id, e.what());
  }
  staged_bytes_ -= transaction->second.staged_bytes;
  transactions_.erase(transaction);
  return {true, r.transaction_id, {}, {}, {}};
}
Response TransactionManager::abort(const Request &r) {
  std::lock_guard lock(mutex_);
  auto transaction = transactions_.find(r.transaction_id);
  if (transaction != transactions_.end()) {
    try {
      fs::remove_all(tx_path(staging_root_, r.transaction_id));
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
    a.sin_port = htons(8080);
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
    if (n < 0 || !decode_request(buffer, n, request, error))
      response = fail({}, error.empty() ? "read failed" : error);
    else if (request.operation == Request::Operation::Submit)
      response = manager.submit(request);
    else if (request.operation == Request::Operation::WaitReady)
      response = manager.wait_ready(request);
    else if (request.operation == Request::Operation::Commit)
      response = manager.commit(request);
    else if (request.operation == Request::Operation::Abort)
      response = manager.abort(request);
    else
      response = fail(request.transaction_id, "unknown operation");
    auto encoded = encode_response(response);
    send(client, encoded.data(), encoded.size(), MSG_NOSIGNAL);
    close(client);
  }
}

} // namespace pagebroker

#ifndef PAGEBROKER_TEST
int main(int argc, char **argv) {
  if (argc != 2 || std::string(argv[1]) != "serve")
    return 2;
  return pagebroker::serve("/run/pagebroker/pagebroker.sock", "/staging",
                           "/scratch", 1ULL << 40);
}
#endif
