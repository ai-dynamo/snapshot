/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "daemon_protocol.h"

#include <barrier>
#ifdef NDEBUG
#undef NDEBUG
#endif
#include <cassert>
#include <cerrno>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <functional>
#include <limits>
#include <signal.h>
#include <sstream>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>

namespace {

using namespace cuda_checkpoint_daemon;

Request TestRequest(Action action) {
  return Request{
      .action = action,
      .backend =
          action == Action::kHealth ? Backend::kUnspecified : Backend::kPosix,
      .pid = 123,
      .transfer_buffer_count =
          action == Action::kCheckpoint || action == Action::kRestore ? 2U : 0U,
      .transfer_chunk_bytes =
          action == Action::kCheckpoint || action == Action::kRestore ? 4096U
                                                                      : 0U,
      .expected_start_time_ticks = 987654,
      .device_map = action == Action::kRestore ? "GPU-a=GPU-b" : "",
      .storage_dir = action == Action::kCheckpoint || action == Action::kRestore
                         ? "/checkpoints/cuda"
                         : "",
      .expected_cgroup = "0::/kubepods/test\n",
      .job_file =
          action == Action::kHealth ? "" : "/host/proc/123/root/tmp/cuda-job",
      .selected_devices =
          action == Action::kCheckpoint || action == Action::kRestore
              ? "GPU-12345678-1234-1234-1234-123456789abc"
              : "",
  };
}

Request LiveProcessRequest(pid_t pid) {
  Request request = TestRequest(Action::kRestore);
  request.pid = static_cast<uint32_t>(pid);
  std::ifstream stat("/proc/" + std::to_string(pid) + "/stat");
  std::string stat_line;
  assert(std::getline(stat, stat_line));
  const size_t closing_paren = stat_line.rfind(')');
  assert(closing_paren != std::string::npos);
  std::istringstream fields(stat_line.substr(closing_paren + 2));
  std::string field;
  for (size_t index = 0; index < 20; ++index) {
    assert(fields >> field);
    if (index == 19) {
      request.expected_start_time_ticks = std::stoull(field);
    }
  }
  std::ifstream cgroup("/proc/" + std::to_string(pid) + "/cgroup");
  std::ostringstream contents;
  contents << cgroup.rdbuf();
  request.expected_cgroup = contents.str();
  assert(!request.expected_cgroup.empty());
  return request;
}

std::vector<unsigned char> ReadGoldenRequest() {
  std::ifstream fixture(
      "cmd/cuda-checkpoint-helper/testdata/daemon_request_v6.hex");
  assert(fixture.good());
  std::string encoded;
  fixture >> encoded;
  assert(!encoded.empty() && encoded.size() % 2 == 0);
  std::vector<unsigned char> bytes;
  bytes.reserve(encoded.size() / 2);
  for (size_t index = 0; index < encoded.size(); index += 2) {
    const std::string byte = encoded.substr(index, 2);
    char *end = nullptr;
    const unsigned long value = std::strtoul(byte.c_str(), &end, 16);
    assert(end == byte.c_str() + 2 && value <= 0xffUL);
    bytes.push_back(static_cast<unsigned char>(value));
  }
  return bytes;
}

void TestGoldenRequestFixture() {
  const std::vector<unsigned char> encoded = ReadGoldenRequest();
  Request parsed;
  std::string error;
  assert(ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
  assert(parsed.action == Action::kRestore);
  assert(parsed.backend == Backend::kPosix);
  assert(parsed.pid == 42U);
  assert(parsed.transfer_buffer_count == 2U);
  assert(parsed.transfer_chunk_bytes == 8U * 1024U * 1024U);
  assert(parsed.expected_start_time_ticks == 12345U);
  assert(parsed.device_map ==
         "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee="
         "GPU-11111111-2222-3333-4444-555555555555");
  assert(parsed.storage_dir == "/checkpoints/process-nspid-42");
  assert(parsed.expected_cgroup == "0::/kubepods/test\n");
  assert(parsed.job_file == "/host/proc/42/root/tmp/cuda-job");
  assert(parsed.selected_devices ==
         "GPU-12345678-1234-1234-1234-123456789abc");
  std::vector<unsigned char> reencoded;
  assert(EncodeRequest(parsed, &reencoded, &error));
  assert(reencoded == encoded);
}

std::string CreateProcRoot(const Request &request) {
  char proc_template[] = "/tmp/cuda-daemon-proc-test-XXXXXX";
  const char *proc_root = mkdtemp(proc_template);
  assert(proc_root != nullptr);
  const std::filesystem::path process_dir =
      std::filesystem::path(proc_root) / std::to_string(request.pid);
  std::filesystem::create_directories(process_dir);
  {
    std::ofstream stat(process_dir / "stat");
    stat << "123 (worker with spaces) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 "
            "17 18 987654 20\n";
    std::ofstream cgroup(process_dir / "cgroup");
    cgroup << request.expected_cgroup;
  }
  return proc_root;
}

void TestProtocol() {
  Request request = TestRequest(Action::kRestore);
  std::vector<unsigned char> encoded;
  std::string error;
  assert(EncodeRequest(request, &encoded, &error));
  Request parsed;
  assert(ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
  assert(
      parsed.pid == request.pid && parsed.storage_dir == request.storage_dir &&
      parsed.backend == Backend::kPosix &&
      parsed.expected_start_time_ticks == request.expected_start_time_ticks &&
      parsed.expected_cgroup == request.expected_cgroup &&
      parsed.job_file == request.job_file &&
      parsed.selected_devices == request.selected_devices);

  encoded[4] = 99;
  assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
  encoded.resize(kMaxRequestSize + 1);
  assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));

  Response response{.cuda_status = 17,
                    .flags = kResponseFatal | kResponseLockNotAcquired,
                    .output = "stdout",
                    .error = "stderr"};
  assert(EncodeResponse(response, &encoded, &error));
  Response parsed_response;
  assert(
      ParseResponse(encoded.data(), encoded.size(), &parsed_response, &error));
  assert(parsed_response.cuda_status == 17 &&
         parsed_response.flags ==
             (kResponseFatal | kResponseLockNotAcquired) &&
         parsed_response.output == "stdout" &&
         parsed_response.error == "stderr");
  encoded[16] = 200;
  assert(
      !ParseResponse(encoded.data(), encoded.size(), &parsed_response, &error));

  for (const Action action : {Action::kLock, Action::kCheckpoint,
                              Action::kRestore, Action::kUnlock}) {
    request = TestRequest(action);
    assert(EncodeRequest(request, &encoded, &error));
    assert(ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
    assert(parsed.action == action);
  }

  request = TestRequest(Action::kCheckpoint);
  request.backend = Backend::kRegular;
  request.transfer_buffer_count = 0;
  request.transfer_chunk_bytes = 0;
  request.storage_dir.clear();
  request.selected_devices.clear();
  assert(EncodeRequest(request, &encoded, &error));
  assert(ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
  assert(parsed.backend == Backend::kRegular && parsed.storage_dir.empty());

  request.job_file = "tmp/cuda-job";
  assert(EncodeRequest(request, &encoded, &error));
  assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));

  request = TestRequest(Action::kCheckpoint);
  request.backend = Backend::kRegular;
  request.transfer_buffer_count = 0;
  request.transfer_chunk_bytes = 0;
  request.storage_dir.clear();
  assert(EncodeRequest(request, &encoded, &error));
  assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));

  request = TestRequest(Action::kRestore);
  request.selected_devices.clear();
  assert(EncodeRequest(request, &encoded, &error));
  assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));

  for (const std::string &invalid_storage : {
           std::string("/checkpoints"), std::string("/checkpoints-other/cuda"),
           std::string("/checkpoints/../etc"), std::string("/tmp/cuda")}) {
    request = TestRequest(Action::kCheckpoint);
    request.storage_dir = invalid_storage;
    assert(EncodeRequest(request, &encoded, &error));
    assert(!ParseRequest(encoded.data(), encoded.size(), &parsed, &error));
  }
}

