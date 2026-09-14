// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved. SPDX-License-Identifier: Apache-2.0

#include "cuinterpose_coordinator.hpp"

#include <fcntl.h>
#include <signal.h>
#include <spawn.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <limits>
#include <stdexcept>
#include <string_view>
#include <thread>

#include "content_digest.h"
#include "protocol.h"

extern char **environ;

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr std::string_view kControlDirectory = "/snapshot-control";
constexpr std::string_view kSocketPrefix = "cuinterpose-";
constexpr std::string_view kSocketSuffix = ".sock";
constexpr std::string_view kStateFilename = "cuinterpose.state";
constexpr size_t kMaximumOutput = 64 << 10;
constexpr uintmax_t kMaximumStateBytes = CUINTERPOSE_MAX_STATE_BYTES;
constexpr uint32_t kCuinterposeProtocolVersion = CUINTERPOSE_VERSION;

class Pipe {
public:
  Pipe() {
    if (pipe(fds_.data()) != 0)
      throw std::runtime_error("create cuinterpose output pipe: " +
                               std::string(std::strerror(errno)));
    for (const int fd : fds_) {
      const int flags = fcntl(fd, F_GETFD);
      if (flags < 0 || fcntl(fd, F_SETFD, flags | FD_CLOEXEC) != 0) {
        const int saved = errno;
        close(fds_[0]);
        close(fds_[1]);
        fds_ = {-1, -1};
        throw std::runtime_error("configure cuinterpose output pipe: " +
                                 std::string(std::strerror(saved)));
      }
    }
  }
  Pipe(const Pipe &) = delete;
  Pipe &operator=(const Pipe &) = delete;
  ~Pipe() {
    for (const int fd : fds_) {
      if (fd >= 0)
        close(fd);
    }
  }
  int read_fd() const { return fds_[0]; }
  int write_fd() const { return fds_[1]; }
  void CloseWrite() {
    if (fds_[1] >= 0) {
      close(fds_[1]);
      fds_[1] = -1;
    }
  }

private:
  std::array<int, 2> fds_{-1, -1};
};

class SpawnActions {
public:
  SpawnActions() {
    const int status = posix_spawn_file_actions_init(&actions_);
    if (status != 0)
      throw std::runtime_error("initialize cuinterpose spawn actions: " +
                               std::string(std::strerror(status)));
  }
  SpawnActions(const SpawnActions &) = delete;
  SpawnActions &operator=(const SpawnActions &) = delete;
  ~SpawnActions() { posix_spawn_file_actions_destroy(&actions_); }
  posix_spawn_file_actions_t *get() { return &actions_; }

private:
  posix_spawn_file_actions_t actions_{};
};

class SpawnAttributes {
public:
  SpawnAttributes() {
    int status = posix_spawnattr_init(&attributes_);
    if (status != 0)
      throw std::runtime_error("initialize cuinterpose spawn attributes: " +
                               std::string(std::strerror(status)));
    status = posix_spawnattr_setflags(&attributes_, POSIX_SPAWN_SETPGROUP);
    if (status == 0)
      status = posix_spawnattr_setpgroup(&attributes_, 0);
    if (status != 0) {
      posix_spawnattr_destroy(&attributes_);
      throw std::runtime_error("configure cuinterpose spawn attributes: " +
                               std::string(std::strerror(status)));
    }
  }
  SpawnAttributes(const SpawnAttributes &) = delete;
  SpawnAttributes &operator=(const SpawnAttributes &) = delete;
  ~SpawnAttributes() { posix_spawnattr_destroy(&attributes_); }
  posix_spawnattr_t *get() { return &attributes_; }

private:
  posix_spawnattr_t attributes_{};
};

void AppendOutput(int fd, std::string *output, bool *eof) {
  std::array<char, 4096> buffer{};
  while (true) {
    const ssize_t count = read(fd, buffer.data(), buffer.size());
    if (count > 0) {
      const size_t available = kMaximumOutput - output->size();
      output->append(buffer.data(),
                     std::min(available, static_cast<size_t>(count)));
      continue;
    }
    if (count == 0) {
      *eof = true;
      return;
    }
    if (errno == EINTR)
      continue;
    if (errno == EAGAIN || errno == EWOULDBLOCK)
      return;
    *eof = true;
    return;
  }
}

std::string ExitDescription(int status) {
  if (WIFEXITED(status))
    return "exit status " + std::to_string(WEXITSTATUS(status));
  if (WIFSIGNALED(status))
    return "signal " + std::to_string(WTERMSIG(status));
  return "unknown wait status " + std::to_string(status);
}

} // namespace

