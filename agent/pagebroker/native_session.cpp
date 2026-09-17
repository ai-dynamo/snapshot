// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "native_session.hpp"
#include "allocation_transport.hpp"

#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/ioctl.h>
#include <linux/nsfs.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <fstream>
#include <sstream>
#include <stdexcept>
#include <thread>

extern char** environ;

namespace snapshot::pagebroker {
namespace {
void Require(bool ok, const char* message) {
  if (!ok) throw std::runtime_error(message);
}
std::string ReadLine(int fd) {
  std::string line;
  char byte;
  while (line.size() < 65536) {
    const auto size = read(fd, &byte, 1);
    if (size < 0 && errno == EINTR) continue;
    Require(size == 1, "native worker disconnected or timed out");
    if (byte == '\n') return line;
    line += byte;
  }
  throw std::runtime_error("native worker report exceeds limit");
}
}

NativeSession::NativeSession(std::shared_ptr<Transaction> transaction,
                             const v1::BindNativeSession& binding, const Path& executable)
    : transaction_(std::move(transaction)), binding_(binding), executable_(executable) {
  Require(binding.container_pid() > 1 && binding.namespace_pid() > 0 &&
          binding.visible_devices_size() > 0, "native binding requires target identity and GPUs");
  Require(binding.direction() == v1::BindAllocationSession::SAVE ||
          binding.direction() == v1::BindAllocationSession::LOAD, "invalid native direction");
  Require(executable.is_absolute(), "native worker must be configured");
  for (const auto& uuid : binding.visible_devices())
    Require(uuid.size() == 40 && uuid.starts_with("GPU-") &&
            uuid.find_first_not_of("GPU-0123456789abcdefABCDEF") == std::string::npos,
            "invalid native GPU UUID");
  namespace_fd_ = FileDescriptor(open(("/proc/" + std::to_string(binding.container_pid()) +
                                      "/ns/pid").c_str(), O_RDONLY | O_CLOEXEC));
  Require(namespace_fd_.get() >= 0, "pin target PID namespace");
  std::lock_guard lock(transaction_->mutex());
  Require(transaction_->state() == Transaction::State::STAGED && !transaction_->allocation_failed,
          "native transaction is not staged");
  const auto key = "native-" + std::to_string(binding.namespace_pid());
  Require(!transaction_->allocation_participants.contains(key), "native target already admitted");
  FileDescriptor root(-1);
  if (binding.direction() == v1::BindAllocationSession::SAVE) {
    const auto* descriptor = std::get_if<CheckpointTransactionDescriptor>(&transaction_->descriptor());
    Require(descriptor != nullptr, "native SAVE requires checkpoint transaction");
    root = FileDescriptor(open(descriptor->staging_directory().c_str(), O_DIRECTORY | O_CLOEXEC));
  } else {
    const auto* descriptor = std::get_if<DirectRestoreDescriptor>(&transaction_->descriptor());
    Require(descriptor != nullptr, "native LOAD requires direct restore transaction");
    root = FileDescriptor(fcntl(descriptor->source_directory.get(), F_DUPFD_CLOEXEC, 0));
  }
  Require(root.get() >= 0, "open native transaction root");
  transaction_->allocation_failed = true;
  if (binding.direction() == v1::BindAllocationSession::SAVE) {
    Require(mkdirat(root.get(), "native", 0700) == 0 || errno == EEXIST, "create native root");
  }
  FileDescriptor native(openat(root.get(), "native", O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Require(native.get() >= 0, "open native root");
  const auto name = std::to_string(binding.namespace_pid());
  if (binding.direction() == v1::BindAllocationSession::SAVE)
    Require(mkdirat(native.get(), name.c_str(), 0700) == 0, "create native target directory");
  directory_fd_ = FileDescriptor(openat(native.get(), name.c_str(), O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  Require(directory_fd_.get() >= 0, "pin native target directory");
  transaction_->allocation_participants.insert(key);
  ++transaction_->allocation_sessions;
  transaction_->allocation_failed = false;
  admitted_ = true;
}

void NativeSession::Start(uint32_t target_pid) {
  // The agent supplies the PID observed from the pinned placeholder namespace,
  // not the restored process's innermost PID (both roots can have inner PID 1).
  // The worker opens a pidfd before any CUDA operation.
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
  int sockets[2];
  Require(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets) == 0, "native socketpair");
  connection_ = FileDescriptor(sockets[0]);
  FileDescriptor child(sockets[1]);
  SetAllocationTimeout(connection_.get());
  std::vector<std::string> environment;
  for (char** item = environ; *item; ++item) {
    const std::string value(*item);
    if (!value.starts_with("CUDA_VISIBLE_DEVICES=") && !value.starts_with("CUDA_CHECKPOINT_JOB_FILE=") &&
        !value.starts_with("PAGEBROKER_NATIVE_DEVICE_MAP=") && !value.starts_with("PAGEBROKER_NATIVE_SELECTED_GPU="))
      environment.push_back(value);
  }
  std::string visible;
  for (const auto& uuid : binding_.visible_devices()) {
    if (!visible.empty()) visible += ',';
    visible += uuid;
  }
  environment.push_back("CUDA_VISIBLE_DEVICES=" + visible);
  environment.push_back("PAGEBROKER_NATIVE_DEVICE_MAP=" + binding_.device_map());
  std::vector<char*> env;
  for (auto& value : environment) env.push_back(value.data());
  env.push_back(nullptr);
  std::string binary = executable_.string(), pid = std::to_string(target), directory = "/proc/self/fd/3";
  char* argv[] = {binary.data(), pid.data(), directory.data(), nullptr};
  posix_spawn_file_actions_t actions;
  Require(posix_spawn_file_actions_init(&actions) == 0, "native spawn actions");
  int error = posix_spawn_file_actions_adddup2(&actions, child.get(), STDIN_FILENO);
  if (!error) error = posix_spawn_file_actions_adddup2(&actions, child.get(), STDOUT_FILENO);
  if (!error) error = posix_spawn_file_actions_adddup2(&actions, directory_fd_.get(), 3);
  if (!error) error = posix_spawn_file_actions_addclosefrom_np(&actions, 4);
  if (!error) error = posix_spawn(&worker_, binary.c_str(), &actions, nullptr, argv, env.data());
  posix_spawn_file_actions_destroy(&actions);
  Require(error == 0, "spawn native worker");
  Require(ReadLine(connection_.get()).find("\"event\":\"ready\"") != std::string::npos,
          "native worker did not become ready");
}

v1::NativeSessionReply NativeSession::Execute(const v1::NativeSessionRequest& request) {
  v1::NativeSessionReply reply;
  try {
    using Op = v1::NativeSessionRequest;
    const bool save = binding_.direction() == v1::BindAllocationSession::SAVE;
    const auto next = phase_ == Op::UNSPECIFIED ? (save ? Op::LOCK : Op::PREPARE) :
                      phase_ == Op::LOCK ? Op::PREPARE :
                      phase_ == Op::PREPARE ? Op::TRANSFER : Op::COMPLETE;
    Require(!finished_ && request.operation() == next, "invalid native phase order");
    if (worker_ < 0) Start(request.target_pid() ? request.target_pid() : binding_.namespace_pid());
    const std::string command = next == Op::LOCK ? "lock\n" : next == Op::PREPARE ?
        (save ? "prepare-save\n" : "prepare-load\n") : next == Op::TRANSFER ? "transfer\n" : "complete\n";
    Require(send(connection_.get(), command.data(), command.size(), MSG_NOSIGNAL) ==
                static_cast<ssize_t>(command.size()), "write native command");
    reply.set_report(ReadLine(connection_.get()));
    phase_ = next;
    finished_ = next == Op::COMPLETE;
  } catch (const std::exception& error) {
    reply.mutable_failure()->set_code(v1::Failure::INTERNAL_ERROR);
    reply.mutable_failure()->set_message(error.what());
  }
  return reply;
}

void NativeSession::Stop() noexcept {
  if (worker_ <= 0) return;
  if (!finished_) {
    kill(worker_, SIGKILL);
    while (waitpid(worker_, nullptr, 0) < 0 && errno == EINTR) {}
    worker_ = -1;
    return;
  }
  // Successful workers retain CUDA context ownership until the target exits.
  // Their completed storage operations no longer pin the transaction. Reaping
  // is independent of admission and must not keep a daemon handler occupied.
  connection_ = FileDescriptor(-1);
  const auto pid = worker_;
  std::thread([pid] { while (waitpid(pid, nullptr, 0) < 0 && errno == EINTR) {} }).detach();
  worker_ = -1;
}
NativeSession::~NativeSession() {
  Stop();
  if (admitted_) {
    std::lock_guard lock(transaction_->mutex());
    --transaction_->allocation_sessions;
    transaction_->allocation_failed |= !finished_;
    transaction_->allocation_drained.notify_all();
  }
}
}  // namespace snapshot::pagebroker