void TestExecutionIdentityAndFatalControlFlow() {
  for (const Action action : {Action::kLock, Action::kCheckpoint,
                              Action::kRestore, Action::kUnlock}) {
    Request request = TestRequest(action);
    const std::string proc_root = CreateProcRoot(request);
    int executions = 0;
    Response response;
    assert(ExecuteValidated(
        request, proc_root,
        [&executions](const Request &) {
          ++executions;
          return Response{};
        },
        &response));
    assert(executions == 1);
    request.expected_start_time_ticks++;
    assert(ExecuteValidated(
        request, proc_root,
        [&executions](const Request &) {
          ++executions;
          return Response{};
        },
        &response));
    assert(executions == 1);
    if (action == Action::kLock) {
      assert((response.flags & kResponseLockNotAcquired) != 0);
    } else {
      assert((response.flags & kResponseLockNotAcquired) == 0);
    }
    std::filesystem::remove_all(proc_root);
  }

  OperationState operation;
  int completions = 0;
  assert(FinishHandledOperation(
             false, 17,
             [&completions] {
               ++completions;
               return 0;
             },
             &operation) == 17);
  assert(completions == 0 && operation.fatal());
  operation = {};
  assert(FinishHandledOperation(
             true, 17,
             [&completions] {
               ++completions;
               return 0;
             },
             &operation) == 0);
  assert(completions == 1 && !operation.fatal());
  Response fatal{
      .cuda_status = 17,
      .flags = kResponseFatal,
      .output = "",
      .error = "",
  };
  assert(!ResponseAllowsServerContinue(fatal));
}