CuinterposeCoordinator::CuinterposeCoordinator(fs::path binary,
                                               fs::path process_root,
                                               std::chrono::seconds timeout)
    : binary_(std::move(binary)), process_root_(std::move(process_root)),
      timeout_(timeout) {
  if (!binary_.is_absolute() || !process_root_.is_absolute() || timeout_ <=
                                                                 std::chrono::seconds::zero())
    throw std::invalid_argument(
        "cuinterpose coordinator paths must be absolute and timeout positive");
}

bool CuinterposeCoordinator::ValidateEndpoints(
    const std::vector<CuinterposeTarget> &targets, bool expected,
    std::string *error) const {
  if (error == nullptr)
    return false;
  size_t sockets = 0;
  size_t present = 0;
  for (const auto &target : targets) {
    const fs::path endpoint =
        process_root_ / std::to_string(target.host_pid) / "root" /
        kControlDirectory.substr(1) /
        (std::string(kSocketPrefix) + std::to_string(target.namespace_pid) +
         std::string(kSocketSuffix));
    struct stat metadata {};
    if (lstat(endpoint.c_str(), &metadata) == 0) {
      ++present;
      if (S_ISSOCK(metadata.st_mode))
        ++sockets;
      continue;
    }
    if (errno != ENOENT) {
      *error = "inspect cuinterpose endpoint " + endpoint.string() + ": " +
               std::strerror(errno);
      return false;
    }
  }
  if (!expected && present == 0) {
    error->clear();
    return true;
  }
  if (!expected) {
    *error = "cuinterpose endpoints are present but the request did not opt in";
    return false;
  }
  if (sockets != targets.size()) {
    *error = "cuinterpose endpoint missing or invalid for " +
             std::to_string(targets.size() - sockets) + " of " +
             std::to_string(targets.size()) + " CUDA targets";
    return false;
  }
  error->clear();
  return true;
}

