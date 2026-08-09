// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "pagebroker.hpp"

#include <cassert>
#include <cerrno>
#include <cstring>
#include <filesystem>
#include <fcntl.h>
#include <fstream>
#include <future>
#include <poll.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <unistd.h>

namespace fs = std::filesystem;

namespace {
void append_varint(std::string &message, std::uint64_t value) {
  while (value >= 128) {
    message.push_back(static_cast<char>((value & 127) | 128));
    value >>= 7;
  }
  message.push_back(static_cast<char>(value));
}

void append_field(std::string &message, int field, std::uint64_t value) {
  append_varint(message, static_cast<std::uint64_t>(field * 8));
  append_varint(message, value);
}

void append_field(std::string &message, int field, const std::string &value) {
  append_varint(message, static_cast<std::uint64_t>(field * 8 + 2));
  append_varint(message, value.size());
  message += value;
}

std::int32_t receive_response(int connection, int *received_fd = nullptr) {
  char response[128] = {};
  char control[CMSG_SPACE(sizeof(int))] = {};
  iovec iov{response, sizeof(response)};
  msghdr msg{};
  msg.msg_iov = &iov;
  msg.msg_iovlen = 1;
  msg.msg_control = control;
  msg.msg_controllen = sizeof(control);
  auto n = recvmsg(connection, &msg, 0);
  assert(n > 0);
  if (received_fd) {
    auto *cmsg = CMSG_FIRSTHDR(&msg);
    assert(cmsg && cmsg->cmsg_type == SCM_RIGHTS);
    std::memcpy(received_fd, CMSG_DATA(cmsg), sizeof(int));
  }
  std::uint64_t value = 0;
  for (int i = 1, shift = 0; i < n && shift < 64; ++i, shift += 7) {
    auto byte = static_cast<unsigned char>(response[i]);
    value |= static_cast<std::uint64_t>(byte & 127) << shift;
    if (!(byte & 128))
      return static_cast<std::int32_t>(value);
  }
  std::abort();
}

std::string get_vma_request(std::uint64_t pid, std::uint64_t vma_id,
                            std::uint64_t address, std::uint64_t length) {
  std::string fields;
  append_field(fields, 1, pid);
  if (vma_id)
    append_field(fields, 2, vma_id);
  append_field(fields, 3, address);
  append_field(fields, 4, length);
  std::string request;
  append_field(request, 1, 3);
  append_field(request, 3, fields);
  return request;
}

std::string get_shared_request(std::uint64_t shmid, std::uint64_t length) {
  std::string fields;
  append_field(fields, 1, shmid);
  append_field(fields, 2, length);
  std::string request;
  append_field(request, 1, 4);
  append_field(request, 4, fields);
  return request;
}

std::string operation_request(std::uint64_t operation) {
  std::string request;
  append_field(request, 1, operation);
  return request;
}

pagebroker::Request restore_request(const fs::path &root,
                                    const std::string &transaction) {
  pagebroker::Request request{pagebroker::Request::Operation::Submit,
                              transaction, root};
  request.manifest_ref = (root / "pagebroker-manifest.yaml").string();
  request.resident_bytes = 20;
  request.images.push_back({"pages-1.img", "pages-1.img", 10});
  pagebroker::HostMemoryObject object;
  object.memory_id = 1;
  object.name = "private-1-2";
  object.pid = 1;
  object.vma_id = 0;
  object.dst_addr = 4096;
  object.length = 32;
  object.semantics = "private_anon";
  object.map_mode = "private";
  object.source_ranges.push_back({"pages-1.img", 0, 8, 10});
  request.host_memory_objects.push_back(std::move(object));

  pagebroker::HostMemoryObject shared;
  shared.memory_id = 2;
  shared.name = "shared-memfd-55";
  shared.shmid = 55;
  shared.length = 16;
  shared.semantics = "shared_memfd";
  shared.map_mode = "shared";
  shared.source_ranges.push_back({"pages-1.img", 0, 0, 10});
  request.host_memory_objects.push_back(std::move(shared));

  std::string wire;
  append_field(wire, 1, 1);
  append_field(wire, 5, request.resident_bytes);
  for (const auto &image : request.images) {
    std::string encoded;
    append_field(encoded, 1, image.name);
    append_field(encoded, 2, image.uri);
    append_field(encoded, 3, image.size);
    append_field(wire, 6, encoded);
  }
  for (const auto &item : request.host_memory_objects) {
    std::string encoded;
    append_field(encoded, 1, item.memory_id);
    append_field(encoded, 2, item.name);
    append_field(encoded, 3, item.pid);
    append_field(encoded, 4, item.vma_id);
    append_field(encoded, 5, item.shmid);
    append_field(encoded, 6, item.dst_addr);
    append_field(encoded, 7, item.length);
    append_field(encoded, 8, item.semantics);
    append_field(encoded, 9, item.map_mode);
    for (const auto &source : item.source_ranges) {
      std::string range;
      append_field(range, 1, source.object);
      append_field(range, 2, source.source_offset);
      append_field(range, 3, source.dst_offset);
      append_field(range, 4, source.length);
      append_field(encoded, 10, range);
    }
    append_field(wire, 7, encoded);
  }
  std::ofstream output(root / "pagebroker-manifest.pb", std::ios::binary);
  output.write(wire.data(), static_cast<std::streamsize>(wire.size()));
  output.close();
  return request;
}
} // namespace