void TestProcessIdentityStates() {
  Request request = TestRequest(Action::kRestore);
  const std::string proc_root = CreateProcRoot(request);
  std::string error;
  assert(InspectProcessExistence(request.pid, proc_root, &error) ==
         ProcessExistenceState::kExists);
  assert(InspectProcessIdentity(request, proc_root, &error) ==
         ProcessIdentityState::kMatches);

  request.expected_start_time_ticks++;
  assert(InspectProcessIdentity(request, proc_root, &error) ==
         ProcessIdentityState::kExitedOrReused);
  request.expected_start_time_ticks--;

  request.expected_cgroup = "0::/kubepods/moved\n";
  assert(InspectProcessIdentity(request, proc_root, &error) ==
         ProcessIdentityState::kIndeterminate);
  request.expected_cgroup = "0::/kubepods/test\n";

  const std::filesystem::path process_dir =
      std::filesystem::path(proc_root) / std::to_string(request.pid);
  assert(std::filesystem::remove(process_dir / "stat"));
  assert(InspectProcessIdentity(request, proc_root, &error) ==
         ProcessIdentityState::kIndeterminate);

  std::filesystem::remove_all(process_dir);
  assert(InspectProcessExistence(request.pid, proc_root, &error) ==
         ProcessExistenceState::kMissing);
  assert(InspectProcessIdentity(request, proc_root, &error) ==
         ProcessIdentityState::kExitedOrReused);

  const std::filesystem::path invalid_proc_root =
      std::filesystem::path(proc_root) / "symlink-loop";
  std::filesystem::create_symlink("symlink-loop", invalid_proc_root);
  assert(InspectProcessExistence(request.pid, invalid_proc_root, &error) ==
         ProcessExistenceState::kIndeterminate);
  std::filesystem::remove_all(proc_root);
}

void TestHealthStates() {
  OperationHealth regular_operation(std::chrono::seconds(1), "instance-a");
  regular_operation.MarkReady(false);
  HealthSnapshot regular_snapshot = regular_operation.Snapshot();
  assert(regular_snapshot.ready && regular_snapshot.healthy &&
         !regular_snapshot.custom_storage_available);
  Response regular_response = HealthResponse(regular_operation);
  assert(regular_response.cuda_status == 0 &&
         (regular_response.flags & kResponseCapabilityDeferredCUDA) != 0 &&
         (regular_response.flags & kResponseCapabilityCustomStorage) == 0);

  OperationHealth no_operation(std::chrono::seconds(1), "instance-b");
  HealthSnapshot snapshot = no_operation.Snapshot();
  assert(!snapshot.ready && !snapshot.busy && !snapshot.healthy);

  no_operation.MarkReady(true);
  snapshot = no_operation.Snapshot();
  assert(snapshot.ready && !snapshot.busy && snapshot.healthy &&
         snapshot.custom_storage_available);
  Response response = HealthResponse(no_operation);
  assert((response.flags & kResponseCapabilityDeferredCUDA) != 0 &&
         (response.flags & kResponseCapabilityCustomStorage) != 0);
  assert(response.output.find("\"incarnation\":\"instance-b\"") !=
         std::string::npos);

  Response deferred_reap =
      HealthResponseAfterReap(no_operation, 0, "temporary proc read failure");
  assert(deferred_reap.cuda_status == 0 &&
         (deferred_reap.flags & kResponseCapabilityDeferredCUDA) != 0 &&
         deferred_reap.error.find("retained contexts; reaping deferred") !=
             std::string::npos);
  Response failed_reap = HealthResponseAfterReap(no_operation, 17, "");
  assert(failed_reap.cuda_status == 17 &&
         (failed_reap.flags & kResponseFatal) != 0);

  no_operation.Begin(Action::kCheckpoint, 123);
  snapshot = no_operation.Snapshot();
  assert(snapshot.ready && snapshot.busy && snapshot.healthy);
  assert(snapshot.action == Action::kCheckpoint && snapshot.pid == 123);
  std::this_thread::sleep_for(std::chrono::milliseconds(1100));
  snapshot = no_operation.Snapshot();
  assert(snapshot.busy && !snapshot.healthy);
  no_operation.End();
  snapshot = no_operation.Snapshot();
  assert(!snapshot.busy && snapshot.healthy);
}