bool CuinterposeCoordinator::ReadState(
    const fs::path &staging_directory, uint32_t participant_count,
    CuinterposeStateMetadata *metadata, std::string *error) const {
  if (metadata == nullptr || error == nullptr)
    return false;
  const fs::path state = staging_directory / kStateFilename;
  const int fd = open(state.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (fd < 0) {
    *error = "open cuinterpose state " + state.string() + ": " +
             std::strerror(errno);
    return false;
  }
  struct stat file_stat {};
  if (fstat(fd, &file_stat) != 0) {
    *error = "inspect open cuinterpose state: " +
             std::string(std::strerror(errno));
    close(fd);
    return false;
  }
  if (!S_ISREG(file_stat.st_mode) || file_stat.st_size <= 0 ||
      static_cast<uintmax_t>(file_stat.st_size) > kMaximumStateBytes) {
    *error = "cuinterpose state is not a bounded nonempty regular file";
    close(fd);
    return false;
  }
  cuda_checkpoint_storage::ContentDigest digest;
  std::array<char, 1 << 16> buffer{};
  uint64_t total = 0;
  while (true) {
    ssize_t count;
    do {
      count = read(fd, buffer.data(), buffer.size());
    } while (count < 0 && errno == EINTR);
    if (count < 0) {
      *error = "read cuinterpose state: " + std::string(std::strerror(errno));
      close(fd);
      return false;
    }
    if (count == 0)
      break;
    total += static_cast<uint64_t>(count);
    if (total > kMaximumStateBytes ||
        !digest.Update(buffer.data(), static_cast<size_t>(count), error)) {
      close(fd);
      return false;
    }
  }
  if (close(fd) != 0) {
    *error = "close cuinterpose state: " + std::string(std::strerror(errno));
    return false;
  }
  std::string sha256;
  if (!digest.Finalize(&sha256, error))
    return false;
  if (total != static_cast<uint64_t>(file_stat.st_size)) {
    *error = "cuinterpose state changed while it was hashed";
    return false;
  }
  metadata->protocol_version = kCuinterposeProtocolVersion;
  metadata->size_bytes = total;
  metadata->sha256 = std::move(sha256);
  metadata->participant_count = participant_count;
  error->clear();
  return true;
}

CuinterposeResult CuinterposeCoordinator::Prepare(
    const std::vector<CuinterposeTarget> &targets,
    const fs::path &staging_directory,
    const std::atomic<bool> &shutting_down,
    bool reject_legacy_ipc) const {
  return Run(reject_legacy_ipc ? "prepare-custom-storage" : "prepare",
             targets, staging_directory, shutting_down);
}

CuinterposeResult CuinterposeCoordinator::ValidateState(
    const fs::path &staging_directory,
    const std::atomic<bool> &shutting_down) const {
  return Run("validate", {}, staging_directory, shutting_down);
}

CuinterposeResult CuinterposeCoordinator::Restore(
    const std::vector<CuinterposeTarget> &targets,
    const fs::path &staging_directory,
    const std::atomic<bool> &shutting_down) const {
  return Run("restore", targets, staging_directory, shutting_down);
}

CuinterposeResult CuinterposeCoordinator::Run(
    const char *operation, const std::vector<CuinterposeTarget> &targets,
    const fs::path &staging_directory,
    const std::atomic<bool> &shutting_down) const {
  CuinterposeResult result;
  const bool validate_only = std::string_view(operation) == "validate";
  if (targets.empty() && !validate_only) {
    result.error = "cuinterpose coordinator requires at least one target";
    return result;
  }
  if (shutting_down.load(std::memory_order_acquire)) {
    result.error = "PageBroker CUDA engine is shutting down";
    return result;
  }

  std::vector<std::string> arguments{binary_.string(),
                                     std::string("--") + operation};
  if (validate_only) {
    arguments.push_back("--checkpoint-dir");
    arguments.push_back(staging_directory.string());
  } else {
    arguments.insert(arguments.end(),
                     {"--proc-root", process_root_.string(),
                      "--checkpoint-dir", staging_directory.string(),
                      "--control-dir", std::string(kControlDirectory)});
    for (const auto &target : targets) {
      arguments.push_back("--process");
      arguments.push_back(std::to_string(target.host_pid));
      arguments.push_back(std::to_string(target.namespace_pid));
    }
  }
  std::vector<char *> argv;
  argv.reserve(arguments.size() + 1);
  for (auto &argument : arguments)
    argv.push_back(argument.data());
  argv.push_back(nullptr);

  try {
    Pipe output_pipe;
    SpawnActions actions;
    SpawnAttributes attributes;
    int status = posix_spawn_file_actions_adddup2(
        actions.get(), output_pipe.write_fd(), STDOUT_FILENO);
    if (status == 0)
      status = posix_spawn_file_actions_adddup2(
          actions.get(), output_pipe.write_fd(), STDERR_FILENO);
    if (status == 0)
      status = posix_spawn_file_actions_addclose(actions.get(),
                                                 output_pipe.read_fd());
    if (status == 0)
      status = posix_spawn_file_actions_addclose(actions.get(),
                                                 output_pipe.write_fd());
    if (status != 0) {
      result.error = "configure cuinterpose coordinator output: " +
                     std::string(std::strerror(status));
      return result;
    }

    pid_t child = -1;
    status = posix_spawn(&child, binary_.c_str(), actions.get(),
                         attributes.get(), argv.data(), environ);
    if (status != 0) {
      result.error = "start cuinterpose coordinator: " +
                     std::string(std::strerror(status));
      return result;
    }
    result.dispatched = true;
    output_pipe.CloseWrite();
    const int current_flags = fcntl(output_pipe.read_fd(), F_GETFL);
    if (current_flags >= 0)
      (void)fcntl(output_pipe.read_fd(), F_SETFL, current_flags | O_NONBLOCK);

    const auto deadline = std::chrono::steady_clock::now() + timeout_;
    bool eof = false;
    bool reaped = false;
    bool cancelled = false;
    bool timed_out = false;
    int wait_status = 0;
    while (!reaped || !eof) {
      AppendOutput(output_pipe.read_fd(), &result.error, &eof);
      if (!reaped) {
        pid_t waited;
        do {
          waited = waitpid(child, &wait_status, WNOHANG);
        } while (waited < 0 && errno == EINTR);
        if (waited == child) {
          reaped = true;
        } else if (waited < 0) {
          result.error = "wait for cuinterpose coordinator: " +
                         std::string(std::strerror(errno));
          return result;
        }
      }
      if ((!reaped || !eof) && !cancelled && !timed_out &&
          (shutting_down.load(std::memory_order_acquire) ||
           std::chrono::steady_clock::now() >= deadline)) {
        cancelled = shutting_down.load(std::memory_order_acquire);
        timed_out = !cancelled;
        if (kill(-child, SIGKILL) != 0 && errno != ESRCH)
          (void)kill(child, SIGKILL);
      }
      if (!reaped || !eof)
        std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }

    if (!result.error.empty()) {
      std::fprintf(stderr, "cuinterpose coordinator operation=%s output=%s\n",
                   operation, result.error.c_str());
      std::fflush(stderr);
    }
    if (cancelled) {
      result.error = "cuinterpose coordinator cancelled during shutdown; " +
                     result.error;
      return result;
    }
    if (timed_out) {
      result.error = "cuinterpose coordinator timed out after " +
                     std::to_string(timeout_.count()) + " seconds; " +
                     result.error;
      return result;
    }
    if (!WIFEXITED(wait_status) || WEXITSTATUS(wait_status) != 0) {
      result.error = "cuinterpose coordinator failed with " +
                     ExitDescription(wait_status) + "; " + result.error;
      return result;
    }
    result.succeeded = true;
    return result;
  } catch (const std::exception &error) {
    result.error = error.what();
    return result;
  }
}

} // namespace snapshot::pagebroker
