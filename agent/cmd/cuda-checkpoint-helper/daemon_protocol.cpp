/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "daemon_protocol.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <climits>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <limits>
#include <poll.h>
#include <sstream>
#include <sys/file.h>
#include <sys/signalfd.h>
#include <sys/random.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <unistd.h>
#include <utility>

namespace cuda_checkpoint_daemon {
namespace {

uint16_t ReadU16(const unsigned char *data) {
  return static_cast<uint16_t>(data[0]) | static_cast<uint16_t>(data[1]) << 8;
}

uint32_t ReadU32(const unsigned char *data) {
  return static_cast<uint32_t>(data[0]) | static_cast<uint32_t>(data[1]) << 8 |
         static_cast<uint32_t>(data[2]) << 16 |
         static_cast<uint32_t>(data[3]) << 24;
}

uint64_t ReadU64(const unsigned char *data) {
  return static_cast<uint64_t>(ReadU32(data)) |
         static_cast<uint64_t>(ReadU32(data + 4)) << 32;
}

void WriteU16(std::vector<unsigned char> *data, size_t offset, uint16_t value) {
  (*data)[offset] = value & 0xff;
  (*data)[offset + 1] = value >> 8;
}

void WriteU32(std::vector<unsigned char> *data, size_t offset, uint32_t value) {
  for (size_t index = 0; index < 4; ++index) {
    (*data)[offset + index] = value >> (index * 8);
  }
}

void WriteU64(std::vector<unsigned char> *data, size_t offset, uint64_t value) {
  WriteU32(data, offset, value & 0xffffffffU);
  WriteU32(data, offset + 4, value >> 32);
}

bool ContainsNul(const std::string &value) {
  return value.find('\0') != std::string::npos;
}

bool IsAllowedStorageDirectory(const std::string &value) {
  const std::filesystem::path path(value);
  const std::filesystem::path root("/checkpoints");
  if (!path.is_absolute() || path.lexically_normal() != path || path == root) {
    return false;
  }
  const auto mismatch = std::mismatch(root.begin(), root.end(), path.begin(),
                                      path.end());
  return mismatch.first == root.end();
}

bool MakeSocketAddress(const std::string &path, sockaddr_un *address) {
  if (path.empty() || path.front() != '/' ||
      path.size() >= sizeof(address->sun_path)) {
    return false;
  }
  *address = {};
  address->sun_family = AF_UNIX;
  std::memcpy(address->sun_path, path.c_str(), path.size() + 1);
  return true;
}

bool SocketConfirmedStale(const sockaddr_un &address) {
  const int probe_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
  if (probe_fd < 0) {
    return false;
  }
  const int result = connect(
      probe_fd, reinterpret_cast<const sockaddr *>(&address), sizeof(address));
  const int connect_error = errno;
  close(probe_fd);
  return result != 0 && connect_error == ECONNREFUSED;
}

uint64_t DurationSeconds(std::chrono::steady_clock::time_point start,
                         std::chrono::steady_clock::time_point end) {
  return std::chrono::duration_cast<std::chrono::seconds>(end - start).count();
}

int WriteWake(int fd) {
  const unsigned char wake = 1;
  ssize_t written;
  do {
    written = write(fd, &wake, sizeof(wake));
  } while (written < 0 && errno == EINTR);
  if (written == static_cast<ssize_t>(sizeof(wake))) {
    return 0;
  }
  return written < 0 ? errno : EIO;
}

void SetError(std::string *error, const char *operation, int error_code) {
  *error = std::string(operation) + ": " + std::strerror(error_code);
}

} // namespace

bool ParseRequest(const unsigned char *data, size_t size, Request *request,
                  std::string *error) {
  if (data == nullptr || request == nullptr || size < kRequestHeaderSize ||
      size > kMaxRequestSize) {
    *error = "invalid request size";
    return false;
  }
  if (ReadU32(data) != kMagic || ReadU16(data + 4) != kVersion ||
      ReadU16(data + 6) != kRequestHeaderSize) {
    *error = "invalid request protocol header";
    return false;
  }
  const auto action = static_cast<Action>(ReadU16(data + 8));
  const auto backend = static_cast<Backend>(ReadU16(data + 10));
  if (action != Action::kHealth && action != Action::kCheckpoint &&
      action != Action::kRestore && action != Action::kLock &&
      action != Action::kUnlock) {
    *error = "invalid request action";
    return false;
  }
  const uint32_t device_map_size = ReadU32(data + 28);
  const uint32_t storage_dir_size = ReadU32(data + 32);
  const uint32_t cgroup_size = ReadU32(data + 36);
  const uint32_t job_file_size = ReadU32(data + 48);
  const uint32_t selected_devices_size = ReadU32(data + 52);
  if (device_map_size > kMaxRequestSize || storage_dir_size > kMaxRequestSize ||
      cgroup_size > kMaxCgroupSize || job_file_size > kMaxJobFileSize ||
      selected_devices_size > kMaxRequestSize ||
      static_cast<size_t>(device_map_size) + storage_dir_size + cgroup_size +
              job_file_size + selected_devices_size !=
          size - kRequestHeaderSize) {
    *error = "invalid request payload lengths";
    return false;
  }
  Request parsed;
  parsed.action = action;
  parsed.backend = backend;
  parsed.pid = ReadU32(data + 12);
  parsed.transfer_buffer_count = ReadU32(data + 16);
  parsed.transfer_chunk_bytes = ReadU64(data + 20);
  parsed.expected_start_time_ticks = ReadU64(data + 40);
  const char *payload =
      reinterpret_cast<const char *>(data + kRequestHeaderSize);
  parsed.device_map.assign(payload, device_map_size);
  parsed.storage_dir.assign(payload + device_map_size, storage_dir_size);
  parsed.expected_cgroup.assign(payload + device_map_size + storage_dir_size,
                                cgroup_size);
  parsed.job_file.assign(payload + device_map_size + storage_dir_size +
                             cgroup_size,
                         job_file_size);
  parsed.selected_devices.assign(
      payload + device_map_size + storage_dir_size + cgroup_size +
          job_file_size,
      selected_devices_size);
  if (ContainsNul(parsed.device_map) || ContainsNul(parsed.storage_dir) ||
      ContainsNul(parsed.expected_cgroup) || ContainsNul(parsed.job_file) ||
      ContainsNul(parsed.selected_devices)) {
    *error = "request strings contain NUL";
    return false;
  }
  if (action == Action::kHealth) {
    if (parsed.backend != Backend::kUnspecified || parsed.pid != 0 ||
        parsed.transfer_buffer_count != 0 || parsed.transfer_chunk_bytes != 0 ||
        parsed.expected_start_time_ticks != 0 || !parsed.device_map.empty() ||
        !parsed.storage_dir.empty() || !parsed.expected_cgroup.empty() ||
        !parsed.job_file.empty() || !parsed.selected_devices.empty()) {
      *error = "health request has arguments";
      return false;
    }
  } else if (parsed.pid == 0 || parsed.pid > INT_MAX ||
             parsed.expected_start_time_ticks == 0 ||
             parsed.expected_cgroup.empty()) {
    *error = "invalid operation arguments";
    return false;
  } else if (action == Action::kLock || action == Action::kUnlock) {
    if ((parsed.backend != Backend::kRegular &&
         parsed.backend != Backend::kPosix) ||
        parsed.transfer_buffer_count != 0 || parsed.transfer_chunk_bytes != 0 ||
        !parsed.device_map.empty() || !parsed.storage_dir.empty() ||
        !parsed.selected_devices.empty()) {
      *error = "lock/unlock request has transfer arguments";
      return false;
    }
  } else {
    if (backend != Backend::kRegular && backend != Backend::kPosix) {
      *error = "checkpoint/restore request has invalid backend";
      return false;
    }
    if (action == Action::kCheckpoint && !parsed.device_map.empty()) {
      *error = "checkpoint request has a device map";
      return false;
    }
    if (backend == Backend::kRegular) {
      if (parsed.transfer_buffer_count != 0 ||
          parsed.transfer_chunk_bytes != 0 || !parsed.storage_dir.empty() ||
          !parsed.selected_devices.empty()) {
        *error = "regular backend request has custom-storage arguments";
        return false;
      }
    } else if (parsed.transfer_buffer_count == 0 ||
               parsed.transfer_chunk_bytes == 0 || parsed.storage_dir.empty() ||
               !IsAllowedStorageDirectory(parsed.storage_dir) ||
               parsed.selected_devices.empty()) {
      *error = "POSIX backend request has invalid transfer arguments";
      return false;
    }
  }
  if (!parsed.job_file.empty() && parsed.job_file.front() != '/') {
    *error = "job file must be absolute";
    return false;
  }
  *request = std::move(parsed);
  return true;
}

bool EncodeRequest(const Request &request, std::vector<unsigned char> *data,
                   std::string *error) {
  if (request.device_map.size() > std::numeric_limits<uint32_t>::max() ||
      request.storage_dir.size() > std::numeric_limits<uint32_t>::max() ||
      request.expected_cgroup.size() > kMaxCgroupSize ||
      request.job_file.size() > kMaxJobFileSize ||
      request.selected_devices.size() >
          std::numeric_limits<uint32_t>::max() ||
      kRequestHeaderSize + request.device_map.size() +
              request.storage_dir.size() + request.expected_cgroup.size() +
              request.job_file.size() + request.selected_devices.size() >
          kMaxRequestSize) {
    *error = "request is too large";
    return false;
  }
  data->assign(kRequestHeaderSize + request.device_map.size() +
                   request.storage_dir.size() + request.expected_cgroup.size() +
                   request.job_file.size() + request.selected_devices.size(),
               0);
  WriteU32(data, 0, kMagic);
  WriteU16(data, 4, kVersion);
  WriteU16(data, 6, kRequestHeaderSize);
  WriteU16(data, 8, static_cast<uint16_t>(request.action));
  WriteU16(data, 10, static_cast<uint16_t>(request.backend));
  WriteU32(data, 12, request.pid);
  WriteU32(data, 16, request.transfer_buffer_count);
  WriteU64(data, 20, request.transfer_chunk_bytes);
  WriteU32(data, 28, request.device_map.size());
  WriteU32(data, 32, request.storage_dir.size());
  WriteU32(data, 36, request.expected_cgroup.size());
  WriteU64(data, 40, request.expected_start_time_ticks);
  WriteU32(data, 48, request.job_file.size());
  WriteU32(data, 52, request.selected_devices.size());
  std::memcpy(data->data() + kRequestHeaderSize, request.device_map.data(),
              request.device_map.size());
  std::memcpy(data->data() + kRequestHeaderSize + request.device_map.size(),
              request.storage_dir.data(), request.storage_dir.size());
  std::memcpy(data->data() + kRequestHeaderSize + request.device_map.size() +
                  request.storage_dir.size(),
              request.expected_cgroup.data(), request.expected_cgroup.size());
  std::memcpy(data->data() + kRequestHeaderSize + request.device_map.size() +
                  request.storage_dir.size() + request.expected_cgroup.size(),
              request.job_file.data(), request.job_file.size());
  std::memcpy(data->data() + kRequestHeaderSize + request.device_map.size() +
                  request.storage_dir.size() + request.expected_cgroup.size() +
                  request.job_file.size(),
              request.selected_devices.data(), request.selected_devices.size());
  return true;
}

bool ParseResponse(const unsigned char *data, size_t size, Response *response,
                   std::string *error) {
  if (data == nullptr || response == nullptr || size < kResponseHeaderSize ||
      size > kMaxResponseSize) {
    *error = "invalid response size";
    return false;
  }
  if (ReadU32(data) != kMagic || ReadU16(data + 4) != kVersion ||
      ReadU16(data + 6) != kResponseHeaderSize) {
    *error = "invalid response protocol header";
    return false;
  }
  const uint32_t flags = ReadU32(data + 12);
  if ((flags & ~(kResponseFatal | kResponseCapabilityDeferredCUDA |
                 kResponseCapabilityCustomStorage |
                 kResponseLockNotAcquired)) != 0) {
    *error = "invalid response flags";
    return false;
  }
  const uint32_t output_size = ReadU32(data + 16);
  const uint32_t error_size = ReadU32(data + 20);
  if (static_cast<size_t>(output_size) + error_size !=
      size - kResponseHeaderSize) {
    *error = "invalid response payload lengths";
    return false;
  }
  response->cuda_status = static_cast<int32_t>(ReadU32(data + 8));
  response->flags = flags;
  const char *payload =
      reinterpret_cast<const char *>(data + kResponseHeaderSize);
  response->output.assign(payload, output_size);
  response->error.assign(payload + output_size, error_size);
  return true;
}

bool EncodeResponse(const Response &response, std::vector<unsigned char> *data,
                    std::string *error) {
  if (response.output.size() > std::numeric_limits<uint32_t>::max() ||
      response.error.size() > std::numeric_limits<uint32_t>::max() ||
      kResponseHeaderSize + response.output.size() + response.error.size() >
          kMaxResponseSize) {
    *error = "response is too large";
    return false;
  }
  data->assign(
      kResponseHeaderSize + response.output.size() + response.error.size(), 0);
  WriteU32(data, 0, kMagic);
  WriteU16(data, 4, kVersion);
  WriteU16(data, 6, kResponseHeaderSize);
  WriteU32(data, 8, static_cast<uint32_t>(response.cuda_status));
  WriteU32(data, 12, response.flags);
  WriteU32(data, 16, response.output.size());
  WriteU32(data, 20, response.error.size());
  std::memcpy(data->data() + kResponseHeaderSize, response.output.data(),
              response.output.size());
  std::memcpy(data->data() + kResponseHeaderSize + response.output.size(),
              response.error.data(), response.error.size());
  return true;
}

ProcessExistenceState InspectProcessExistence(uint32_t pid,
                                              const std::string &proc_root,
                                              std::string *error) {
  const std::filesystem::path process_dir =
      std::filesystem::path(proc_root) / std::to_string(pid);
  std::error_code exists_error;
  const bool process_exists = std::filesystem::exists(process_dir, exists_error);
  if (exists_error) {
    *error = "failed to inspect current process directory: " +
             exists_error.message();
    return ProcessExistenceState::kIndeterminate;
  }
  if (!process_exists) {
    *error = "current process no longer exists";
    return ProcessExistenceState::kMissing;
  }
  return ProcessExistenceState::kExists;
}

ProcessIdentityState InspectProcessIdentity(const Request &request,
                                            const std::string &proc_root,
                                            std::string *error) {
  const ProcessExistenceState existence =
      InspectProcessExistence(request.pid, proc_root, error);
  if (existence == ProcessExistenceState::kMissing) {
    return ProcessIdentityState::kExitedOrReused;
  }
  if (existence == ProcessExistenceState::kIndeterminate) {
    return ProcessIdentityState::kIndeterminate;
  }
  const std::filesystem::path process_dir =
      std::filesystem::path(proc_root) / std::to_string(request.pid);
  std::ifstream stat(process_dir / "stat");
  std::string stat_line;
  if (!std::getline(stat, stat_line)) {
    *error = "failed to read current process stat";
    return ProcessIdentityState::kIndeterminate;
  }
  const size_t closing_paren = stat_line.rfind(')');
  if (closing_paren == std::string::npos ||
      closing_paren + 2 >= stat_line.size()) {
    *error = "malformed current process stat";
    return ProcessIdentityState::kIndeterminate;
  }
  std::istringstream fields(stat_line.substr(closing_paren + 2));
  std::string field;
  uint64_t start_time_ticks = 0;
  for (size_t index = 0; index < 20; ++index) {
    if (!(fields >> field)) {
      *error = "malformed current process stat fields";
      return ProcessIdentityState::kIndeterminate;
    }
    if (index == 19) {
      char *end = nullptr;
      errno = 0;
      const unsigned long long parsed = std::strtoull(field.c_str(), &end, 10);
      if (errno != 0 || end == nullptr || *end != '\0') {
        *error = "invalid current process start time";
        return ProcessIdentityState::kIndeterminate;
      }
      start_time_ticks = parsed;
    }
  }
  std::ifstream cgroup_file(process_dir / "cgroup");
  std::ostringstream cgroup_contents;
  cgroup_contents << cgroup_file.rdbuf();
  const std::string cgroup = cgroup_contents.str();
  if ((!cgroup_file.good() && !cgroup_file.eof()) || cgroup.empty() ||
      cgroup.size() > kMaxCgroupSize) {
    *error = "failed to read valid current process cgroup";
    return ProcessIdentityState::kIndeterminate;
  }
  if (start_time_ticks != request.expected_start_time_ticks) {
    *error = "start time mismatch";
    return ProcessIdentityState::kExitedOrReused;
  }
  if (cgroup != request.expected_cgroup) {
    *error = "cgroup changed while PID and start time still match";
    return ProcessIdentityState::kIndeterminate;
  }
  return ProcessIdentityState::kMatches;
}

bool TerminateMatchingProcesses(const std::vector<Request> &requests,
                                const std::string &proc_root,
                                std::chrono::milliseconds timeout,
                                std::string *error) {
  if (error == nullptr) {
    return false;
  }
  for (const auto &request : requests) {
    std::string identity_error;
    if (InspectProcessIdentity(request, proc_root, &identity_error) ==
        ProcessIdentityState::kIndeterminate) {
      *error = "cannot safely terminate target " +
               std::to_string(request.pid) + ": " + identity_error;
      return false;
    }
  }

  struct PidfdTarget {
    const Request *request;
    int pidfd;
  };
  std::vector<PidfdTarget> targets;
  auto close_targets = [&targets]() {
    for (const auto &target : targets) {
      (void)close(target.pidfd);
    }
    targets.clear();
  };
  for (const auto &request : requests) {
    std::string identity_error;
    if (InspectProcessIdentity(request, proc_root, &identity_error) !=
        ProcessIdentityState::kMatches) {
      continue;
    }
    const int pidfd = static_cast<int>(
        syscall(SYS_pidfd_open, static_cast<pid_t>(request.pid), 0));
    if (pidfd < 0) {
      if (errno == ESRCH) {
        continue;
      }
      close_targets();
      *error = "pidfd_open target " + std::to_string(request.pid) + ": " +
               std::strerror(errno);
      return false;
    }
    const auto post_open_state =
        InspectProcessIdentity(request, proc_root, &identity_error);
    if (post_open_state == ProcessIdentityState::kIndeterminate) {
      (void)close(pidfd);
      close_targets();
      *error = "cannot safely signal target " +
               std::to_string(request.pid) + ": " + identity_error;
      return false;
    }
    if (post_open_state == ProcessIdentityState::kExitedOrReused) {
      (void)close(pidfd);
      continue;
    }
    targets.push_back(PidfdTarget{.request = &request, .pidfd = pidfd});
  }
  // Every target identity is validated and pinned by pidfd before the first
  // signal. A late identity error therefore cannot leave an earlier sibling
  // terminated while a later sibling was never eligible for termination.
  for (const auto &target : targets) {
    if (syscall(SYS_pidfd_send_signal, target.pidfd, SIGKILL, nullptr, 0) != 0 &&
        errno != ESRCH) {
      const int signal_error = errno;
      const uint32_t target_pid = target.request->pid;
      close_targets();
      *error = "pidfd_send_signal target " + std::to_string(target_pid) + ": " +
               std::strerror(signal_error);
      return false;
    }
  }
  close_targets();
  const auto deadline = std::chrono::steady_clock::now() + timeout;
  for (;;) {
    bool all_exited = true;
    for (const auto &request : requests) {
      std::string identity_error;
      const auto state =
          InspectProcessIdentity(request, proc_root, &identity_error);
      if (state == ProcessIdentityState::kIndeterminate) {
        *error = "cannot confirm target " + std::to_string(request.pid) +
                 " terminated: " + identity_error;
        return false;
      }
      if (state == ProcessIdentityState::kMatches) {
        all_exited = false;
      }
    }
    if (all_exited) {
      return true;
    }
    if (std::chrono::steady_clock::now() >= deadline) {
      *error = "timed out confirming retained targets terminated";
      return false;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
}

bool ValidateProcessIdentity(const Request &request,
                             const std::string &proc_root, std::string *error) {
  return InspectProcessIdentity(request, proc_root, error) ==
         ProcessIdentityState::kMatches;
}

bool ExecuteValidated(const Request &request, const std::string &proc_root,
                      const OperationExecutor &executor, Response *response) {
  if (request.action == Action::kHealth) {
    response->cuda_status = 1;
    response->error = "health requests must use the health socket";
    return true;
  }
  std::string identity_error;
  if (!ValidateProcessIdentity(request, proc_root, &identity_error)) {
    response->cuda_status = 1;
    if (request.action == Action::kLock) {
      response->flags |= kResponseLockNotAcquired;
    }
    response->error =
        "process identity changed before CUDA operation: " + identity_error;
    return true;
  }
  *response = executor(request);
  return ResponseAllowsServerContinue(*response);
}

bool ResponseAllowsServerContinue(const Response &response) {
  return (response.flags & kResponseFatal) == 0;
}

int32_t FinishHandledOperation(bool post_handle_succeeded,
                               int32_t failure_status,
                               const CompletionExecutor &completion,
                               OperationState *state) {
  state->handle_returned = true;
  if (!post_handle_succeeded) {
    return failure_status;
  }
  const int32_t status = completion();
  state->completion_succeeded = status == 0;
  return status;
}

OperationHealth::OperationHealth(std::chrono::seconds max_operation_duration,
                                 std::string incarnation)
    : max_operation_duration_(max_operation_duration),
      incarnation_(std::move(incarnation)) {}

void OperationHealth::MarkReady(bool custom_storage_available) {
  std::lock_guard<std::mutex> lock(mutex_);
  ready_ = true;
  custom_storage_available_ = custom_storage_available;
}

void OperationHealth::Begin(Action action, uint32_t pid) {
  std::lock_guard<std::mutex> lock(mutex_);
  const auto now = Clock::now();
  busy_ = true;
  action_ = action;
  pid_ = pid;
  started_ = now;
}

void OperationHealth::End() {
  std::lock_guard<std::mutex> lock(mutex_);
  busy_ = false;
  action_ = Action::kHealth;
  pid_ = 0;
}

HealthSnapshot OperationHealth::Snapshot() const {
  std::lock_guard<std::mutex> lock(mutex_);
  const auto now = Clock::now();
  HealthSnapshot snapshot{
      .ready = ready_,
      .busy = busy_,
      .healthy = ready_,
      .action = action_,
      .pid = pid_,
      .deadline_seconds =
          static_cast<uint64_t>(max_operation_duration_.count()),
      .custom_storage_available = custom_storage_available_,
      .incarnation = incarnation_,
  };
  if (busy_) {
    snapshot.elapsed_seconds = DurationSeconds(started_, now);
    snapshot.healthy = ready_ && now - started_ <= max_operation_duration_;
  }
  return snapshot;
}

Response HealthResponse(const OperationHealth &health) {
  const HealthSnapshot snapshot = health.Snapshot();
  Response response;
  response.cuda_status = snapshot.healthy ? 0 : 1;
  if (snapshot.ready) {
    response.flags = kResponseCapabilityDeferredCUDA;
    if (snapshot.custom_storage_available) {
      response.flags |= kResponseCapabilityCustomStorage;
    }
  }
  std::ostringstream output;
  output << "{\"ready\":" << (snapshot.ready ? "true" : "false")
         << ",\"busy\":" << (snapshot.busy ? "true" : "false")
         << ",\"healthy\":" << (snapshot.healthy ? "true" : "false")
         << ",\"action\":\"" << ActionName(snapshot.action) << "\""
         << ",\"pid\":" << snapshot.pid
         << ",\"elapsed_seconds\":" << snapshot.elapsed_seconds
         << ",\"deadline_seconds\":" << snapshot.deadline_seconds
         << ",\"custom_storage_available\":"
         << (snapshot.custom_storage_available ? "true" : "false")
         << ",\"incarnation\":\"" << snapshot.incarnation << "\"}\n";
  response.output = output.str();
  if (!snapshot.healthy) {
    response.error = snapshot.ready ? "operation exceeded watchdog deadline"
                                    : "daemon is not ready";
  }
  return response;
}

Response HealthResponseAfterReap(const OperationHealth &health,
                                 int32_t release_status,
                                 const std::string &reap_warning) {
  if (release_status != 0) {
    return Response{
        .cuda_status = release_status,
        .flags = kResponseFatal,
        .output = "",
        .error = "failed to release CUDA primary contexts for an exited "
                 "restore target",
    };
  }
  Response response = HealthResponse(health);
  if (!reap_warning.empty()) {
    response.error = reap_warning + "; retained contexts; reaping deferred";
  }
  return response;
}

bool OperationTimeoutMilliseconds(std::chrono::seconds duration,
                                  unsigned int *timeout_ms,
                                  std::string *error) {
  if (timeout_ms == nullptr || duration.count() <= 0 ||
      static_cast<uint64_t>(duration.count()) >
          std::numeric_limits<unsigned int>::max() / 1000ULL) {
    if (error != nullptr) {
      *error = "operation duration cannot be represented as a CUDA lock timeout";
    }
    return false;
  }
  *timeout_ms = static_cast<unsigned int>(duration.count() * 1000ULL);
  return true;
}

bool GenerateIncarnation(std::string *value, std::string *error) {
  if (value == nullptr || error == nullptr) {
    return false;
  }
  std::array<unsigned char, 16> bytes{};
  size_t offset = 0;
  while (offset < bytes.size()) {
    const ssize_t count =
        getrandom(bytes.data() + offset, bytes.size() - offset, 0);
    if (count > 0) {
      offset += static_cast<size_t>(count);
      continue;
    }
    if (count < 0 && errno == EINTR) {
      continue;
    }
    *error = std::string("generate daemon incarnation: ") +
             (count < 0 ? std::strerror(errno) : "short random read");
    return false;
  }
  bytes[6] = static_cast<unsigned char>((bytes[6] & 0x0fU) | 0x40U);
  bytes[8] = static_cast<unsigned char>((bytes[8] & 0x3fU) | 0x80U);
  static constexpr char hex[] = "0123456789abcdef";
  value->clear();
  value->reserve(36);
  for (size_t index = 0; index < bytes.size(); ++index) {
    if (index == 4 || index == 6 || index == 8 || index == 10) {
      value->push_back('-');
    }
    value->push_back(hex[bytes[index] >> 4]);
    value->push_back(hex[bytes[index] & 0x0fU]);
  }
  return true;
}

OwnedUnixSocket::~OwnedUnixSocket() { Close(); }

BoundedOutputCapture::~BoundedOutputCapture() noexcept {
  if (write_fd_ >= 0) {
    close(write_fd_);
    write_fd_ = -1;
  }
  if (reader_.joinable()) {
    reader_.join();
  }
  if (read_fd_ >= 0) {
    close(read_fd_);
    read_fd_ = -1;
  }
}

bool BoundedOutputCapture::Start(std::string *error) {
  int pipe_fds[2]{-1, -1};
  if (pipe2(pipe_fds, O_CLOEXEC) != 0) {
    if (error != nullptr) {
      *error = std::string("create bounded output pipe: ") +
               std::strerror(errno);
    }
    return false;
  }
  read_fd_ = pipe_fds[0];
  write_fd_ = pipe_fds[1];
  try {
    reader_ = std::thread(&BoundedOutputCapture::Drain, this);
  } catch (const std::system_error &e) {
    close(read_fd_);
    close(write_fd_);
    read_fd_ = -1;
    write_fd_ = -1;
    if (error != nullptr) {
      *error = std::string("start bounded output reader: ") + e.what();
    }
    return false;
  }
  return true;
}

void BoundedOutputCapture::Drain() noexcept {
  std::array<char, 4096> buffer{};
  for (;;) {
    const ssize_t count = read(read_fd_, buffer.data(), buffer.size());
    if (count > 0) {
      const size_t available = limit_ > output_.size() ? limit_ - output_.size() : 0;
      const size_t retained = std::min(available, static_cast<size_t>(count));
      output_.append(buffer.data(), retained);
      truncated_ = truncated_ || retained < static_cast<size_t>(count);
      continue;
    }
    if (count == 0) {
      return;
    }
    if (errno == EINTR) {
      continue;
    }
    read_error_ = std::strerror(errno);
    return;
  }
}

bool BoundedOutputCapture::Finish(std::string *output, bool *truncated,
                                  std::string *error) {
  if (write_fd_ >= 0) {
    close(write_fd_);
    write_fd_ = -1;
  }
  if (reader_.joinable()) {
    reader_.join();
  }
  if (read_fd_ >= 0) {
    close(read_fd_);
    read_fd_ = -1;
  }
  *output = std::move(output_);
  *truncated = truncated_;
  if (!read_error_.empty()) {
    if (error != nullptr) {
      *error = "read bounded output: " + read_error_;
    }
    return false;
  }
  return true;
}

bool OwnedUnixSocket::Bind(const std::string &path, int backlog,
                           std::string *error) {
  sockaddr_un address{};
  if (fd_ >= 0 || !MakeSocketAddress(path, &address)) {
    *error = "invalid socket path";
    return false;
  }
  const std::string lock_path = path + ".lock";
  lock_fd_ = open(lock_path.c_str(), O_CREAT | O_CLOEXEC | O_RDWR, 0600);
  if (lock_fd_ < 0 || flock(lock_fd_, LOCK_EX | LOCK_NB) != 0) {
    *error = lock_fd_ < 0 ? std::strerror(errno)
                          : "socket path is owned by another server";
    Close();
    return false;
  }
  fd_ = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
  if (fd_ < 0) {
    *error = std::strerror(errno);
    Close();
    return false;
  }
  struct stat existing{};
  if (lstat(path.c_str(), &existing) == 0) {
    if (!S_ISSOCK(existing.st_mode) || !SocketConfirmedStale(address)) {
      *error = "refusing to replace non-socket or live socket path";
      Close();
      return false;
    }
    if (unlink(path.c_str()) != 0) {
      *error = std::strerror(errno);
      Close();
      return false;
    }
  } else if (errno != ENOENT) {
    *error = std::strerror(errno);
    Close();
    return false;
  }
  if (bind(fd_, reinterpret_cast<sockaddr *>(&address), sizeof(address)) != 0) {
    *error = std::strerror(errno);
    Close();
    return false;
  }
  path_ = path;
  bound_ = true;
  struct stat bound_stat{};
  if (lstat(path.c_str(), &bound_stat) != 0 || !S_ISSOCK(bound_stat.st_mode)) {
    *error = "failed to inspect bound socket";
    Close();
    return false;
  }
  device_ = bound_stat.st_dev;
  inode_ = bound_stat.st_ino;
  if (backlog < 0) {
    *error = "invalid backlog";
    Close();
    return false;
  }
  if (chmod(path.c_str(), 0600) != 0 || listen(fd_, backlog) != 0) {
    *error = std::strerror(errno);
    Close();
    return false;
  }
  return true;
}

void OwnedUnixSocket::UnlinkIfOwned() {
  if (!bound_) {
    return;
  }
  struct stat current{};
  if (lstat(path_.c_str(), &current) == 0 &&
      static_cast<uint64_t>(current.st_dev) == device_ &&
      static_cast<uint64_t>(current.st_ino) == inode_) {
    (void)unlink(path_.c_str());
  }
  bound_ = false;
}

void OwnedUnixSocket::Close() {
  if (fd_ >= 0) {
    close(fd_);
    fd_ = -1;
  }
  UnlinkIfOwned();
  if (lock_fd_ >= 0) {
    close(lock_fd_);
    lock_fd_ = -1;
  }
}

ShutdownSignalOwner::~ShutdownSignalOwner() noexcept {
  (void)StopAndJoinNoThrow();
  Close();
}

bool ShutdownSignalOwner::Start(std::string *error) {
  if (thread_started_ || signal_fd_ >= 0) {
    *error = "shutdown signal owner already started";
    return false;
  }

  if (sigemptyset(&signals_) != 0 || sigaddset(&signals_, SIGTERM) != 0 ||
      sigaddset(&signals_, SIGINT) != 0) {
    SetError(error, "build shutdown signal set", errno);
    return false;
  }
  const int mask_error = pthread_sigmask(SIG_BLOCK, &signals_, nullptr);
  if (mask_error != 0) {
    SetError(error, "block shutdown signals", mask_error);
    return false;
  }
  // Keep the mask blocked until process exit. Restoring it after the owner is
  // joined would reopen a window for the default signal action during teardown.

  if (pipe2(operation_stop_pipe_, O_CLOEXEC | O_NONBLOCK) != 0) {
    SetError(error, "create operation shutdown pipe", errno);
    Close();
    return false;
  }
  if (pipe2(health_stop_pipe_, O_CLOEXEC | O_NONBLOCK) != 0) {
    SetError(error, "create health shutdown pipe", errno);
    Close();
    return false;
  }
  if (pipe2(control_pipe_, O_CLOEXEC | O_NONBLOCK) != 0) {
    SetError(error, "create signal owner control pipe", errno);
    Close();
    return false;
  }
  signal_fd_ = signalfd(-1, &signals_, SFD_CLOEXEC | SFD_NONBLOCK);
  if (signal_fd_ < 0) {
    SetError(error, "create shutdown signal fd", errno);
    Close();
    return false;
  }

  const int thread_error = pthread_create(
      &thread_, nullptr, &ShutdownSignalOwner::ThreadEntry, this);
  if (thread_error != 0) {
    SetError(error, "create shutdown signal thread", thread_error);
    Close();
    return false;
  }
  thread_started_ = true;
  return true;
}

bool ShutdownSignalOwner::StopAndJoin(std::string *error) {
  const ShutdownResult result = StopAndJoinNoThrow();
  if (!result.ok()) {
    SetError(error, result.operation, result.error_code);
    return false;
  }
  return true;
}

ShutdownSignalOwner::ShutdownResult
ShutdownSignalOwner::RequestShutdownNoThrow() noexcept {
  if (!thread_started_) {
    return {};
  }

  const int wake_error = WriteWake(control_pipe_[1]);
  if (wake_error != 0) {
    return {
        .operation = "wake shutdown signal thread",
        .error_code = wake_error,
    };
  }
  return {};
}

ShutdownSignalOwner::ShutdownResult
ShutdownSignalOwner::StopAndJoinNoThrow() noexcept {
  if (!thread_started_) {
    return {};
  }

  const ShutdownResult wake_result = RequestShutdownNoThrow();
  const int join_error = pthread_join(thread_, nullptr);
  if (join_error != 0) {
    return {
        .operation = "join shutdown signal thread",
        .error_code = join_error,
    };
  }
  thread_started_ = false;
  if (!wake_result.ok()) {
    return wake_result;
  }
  if (thread_error_ != 0) {
    return {
        .operation = "shutdown signal thread",
        .error_code = thread_error_,
    };
  }
  return {};
}

void ShutdownSignalOwner::Close() noexcept {
  if (thread_started_) {
    return;
  }
  for (int *pipe : {operation_stop_pipe_, health_stop_pipe_, control_pipe_}) {
    for (size_t index = 0; index < 2; ++index) {
      if (pipe[index] >= 0) {
        close(pipe[index]);
        pipe[index] = -1;
      }
    }
  }
  if (signal_fd_ >= 0) {
    close(signal_fd_);
    signal_fd_ = -1;
  }
}

void *ShutdownSignalOwner::ThreadEntry(void *owner) noexcept {
  static_cast<ShutdownSignalOwner *>(owner)->Run();
  return nullptr;
}

void ShutdownSignalOwner::Run() noexcept {
  pollfd descriptors[2]{
      {.fd = signal_fd_, .events = POLLIN, .revents = 0},
      {.fd = control_pipe_[0], .events = POLLIN, .revents = 0},
  };
  int poll_result;
  do {
    poll_result = poll(descriptors, 2, -1);
  } while (poll_result < 0 && errno == EINTR);
  if (poll_result < 0) {
    thread_error_ = errno;
  } else if ((descriptors[0].revents &
              (POLLIN | POLLERR | POLLHUP | POLLNVAL)) != 0) {
    signalfd_siginfo signal_info{};
    const ssize_t bytes = read(signal_fd_, &signal_info, sizeof(signal_info));
    if (bytes != static_cast<ssize_t>(sizeof(signal_info))) {
      thread_error_ = bytes < 0 ? errno : EIO;
    } else if (signal_info.ssi_signo != SIGTERM &&
               signal_info.ssi_signo != SIGINT) {
      thread_error_ = EINVAL;
    }
  } else if ((descriptors[1].revents &
              (POLLIN | POLLERR | POLLHUP | POLLNVAL)) != 0) {
    unsigned char control = 0;
    const ssize_t bytes = read(control_pipe_[0], &control, sizeof(control));
    if (bytes != static_cast<ssize_t>(sizeof(control))) {
      thread_error_ = bytes < 0 ? errno : EIO;
    }
  } else {
    thread_error_ = EIO;
  }

  shutdown_requested_.store(true, std::memory_order_release);
  const int operation_error = WriteWake(operation_stop_pipe_[1]);
  const int health_error = WriteWake(health_stop_pipe_[1]);
  if (thread_error_ == 0) {
    thread_error_ = operation_error != 0 ? operation_error : health_error;
  }
}

int PollForInputOrStop(int input_fd, int stop_fd,
                       const std::function<void()> &before_poll,
                       int timeout_milliseconds) {
  pollfd descriptors[2]{
      {.fd = input_fd, .events = POLLIN, .revents = 0},
      {.fd = stop_fd, .events = POLLIN, .revents = 0},
  };
  if (before_poll) {
    before_poll();
  }
  for (;;) {
    const int result = poll(descriptors, 2, timeout_milliseconds);
    if (result < 0 && errno == EINTR) {
      continue;
    }
    if (result == 0) {
      return -1;
    }
    if (result < 0) {
      return -2;
    }
    if ((descriptors[1].revents & (POLLIN | POLLERR | POLLHUP | POLLNVAL)) !=
        0) {
      return 0;
    }
    if ((descriptors[0].revents & (POLLIN | POLLERR | POLLHUP | POLLNVAL)) !=
        0) {
      return 1;
    }
  }
}

const char *ActionName(Action action) {
  switch (action) {
  case Action::kHealth:
    return "none";
  case Action::kCheckpoint:
    return "checkpoint";
  case Action::kRestore:
    return "restore";
  case Action::kLock:
    return "lock";
  case Action::kUnlock:
    return "unlock";
  }
  return "unknown";
}

} // namespace cuda_checkpoint_daemon