void TestIncarnationGeneration() {
  std::string first;
  std::string second;
  std::string error;
  assert(GenerateIncarnation(&first, &error));
  assert(GenerateIncarnation(&second, &error));
  assert(first.size() == 36 && second.size() == 36 && first != second);
  assert(first[8] == '-' && first[13] == '-' && first[18] == '-' &&
         first[23] == '-' && first[14] == '4');
}

void TestSocketLifecycle() {
  char socket_template[] = "/tmp/cuda-daemon-socket-test-XXXXXX";
  const char *directory = mkdtemp(socket_template);
  assert(directory != nullptr);
  const std::string path = std::string(directory) + "/helper.sock";
  std::string error;

  OwnedUnixSocket owner;
  assert(owner.Bind(path, 1, &error));
  struct stat owned{};
  assert(lstat(path.c_str(), &owned) == 0);
  OwnedUnixSocket contender;
  assert(!contender.Bind(path, 1, &error));
  struct stat after_contender{};
  assert(lstat(path.c_str(), &after_contender) == 0);
  assert(after_contender.st_ino == owned.st_ino &&
         after_contender.st_dev == owned.st_dev);

  const std::string replacement = std::string(directory) + "/replacement";
  assert(unlink(path.c_str()) == 0);
  {
    std::ofstream file(replacement);
    file << "replacement";
  }
  assert(rename(replacement.c_str(), path.c_str()) == 0);
  owner.Close();
  struct stat replacement_stat{};
  assert(lstat(path.c_str(), &replacement_stat) == 0 &&
         S_ISREG(replacement_stat.st_mode));
  assert(unlink(path.c_str()) == 0);

  const int stale_fd = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0);
  assert(stale_fd >= 0);
  sockaddr_un address{};
  address.sun_family = AF_UNIX;
  std::memcpy(address.sun_path, path.c_str(), path.size() + 1);
  assert(bind(stale_fd, reinterpret_cast<sockaddr *>(&address),
              sizeof(address)) == 0);
  close(stale_fd);
  OwnedUnixSocket stale_replacer;
  assert(stale_replacer.Bind(path, 1, &error));
  stale_replacer.Close();
  assert(lstat(path.c_str(), &replacement_stat) != 0 && errno == ENOENT);

  OwnedUnixSocket setup_failure;
  assert(!setup_failure.Bind(path, -1, &error));
  assert(lstat(path.c_str(), &replacement_stat) != 0 && errno == ENOENT);
  std::filesystem::remove_all(directory);
}

void RunBounded(const std::function<void()> &test) {
  const pid_t child = fork();
  assert(child >= 0);
  if (child == 0) {
    test();
    _exit(0);
  }

  int status = 0;
  pid_t wait_result = 0;
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(5);
  while ((wait_result = waitpid(child, &status, WNOHANG)) == 0 &&
         std::chrono::steady_clock::now() < deadline) {
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
  }
  if (wait_result == 0) {
    assert(kill(child, SIGKILL) == 0);
    assert(waitpid(child, &status, 0) == child);
    assert(false && "bounded shutdown test timed out");
  }
  assert(wait_result == child);
  assert(WIFEXITED(status) && WEXITSTATUS(status) == 0);
}

