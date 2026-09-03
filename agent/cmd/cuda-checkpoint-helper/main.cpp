/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include <cuda.h>

#include <cerrno>
#include <climits>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <string>

#include "daemon_protocol.h"
#include "daemon_server.h"

namespace daemon_protocol = cuda_checkpoint_daemon;

namespace {

constexpr uint64_t kMaxOperationSeconds = 60 * 60;

bool ParsePositiveSeconds(const char *value, uint64_t *seconds_out) {
  char *end = nullptr;
  errno = 0;
  const unsigned long long seconds = std::strtoull(value, &end, 10);
  if (value[0] == '\0' || end == nullptr || *end != '\0' || errno != 0 ||
      seconds == 0 ||
      seconds > kMaxOperationSeconds) {
    return false;
  }
  *seconds_out = seconds;
  return true;
}

int PrintUsage(FILE *stream) {
  return std::fprintf(stream,
                      "Usage:\n"
                      "  cuda-checkpoint-helper --get-restore-tid --pid <pid>\n"
                      "  cuda-checkpoint-helper --daemon --socket "
                      "<absolute-socket-path> "
                      "[--max-operation-seconds <seconds>]\n"
                      "  cuda-checkpoint-helper --health --socket "
                      "<absolute-socket-path>\n") < 0
             ? 1
             : 0;
}

int PrintUsageError() {
  (void)PrintUsage(stderr);
  return 1;
}

void PrintCudaError(CUresult status) {
  const char *name = nullptr;
  const char *message = nullptr;
  (void)cuGetErrorName(status, &name);
  (void)cuGetErrorString(status, &message);
  std::fprintf(stderr, "%s: %s\n",
               name == nullptr ? "CUDA_ERROR_UNKNOWN" : name,
               message == nullptr ? "unknown CUDA error" : message);
}

bool ParsePID(const char *value, int *pid_out) {
  char *end = nullptr;
  long pid = std::strtol(value, &end, 10);
  if (value[0] == '\0' || end == nullptr || *end != '\0' || pid <= 0 ||
      pid > INT_MAX) {
    return false;
  }
  *pid_out = static_cast<int>(pid);
  return true;
}

} // namespace

int main(int argc, char **argv) {
  int pid = 0;
  bool have_pid = false;
  bool get_restore_tid = false;
  bool daemon = false;
  bool health = false;
  std::string socket_path;
  uint64_t max_operation_seconds = kMaxOperationSeconds;

  if (argc == 1) {
    return PrintUsageError();
  }
  for (int index = 1; index < argc; ++index) {
    std::string argument = argv[index];
    if (argument == "--daemon") {
      daemon = true;
    } else if (argument == "--health") {
      health = true;
    } else if (argument == "--socket" && ++index < argc) {
      socket_path = argv[index];
    } else if (argument == "--max-operation-seconds" && ++index < argc &&
               ParsePositiveSeconds(argv[index], &max_operation_seconds)) {
    } else if (argument == "--get-restore-tid") {
      get_restore_tid = true;
    } else if ((argument == "--pid" || argument == "-p") && ++index < argc &&
               ParsePID(argv[index], &pid)) {
      have_pid = true;
    } else if (argument == "--help" || argument == "-h") {
      return PrintUsage(stdout);
    } else {
      return PrintUsageError();
    }
  }

  if (daemon || health) {
    if (static_cast<int>(daemon) + static_cast<int>(health) != 1 ||
        socket_path.empty() || have_pid || get_restore_tid ||
        (health && max_operation_seconds != kMaxOperationSeconds)) {
      return PrintUsageError();
    }
    return daemon
               ? cuda_checkpoint_server::RunDaemon(socket_path,
                                                    max_operation_seconds)
               : cuda_checkpoint_server::RunHealthClient(socket_path);
  }

  if (!get_restore_tid || !have_pid) {
    return PrintUsageError();
  }

  CUresult status = cuInit(0);
  if (status != CUDA_SUCCESS) {
    PrintCudaError(status);
    return 1;
  }
  int tid = 0;
  status = cuCheckpointProcessGetRestoreThreadId(pid, &tid);
  if (status == CUDA_ERROR_INVALID_VALUE) {
    std::string process_error;
    const daemon_protocol::ProcessExistenceState existence =
        daemon_protocol::InspectProcessExistence(pid, "/proc", &process_error);
    if (existence == daemon_protocol::ProcessExistenceState::kExists) {
      // The output pointer is valid and the candidate PID still exists. The
      // driver uses INVALID_VALUE for a live process without CUDA checkpoint
      // state. Keep that negative result distinct from helper, driver, and
      // raced-PID failures so the agent can fail closed on the latter.
      return std::fprintf(stdout, "none\n") < 0 ? 1 : 0;
    }
    std::fprintf(stderr, "CUDA restore-tid candidate validation failed: %s\n",
                 process_error.c_str());
    return 1;
  }
  if (status != CUDA_SUCCESS) {
    PrintCudaError(status);
    return 1;
  }
  return std::fprintf(stdout, "%d\n", tid) < 0 ? 1 : 0;
}