int main() {
  assert(pagebroker::test_copy_pool_priority());
  auto root = fs::temp_directory_path() /
              ("pagebroker-test-" + std::to_string(getpid()));
  fs::remove_all(root);
  auto checkpoint = root / "checkpoint";
  fs::create_directories(checkpoint);
  std::ofstream(checkpoint / "pagebroker-manifest.yaml") << "version: 1\n";
  std::ofstream(checkpoint / "pages-1.img") << "range-data";

  pagebroker::test_set_fill_delay(150);
  pagebroker::TransactionManager manager(root / "staging", root / "scratch",
                                         64);
  auto request = restore_request(checkpoint, "tx-1");
  auto submitted = manager.submit(request);
  assert(submitted.ok);
  assert(fs::equivalent(submitted.staging_path, checkpoint));
  assert(!fs::exists(root / "staging/tx/tx-1/pages-1.img"));

  auto wait = std::async(std::launch::async, [&] { return manager.wait_ready(request); });
  assert(wait.wait_for(std::chrono::milliseconds(25)) == std::future_status::timeout);

  auto state = manager.staging_state("tx-1");
  assert(state);
  int sockets[2];
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET, 0, sockets) == 0);
  std::thread provider([&] {
    assert(pagebroker::serve_provider(checkpoint, sockets[1], -1, state) == 0);
    close(sockets[1]);
  });
  auto get = get_vma_request(1, 0, 4096, 32);
  assert(send(sockets[0], get.data(), get.size(), 0) ==
         static_cast<ssize_t>(get.size()));
  int vma_fd = -1;
  assert(receive_response(sockets[0], &vma_fd) == 0);
  struct stat returned{}, owned{};
  assert(fstat(vma_fd, &returned) == 0);
  assert(fstat(state->vmas.begin()->second->fd, &owned) == 0);
  assert(returned.st_ino == owned.st_ino);
  assert(returned.st_size == 32);

  auto get_shared = get_shared_request(55, 16);
  assert(send(sockets[0], get_shared.data(), get_shared.size(), 0) ==
         static_cast<ssize_t>(get_shared.size()));
  int shared_fd = -1;
  assert(receive_response(sockets[0], &shared_fd) == 0);
  struct stat returned_shared{}, owned_shared{};
  assert(fstat(shared_fd, &returned_shared) == 0);
  assert(fstat(state->shared.begin()->second->fd, &owned_shared) == 0);
  assert(returned_shared.st_ino == owned_shared.st_ino);
  assert(returned_shared.st_size == 16);
  assert(fcntl(shared_fd, F_GET_SEALS) == 0);

  auto provider_wait = std::async(std::launch::async, [&] {
    auto request = operation_request(7);
    assert(send(sockets[0], request.data(), request.size(), 0) ==
           static_cast<ssize_t>(request.size()));
    return receive_response(sockets[0]);
  });
  assert(provider_wait.wait_for(std::chrono::milliseconds(25)) ==
         std::future_status::timeout);

  auto ready = wait.get();
  assert(ready.ok);
  assert(provider_wait.get() == 0);
  char data[11] = {};
  assert(pread(vma_fd, data, 10, 8) == 10);
  assert(std::string(data, 10) == "range-data");
  char hole[8];
  std::memset(hole, 1, sizeof(hole));
  assert(pread(vma_fd, hole, sizeof(hole), 0) == 8);
  assert(std::string(hole, sizeof(hole)) == std::string(sizeof(hole), '\0'));
  close(vma_fd);
  assert(pread(shared_fd, data, 10, 0) == 10);
  assert(std::string(data, 10) == "range-data");
  close(shared_fd);

  assert(setenv("PAGEBROKER_DEFER_FILL", "1", 1) == 0);
  pagebroker::TransactionManager deferred(root / "staging-deferred",
                                          root / "scratch-deferred", 64);
  assert(unsetenv("PAGEBROKER_DEFER_FILL") == 0);
  auto deferred_request = restore_request(checkpoint, "tx-deferred");
  auto deferred_submit = deferred.submit(deferred_request);
  assert(deferred_submit.ok);
  auto deferred_state = deferred.staging_state("tx-deferred");
  assert(deferred_state);
  {
    std::lock_guard lock(deferred_state->mutex);
    assert(!deferred_state->fill_started);
    assert(!deferred_state->complete);
  }
  auto deferred_ready = deferred.wait_ready(deferred_request);
  assert(deferred_ready.ok);
  {
    std::lock_guard lock(deferred_state->mutex);
    assert(deferred_state->fill_started);
    assert(deferred_state->complete);
  }

  std::string provider_commit;
  append_field(provider_commit, 1, 5);
  assert(send(sockets[0], provider_commit.data(), provider_commit.size(), 0) ==
         static_cast<ssize_t>(provider_commit.size()));
  assert(receive_response(sockets[0]) == 0);
  close(sockets[0]);
  provider.join();
  assert(manager.commit(request).ok);

  pagebroker::TransactionManager disconnected(root / "staging-disconnected",
                                               root / "scratch-disconnected", 64);
  auto disconnected_request = restore_request(checkpoint, "tx-disconnected");
  assert(disconnected.submit(disconnected_request).ok);
  auto disconnected_state = disconnected.staging_state("tx-disconnected");
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET, 0, sockets) == 0);
  close(sockets[0]);
  assert(pagebroker::serve_provider(checkpoint, sockets[1], -1,
                                    disconnected_state) == ECONNRESET);
  close(sockets[1]);
  auto disconnect_failure = disconnected.wait_ready(disconnected_request);
  assert(!disconnect_failure.ok);
  assert(disconnect_failure.error.find("provider disconnected") !=
         std::string::npos);
  assert(disconnected.abort(disconnected_request).ok);

  pagebroker::test_set_fill_delay(150);
  pagebroker::TransactionManager failing(root / "staging-fail",
                                         root / "scratch-fail", 64);
  auto backend_failure = restore_request(checkpoint, "tx-backend-failure");
  assert(failing.submit(backend_failure).ok);
  fs::remove(checkpoint / "pages-1.img");
  auto failed = failing.wait_ready(backend_failure);
  assert(!failed.ok);
  assert(failed.error.find("open manifest source image") != std::string::npos);
  assert(failing.abort(backend_failure).ok);
  std::ofstream(checkpoint / "pages-1.img") << "range-data";
  pagebroker::test_set_fill_delay(0);

  pagebroker::TransactionManager budget(root / "staging-budget",
                                        root / "scratch-budget", 19);
  auto rejected = budget.submit(restore_request(checkpoint, "tx-budget"));
  assert(!rejected.ok);
  assert(rejected.error.find("budget") != std::string::npos);

  auto destination = root / "checkpoints/final";
  pagebroker::TransactionManager checkpoint_manager(root / "staging-checkpoint",
                                                     root / "scratch-checkpoint",
                                                     100);
  pagebroker::Request prepare{pagebroker::Request::Operation::PrepareCheckpoint,
                              "tx-checkpoint", destination};
  auto prepared = checkpoint_manager.prepare_checkpoint(prepare);
  assert(prepared.ok);
  std::ofstream(fs::path(prepared.staging_path) / "manifest") << "ok";
  assert(checkpoint_manager.commit(prepare).ok);
  assert(fs::exists(destination / "manifest"));

  fs::remove_all(root);
}