void RunSignalOwnerScenario(bool external_signal) {
  ShutdownSignalOwner signal_owner;
  std::string error;
  assert(signal_owner.Start(&error));

  int silent_operation_client[2];
  int silent_health_client[2];
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0,
                    silent_operation_client) == 0);
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0,
                    silent_health_client) == 0);
  std::barrier waiters_started(3);
  const auto rendezvous = [&] { waiters_started.arrive_and_wait(); };
  const auto wait_for_stop = [&](int client_fd, int stop_fd) {
    assert(PollForInputOrStop(client_fd, stop_fd, rendezvous) == 0);
    unsigned char wake = 0;
    assert(read(stop_fd, &wake, sizeof(wake)) == sizeof(wake));
  };
  std::thread operation_waiter([&] {
    wait_for_stop(silent_operation_client[0], signal_owner.operation_stop_fd());
  });
  std::thread health_waiter([&] {
    wait_for_stop(silent_health_client[0], signal_owner.health_stop_fd());
  });
  waiters_started.arrive_and_wait();

  if (external_signal) {
    assert(kill(getpid(), SIGTERM) == 0);
    operation_waiter.join();
    health_waiter.join();
    assert(signal_owner.ShutdownRequested());
    assert(signal_owner.StopAndJoin(&error));
  } else {
    assert(signal_owner.StopAndJoin(&error));
    operation_waiter.join();
    health_waiter.join();
    assert(signal_owner.ShutdownRequested());
  }
  signal_owner.Close();
  close(silent_operation_client[0]);
  close(silent_operation_client[1]);
  close(silent_health_client[0]);
  close(silent_health_client[1]);
}

void TestExternalSignalWakesIndependentWaiters() {
  RunBounded([] { RunSignalOwnerScenario(true); });
}

void TestInternalStopJoinsSignalOwnerAndWakesIndependentWaiters() {
  RunBounded([] { RunSignalOwnerScenario(false); });
}

void TestPollDoesNotConsumeStopWake() {
  int stop_pipe[2];
  int silent_client[2];
  assert(pipe2(stop_pipe, O_CLOEXEC | O_NONBLOCK) == 0);
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, silent_client) ==
         0);
  const unsigned char wake = 1;
  assert(write(stop_pipe[1], &wake, sizeof(wake)) == sizeof(wake));
  assert(PollForInputOrStop(silent_client[0], stop_pipe[0]) == 0);
  assert(PollForInputOrStop(silent_client[0], stop_pipe[0]) == 0);
  unsigned char remaining = 0;
  assert(read(stop_pipe[0], &remaining, sizeof(remaining)) ==
         sizeof(remaining));
  close(silent_client[0]);
  close(silent_client[1]);
  close(stop_pipe[0]);
  close(stop_pipe[1]);
}

void TestPollTimesOutWithoutInput() {
  int stop_pipe[2];
  int silent_client[2];
  assert(pipe2(stop_pipe, O_CLOEXEC | O_NONBLOCK) == 0);
  assert(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, silent_client) ==
         0);
  assert(PollForInputOrStop(silent_client[0], stop_pipe[0], {}, 1) == -1);
  close(silent_client[0]);
  close(silent_client[1]);
  close(stop_pipe[0]);
  close(stop_pipe[1]);
}

void TestOperationTimeoutMilliseconds() {
  unsigned int timeout_ms = 0;
  std::string error;
  assert(OperationTimeoutMilliseconds(std::chrono::seconds(3600), &timeout_ms,
                                      &error));
  assert(timeout_ms == 3600000U);
  assert(!OperationTimeoutMilliseconds(std::chrono::seconds(0), &timeout_ms,
                                       &error));
  assert(!OperationTimeoutMilliseconds(
      std::chrono::seconds(
          static_cast<uint64_t>(std::numeric_limits<unsigned int>::max()) /
              1000ULL +
          1ULL),
      &timeout_ms, &error));
}

