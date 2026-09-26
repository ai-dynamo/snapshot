// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "gpu_engine.hpp"
#include "gpu_engine.pb.h"
#include "fd_transport.hpp"
#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>
#include <cstdio>
#include <stdexcept>

extern char** environ;

namespace snapshot::pagebroker {
GpuEngine::GpuEngine(std::filesystem::path executable) : executable_(std::move(executable)) {}
GpuEngine::~GpuEngine() { Stop(); }

void GpuEngine::Start() {
  std::lock_guard lock(mutex_);
  int sockets[2];
  if (socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets))
    throw std::runtime_error("GPU engine socketpair");
  control_ = FileDescriptor(sockets[0]);
  FileDescriptor child(sockets[1]);
  posix_spawn_file_actions_t actions;
  int error = posix_spawn_file_actions_init(&actions);
  if (error) throw std::runtime_error("GPU engine spawn actions");
  error = posix_spawn_file_actions_adddup2(&actions, child.get(), 3);
  if (!error) error = posix_spawn_file_actions_addclosefrom_np(&actions, 4);
  auto binary = executable_.string();
  char* argv[] = {binary.data(), nullptr};
  if (!error) error = posix_spawn(&process_, binary.c_str(), &actions, nullptr, argv, environ);
  posix_spawn_file_actions_destroy(&actions);
  if (error) throw std::runtime_error("spawn GPU engine");
  SetSessionTimeout(control_.get());
  v1::NativeSessionReply ready;
  std::vector<FileDescriptor> descriptors;
  if (!ReceiveFrame(control_.get(), ready, descriptors) || ready.has_failure())
    throw std::runtime_error("GPU engine initialization failed: " + ready.failure().message());
  std::fprintf(stderr, "GPU engine pid=%d %s\n", process_, ready.report().c_str());
}

FileDescriptor GpuEngine::Bind(const v1::BindNativeSession& binding, int target, int directory,
                               const internal::LoadManifest* load) {
  std::lock_guard lock(mutex_);
  if (process_ <= 0) throw std::runtime_error("GPU engine is not running");
  int sockets[2];
  if (socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets))
    throw std::runtime_error("GPU engine session socketpair");
  FileDescriptor connection(sockets[0]), peer(sockets[1]);
  SetSessionTimeout(connection.get());
  internal::NativeBinding request;
  *request.mutable_binding() = binding;
  request.set_host_pid(target);
  if (load) *request.mutable_load_manifest() = *load;
  SendFrame(control_.get(), request, {peer.get(), directory});
  return connection;
}

void GpuEngine::Stop() noexcept {
  std::lock_guard lock(mutex_);
  if (process_ <= 0) return;
  kill(process_, SIGKILL);
  while (waitpid(process_, nullptr, 0) < 0 && errno == EINTR) {}
  process_ = -1;
  control_ = FileDescriptor(-1);
}
}  // namespace snapshot::pagebroker
