// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "native_session.hpp"
#include "fd_transport.hpp"
#include "gpu_engine.hpp"
#include "gpu_engine.pb.h"

#include <fcntl.h>
#include <sys/ioctl.h>
#include <linux/nsfs.h>
#include <sys/stat.h>
#include <unistd.h>

#include <fstream>
#include <cstdio>
#include <sstream>
#include <stdexcept>

namespace snapshot::pagebroker {
namespace {
void Require(bool ok, const char* message) {
  if (!ok) throw std::runtime_error(message);
}
}

NativeSession::NativeSession(std::shared_ptr<Transaction> transaction,
                             const v1::BindNativeSession& binding, std::shared_ptr<GpuEngine> engine)
    : transaction_(std::move(transaction)), binding_(binding), engine_(std::move(engine)) {
  Require(binding.container_pid() > 1 && binding.namespace_pid() > 0 &&
          binding.visible_devices_size() > 0, "native binding requires target identity and GPUs");
  Require(binding.direction() == v1::BindNativeSession::SAVE ||
          binding.direction() == v1::BindNativeSession::LOAD, "invalid native direction");
  Require(engine_ != nullptr, "GPU engine must be configured");
  namespace_fd_ = FileDescriptor(open(("/proc/" + std::to_string(binding.container_pid()) +
                                      "/ns/pid").c_str(), O_RDONLY | O_CLOEXEC));
  Require(namespace_fd_.get() >= 0, "pin target PID namespace");
  std::lock_guard lock(transaction_->mutex());
  Require(transaction_->state() == Transaction::State::STAGED && !transaction_->native_failed,
          "native transaction is not staged");
  const auto key = binding.namespace_pid();
  Require(!transaction_->native_targets.contains(key), "native target already admitted");
  FileDescriptor root(-1);
  if (binding.direction() == v1::BindNativeSession::SAVE) {
    const auto* descriptor = std::get_if<CheckpointTransactionDescriptor>(&transaction_->descriptor());
    Require(descriptor != nullptr, "native SAVE requires checkpoint transaction");
    root = FileDescriptor(open(descriptor->staging_directory().c_str(), O_DIRECTORY | O_CLOEXEC));
  } else {
    const auto* descriptor = std::get_if<DirectRestoreDescriptor>(&transaction_->descriptor());
    Require(descriptor != nullptr, "native LOAD requires GPU restore preparation");
    root = FileDescriptor(fcntl(descriptor->source_directory.get(), F_DUPFD_CLOEXEC, 0));
  }
  Require(root.get() >= 0, "open native transaction root");
  transaction_->native_failed = true;
  if (binding.direction() == v1::BindNativeSession::SAVE) {
    Require(mkdirat(root.get(), "native", 0700) == 0 || errno == EEXIST, "create native root");
  }
  FileDescriptor native(openat(root.get(), "native", O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Require(native.get() >= 0, "open native root");
  const auto name = std::to_string(binding.namespace_pid());
  if (binding.direction() == v1::BindNativeSession::SAVE)
    Require(mkdirat(native.get(), name.c_str(), 0700) == 0, "create native target directory");
  directory_fd_ = FileDescriptor(openat(native.get(), name.c_str(), O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Require(directory_fd_.get() >= 0, "pin native target directory");
  transaction_->native_targets.insert(key);
  ++transaction_->native_sessions;
  transaction_->native_failed = false;
}

void NativeSession::Start(uint32_t target_pid) {
  // The agent supplies the PID observed from the pinned placeholder namespace,
  // not the restored process's innermost PID (both roots can have inner PID 1).
  // The GPU engine opens a pidfd before any operation on the target.
  struct stat expected{};
  Require(fstat(namespace_fd_.get(), &expected) == 0, "stat target namespace");
  int target = 0;
  for (const auto& entry : std::filesystem::directory_iterator("/proc")) {
    const auto name = entry.path().filename().string();
    if (name.find_first_not_of("0123456789") != std::string::npos) continue;
    // CRIU may create a nested PID namespace beneath the placeholder. Accept
    // only that pinned namespace or one of its descendants, never a sibling.
    FileDescriptor candidate(open((entry.path() / "ns/pid").c_str(), O_RDONLY | O_CLOEXEC));
    bool contained = false;
    size_t depth = 0;
    while (candidate.get() >= 0) {
      struct stat actual{};
      if (fstat(candidate.get(), &actual)) break;
      if (actual.st_dev == expected.st_dev && actual.st_ino == expected.st_ino) {
        contained = true;
        break;
      }
      candidate = FileDescriptor(ioctl(candidate.get(), NS_GET_PARENT));
      ++depth;
    }
    if (!contained) continue;
    std::ifstream status(entry.path() / "status");
    std::string line;
    while (std::getline(status, line)) {
      if (!line.starts_with("NSpid:")) continue;
      std::istringstream values(line.substr(6));
      std::vector<unsigned> pids;
      unsigned value;
      while (values >> value) pids.push_back(value);
      if (pids.size() > depth && pids[pids.size() - depth - 1] == target_pid) {
        Require(target == 0, "ambiguous native target");
        target = std::stoi(name);
      }
    }
  }
  Require(target > 0, "native target absent from pinned namespace");
  connection_ = engine_->Bind(binding_, target, directory_fd_.get());
  v1::NativeSessionReply reply;
  std::vector<FileDescriptor> descriptors;
  Require(ReceiveFrame(connection_.get(), reply, descriptors), "GPU engine disconnected during bind");
  Require(!reply.has_failure(), reply.failure().message().c_str());
  std::fprintf(stderr, "Native session target=%d %s\n", target, reply.report().c_str());
}

v1::NativeSessionReply NativeSession::Execute(const v1::NativeSessionRequest& request) {
  v1::NativeSessionReply reply;
  try {
    using Op = v1::NativeSessionRequest;
    if (connection_.get() < 0)
      Start(request.target_pid() ? request.target_pid() : binding_.namespace_pid());
    internal::NativeCommand command;
    *command.mutable_execute() = request;
    SendFrame(connection_.get(), command);
    std::vector<FileDescriptor> descriptors;
    Require(ReceiveFrame(connection_.get(), reply, descriptors), "GPU engine disconnected during operation");
    if (reply.has_failure()) return reply;
    finished_ = request.operation() == Op::COMPLETE;
  } catch (const std::exception& error) {
    reply.mutable_failure()->set_code(v1::Failure::INTERNAL_ERROR);
    reply.mutable_failure()->set_message(error.what());
  }
  return reply;
}

void NativeSession::Stop() noexcept {
  if (connection_.get() < 0) return;
  try {
    internal::NativeCommand command;
    command.set_drain(true);
    SendFrame(connection_.get(), command);
    v1::NativeSessionReply drained;
    std::vector<FileDescriptor> descriptors;
    Require(ReceiveFrame(connection_.get(), drained, descriptors) && !drained.has_failure() &&
            drained.report() == "drained", "GPU engine did not acknowledge drain");
  } catch (const std::exception& error) {
    std::fprintf(stderr, "GPU engine drain failed: %s\n", error.what());
    engine_->Stop();
  }
  connection_ = FileDescriptor(-1);
}

NativeSession::~NativeSession() {
  Stop();
  std::lock_guard lock(transaction_->mutex());
  --transaction_->native_sessions;
  transaction_->native_failed |= !finished_;
  transaction_->native_drained.notify_all();
}
}  // namespace snapshot::pagebroker