void TestTerminateMatchingProcesses() {
  assert(!TerminateMatchingProcesses({}, "/proc", std::chrono::seconds(1),
                                     nullptr));
  const pid_t child = fork();
  assert(child >= 0);
  if (child == 0) {
    for (;;) {
      pause();
    }
  }
  const Request request = LiveProcessRequest(child);
  std::thread reaper([child] {
    int status = 0;
    assert(waitpid(child, &status, 0) == child);
    assert(WIFSIGNALED(status));
  });
  std::string error;
  assert(TerminateMatchingProcesses({request}, "/proc",
                                    std::chrono::seconds(2), &error));
  reaper.join();

  Request reused = LiveProcessRequest(getpid());
  ++reused.expected_start_time_ticks;
  assert(TerminateMatchingProcesses({reused}, "/proc",
                                    std::chrono::milliseconds(20), &error));

  const auto proc_root = std::filesystem::temp_directory_path() /
                         ("daemon-termination-test-" +
                          std::to_string(static_cast<unsigned long>(getpid())));
  std::filesystem::remove_all(proc_root);
  std::filesystem::create_directories(proc_root / "42");
  Request indeterminate = TestRequest(Action::kRestore);
  indeterminate.pid = 42;
  assert(!TerminateMatchingProcesses({indeterminate}, proc_root.string(),
                                     std::chrono::milliseconds(20), &error));
  assert(error.find("cannot safely terminate") != std::string::npos);
  std::filesystem::remove_all(proc_root);

  const pid_t first = fork();
  assert(first >= 0);
  if (first == 0) {
    for (;;) {
      pause();
    }
  }
  const pid_t second = fork();
  assert(second >= 0);
  if (second == 0) {
    for (;;) {
      pause();
    }
  }
  Request first_request = LiveProcessRequest(first);
  Request invalid_second = LiveProcessRequest(second);
  invalid_second.expected_cgroup += "moved";
  assert(!TerminateMatchingProcesses({first_request, invalid_second}, "/proc",
                                     std::chrono::milliseconds(20), &error));
  assert(kill(first, 0) == 0);
  assert(kill(first, SIGKILL) == 0);
  assert(kill(second, SIGKILL) == 0);
  int status = 0;
  assert(waitpid(first, &status, 0) == first);
  assert(waitpid(second, &status, 0) == second);

  const pid_t timeout_child = fork();
  assert(timeout_child >= 0);
  if (timeout_child == 0) {
    for (;;) {
      pause();
    }
  }
  const Request timeout_request = LiveProcessRequest(timeout_child);
  const auto fake_proc_root = std::filesystem::temp_directory_path() /
                              ("daemon-termination-timeout-" +
                               std::to_string(static_cast<unsigned long>(getpid())));
  const auto fake_process_dir =
      fake_proc_root / std::to_string(timeout_request.pid);
  std::filesystem::remove_all(fake_proc_root);
  std::filesystem::create_directories(fake_process_dir);
  std::filesystem::copy_file(
      "/proc/" + std::to_string(timeout_child) + "/stat",
      fake_process_dir / "stat");
  std::filesystem::copy_file(
      "/proc/" + std::to_string(timeout_child) + "/cgroup",
      fake_process_dir / "cgroup");
  std::thread timeout_reaper([timeout_child] {
    int child_status = 0;
    assert(waitpid(timeout_child, &child_status, 0) == timeout_child);
  });
  assert(!TerminateMatchingProcesses({timeout_request}, fake_proc_root.string(),
                                     std::chrono::milliseconds(20), &error));
  assert(error.find("timed out") != std::string::npos);
  timeout_reaper.join();
  std::filesystem::remove_all(fake_proc_root);
}

void BoundedOutputCaptureScenario() {
  BoundedOutputCapture capture(8);
  std::string error;
  assert(capture.Start(&error));
  const std::string payload(1024ULL * 1024ULL, 'x');
  size_t offset = 0;
  while (offset < payload.size()) {
    const ssize_t written = write(capture.write_fd(), payload.data() + offset,
                                  payload.size() - offset);
    assert(written > 0);
    offset += static_cast<size_t>(written);
  }
  std::string output;
  bool truncated = false;
  assert(capture.Finish(&output, &truncated, &error));
  assert(output == "xxxxxxxx");
  assert(truncated);
}

void TestBoundedOutputCaptureDrainsAndTruncates() {
  RunBounded([] { BoundedOutputCaptureScenario(); });
}

} // namespace

int main() {
  TestGoldenRequestFixture();
  TestProtocol();
  TestExecutionIdentityAndFatalControlFlow();
  TestProcessIdentityStates();
  TestHealthStates();
  TestIncarnationGeneration();
  TestSocketLifecycle();
  TestExternalSignalWakesIndependentWaiters();
  TestInternalStopJoinsSignalOwnerAndWakesIndependentWaiters();
  TestPollDoesNotConsumeStopWake();
  TestPollTimesOutWithoutInput();
  TestOperationTimeoutMilliseconds();
  TestTerminateMatchingProcesses();
  TestBoundedOutputCaptureDrainsAndTruncates();
  return 0;
}
